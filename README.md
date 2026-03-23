# Koshi Runtime

Koshi Runtime is a workload-scoped runtime governance plane for AI systems. It deploys as a Kubernetes sidecar that observes and enforces deterministic policy at the workload boundary — token budgets, per-request guards, and tiered enforcement decisions — using reservation-first accounting.

**Start with observation, enforce when ready.** Koshi ships in **listener mode** by default: the full enforcement pipeline runs on every request, but no traffic is ever blocked. Shadow decisions (`would_reject`, `would_throttle`, `would_kill`) are emitted as structured events and Prometheus metrics. When you're confident in your policies, switch to enforcement mode — same binary, same config shape, same metrics.

## Quick Start: Kubernetes (Listener Mode)

```bash
# 1. Install into koshi-system
helm install koshi deploy/helm/koshi/ \
  --namespace koshi-system --create-namespace \
  --set image.repository=your-registry/koshi \
  --set image.tag=latest

# 2. Opt namespaces in — only labeled namespaces get the sidecar
kubectl label namespace my-namespace runtime.getkoshi.ai/inject=true

# 3. Restart workloads to pick up the sidecar
kubectl rollout restart deployment -n my-namespace

# 4. Observe shadow events
kubectl logs -n my-namespace deploy/my-app -c koshi-listener --tail=100 | \
  jq 'select(.stream == "event")'

# 5. Check metrics
kubectl port-forward -n my-namespace deploy/my-app 15080:15080
curl http://localhost:15080/metrics | grep koshi_listener
```

## What Gets Installed

| Component | Namespace | Purpose |
|-----------|-----------|---------|
| Injector Deployment | `koshi-system` | Mutating admission webhook — injects sidecar into labeled namespaces |
| MutatingWebhookConfiguration | cluster-scoped | Intercepts pod CREATE in namespaces with `runtime.getkoshi.ai/inject: "true"` |
| ConfigMap | `koshi-system` | Runtime config (mode, upstreams, default policy) |
| TLS Secret | `koshi-system` | Webhook serving certificate (self-signed by default) |
| NetworkPolicy | `koshi-system` | Restricts injector ingress to apiserver, sidecar egress to upstreams |

The sidecar (`koshi-listener`) is injected into workload pods automatically. It:
- Listens on `:15080` (configurable)
- Exposes `/metrics`, `/healthz`, `/readyz`, `/status`
- Receives traffic via `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` env vars injected into app containers

## How It Works

```
Request → Identify workload (pod metadata) → Resolve policy → Extract max_tokens →
  Per-request guard check → Reserve tokens (rolling window) → Tier decision →
  Emit shadow event (listener) or enforce (enforcement) → Proxy to upstream →
  Record actual usage on response
```

### Listener vs Enforcement Mode

| Behavior | Listener | Enforcement |
|----------|----------|-------------|
| Identity failure | Emit `would_reject`, proxy through | Return 403 |
| Budget exceeded | Emit `would_throttle`, proxy through | Return 429 with `Retry-After` |
| Kill decision | Emit `would_kill`, proxy through | Return 503 |
| Metrics | `koshi_listener_*` series | `koshi_enforcement_*` series |
| Default listen addr | `:15080` | `:8080` |

Both modes run the identical enforcement pipeline. Listener mode swallows deny signals and emits shadow events instead.

### Workload Identity

In Kubernetes, identity is derived from pod metadata injected by the webhook at admission time:

| Source | Env Var | Example |
|--------|---------|---------|
| Pod namespace | `KOSHI_POD_NAMESPACE` | `production` |
| Owner kind (normalized) | `KOSHI_WORKLOAD_KIND` | `Deployment` |
| Owner name (normalized) | `KOSHI_WORKLOAD_NAME` | `my-service` |
| Pod name | `KOSHI_POD_NAME` | `my-service-abc123-xyz` |

**Normalization rules:** ReplicaSet owners with `pod-template-hash` are normalized to `Deployment`. StatefulSet, DaemonSet, Job, and CronJob owners are used directly. Pods with no owner resolve as `Pod/<name>`.

In standalone (non-Kubernetes) mode, identity comes from an HTTP header (default: `x-genops-workload-id`).

## Configuration

Config is loaded from `KOSHI_CONFIG_PATH` (default: `/etc/koshi/config.yaml`).

### Listener Mode (recommended starting point)

```yaml
mode:
  type: "listener"

upstreams:
  openai: "https://api.openai.com"
  anthropic: "https://api.anthropic.com"

default_policy:
  id: "_listener_default"
  budgets:
    rolling_tokens:
      window_seconds: 3600
      limit_tokens: 1000000
      burst_tokens: 0
  guards:
    max_tokens_per_request: 32768
  decision_tiers:
    tier1_auto: { action: "throttle" }
```

See [`examples/listener-config.yaml`](examples/listener-config.yaml) for a fully annotated reference.

### Enforcement Mode

```yaml
upstreams:
  openai: "https://api.openai.com"
  anthropic: "https://api.anthropic.com"

workloads:
  - id: "my-agent"
    identity: { mode: "header", key: "x-genops-workload-id" }
    policy_refs: ["standard"]

policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 300
        limit_tokens: 100000
        burst_tokens: 10000
    guards:
      max_tokens_per_request: 4096
    decision_tiers:
      tier1_auto: { action: "throttle" }
      tier3_platform: { action: "kill_workload" }
```

See [`examples/config.yaml`](examples/config.yaml) for a fully annotated reference.

### Settings Reference

| Field | Default | Description |
|-------|---------|-------------|
| `mode.type` | `"enforcement"` | `"listener"` or `"enforcement"` |
| `default_policy` | none | Policy applied to unknown workloads |
| `strict_mode` | `false` | Reject unknown workloads even if `default_policy` is set |
| `sse_extraction` | `true` | Extract actual token usage from SSE streams |
| `listen_addr` | `:8080` (enforcement) / `:15080` (listener) | Server listen address |

## Observability

### Structured Events

Events are emitted as JSON to stdout with `"stream": "event"`. Filter with:

```bash
kubectl logs -c koshi-listener | jq 'select(.stream == "event")'
```

### Listener Metrics

| Metric | Labels | Description |
|--------|--------|-------------|
| `koshi_listener_decisions_total` | `namespace`, `decision_shadow`, `reason_code` | Shadow decision counter |
| `koshi_listener_tokens_total` | `namespace`, `provider`, `phase` | Token reservation/actual counter |
| `koshi_listener_latency_seconds` | (none) | Enforcement pipeline latency histogram |

### Reason Codes

All decisions and error responses include a stable `reason_code`:

| Code | Meaning |
|------|---------|
| `identity_missing` | Could not resolve workload identity and no default policy fallback available |
| `policy_not_found` | Identity resolved but no explicit or default policy available for evaluation |
| `guard_max_tokens` | Request `max_tokens` exceeds the resolved policy's per-request guard |
| `budget_exhausted_throttle` | Resolved policy's rolling window budget exceeded → throttle |
| `budget_exhausted_kill` | Resolved policy's rolling window budget exceeded → kill |
| `upstream_not_configured` | No upstream URL for detected provider |
| `upstream_timeout` | Upstream did not respond in time |
| `system_degraded` | Runtime entered degraded mode |
| `budget_config_error` | Budget tracker misconfiguration |

See [`docs/kubernetes-observability.md`](docs/kubernetes-observability.md) for detailed observability guidance including sample Prometheus queries and event field reference.

### How Shadow Decisions Relate to Policy

Shadow decisions are computed against the policy context available to the sidecar at request time. If listener mode is started with a `default_policy`, requests are evaluated against that policy even when no explicit workload-to-policy mappings are defined. In that case, expect `allow`, `would_throttle`, or `would_kill` outcomes — not `would_reject`. A `would_reject` shadow decision only appears when Koshi cannot resolve a usable policy context for the request.

| Situation | Policy context | Expected shadow outcomes |
|---|---|---|
| Listener with `default_policy` only | `default_policy` | `allow`, `would_throttle`, `would_kill` |
| Listener with explicit workload-to-policy mapping | matched policy | `allow`, `would_throttle`, `would_kill` |
| Listener with policy override annotation | override policy | `allow`, `would_throttle`, `would_kill` |
| Identity missing, no default policy | none | `would_reject` (`identity_missing`) |
| Identity resolved, no matching or default policy | none | `would_reject` (`policy_not_found`) |

## Deployment

### Docker

```bash
make docker
# Produces koshi:latest — distroless nonroot image
```

### Kubernetes (Helm)

```bash
helm install koshi deploy/helm/koshi/ \
  --namespace koshi-system --create-namespace \
  --set image.repository=your-registry/koshi \
  --set image.tag=latest
```

Key Helm values:

| Value | Default | Description |
|-------|---------|-------------|
| `mode` | `listener` | Runtime mode |
| `injector.enabled` | `true` | Deploy the admission webhook |
| `webhook.failurePolicy` | `Ignore` | Webhook down → pods still create without sidecar |
| `sidecar.port` | `15080` | Sidecar listen port |
| `namespaceSelector.matchLabels` | `runtime.getkoshi.ai/inject: "true"` | Which namespaces get injection |
| `networkPolicy.enabled` | `true` | Deploy NetworkPolicy for injector |

### Annotations

| Annotation | Values | Description |
|------------|--------|-------------|
| `runtime.getkoshi.ai/inject` | `"false"` | Opt out a specific pod from injection |
| `runtime.getkoshi.ai/policy` | policy ID | Override the default policy for this pod's sidecar |

## Health Endpoints

| Endpoint | Method | Response |
|----------|--------|----------|
| `/healthz` | GET | 200 OK / 503 degraded |
| `/readyz` | GET | 200 ready / 503 degraded |
| `/status` | GET | Runtime diagnostics and budget state (JSON) |
| `/metrics` | GET | Prometheus metrics |

## Enabling Enforcement

When you're ready to enforce after observing in listener mode:

1. Update the ConfigMap: change `mode.type` from `"listener"` to `"enforcement"`
2. Define explicit workloads with `identity.mode: "header"` and `policy_refs`
3. Restart the sidecar (rolling restart of your workloads)

The binary, image, and Helm chart are unchanged. Only the config changes.

## Architecture

- **One binary, two roles.** `KOSHI_ROLE=injector` starts the admission webhook server. Default starts the proxy.
- **No Kubernetes API calls on the request path.** Pod identity is normalized at admission time by the webhook and read from env vars by the sidecar.
- **Webhook `failurePolicy: Ignore`.** If the injector is down, pods still create — they just don't get the sidecar.
- **Base-URL injection is safe.** `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` are only set on app containers if not already present.
- **Reservation-first accounting.** Tokens are reserved before the request and reconciled with actual usage after the response.
- **Fail open on infrastructure, fail closed on policy.** A panic triggers degraded pass-through mode. An unknown workload gets 403.

## Uninstall / Rollback

```bash
# Remove Koshi entirely
helm uninstall koshi -n koshi-system

# Remove namespace label (stops new pods from getting sidecars)
kubectl label namespace my-namespace runtime.getkoshi.ai/inject-

# Restart workloads to remove existing sidecars
kubectl rollout restart deployment -n my-namespace
```

Existing pods with sidecars continue to function until restarted. Removing the namespace label only affects future pod creation.

## Documentation

For setup, evaluation, contribution, and architectural context:

**Start here**
- [Local demo walkthrough](demo/local/README.md) — kind cluster setup, synthetic traffic, event and metric validation
- [Kubernetes observability guide](docs/kubernetes-observability.md) — structured events, Prometheus queries, Grafana patterns

**Project**
- [Roadmap](ROADMAP.md) — public product direction for the open runtime
- [Contributing](CONTRIBUTING.md) — contribution guide
- [Security](SECURITY.md) — vulnerability reporting and disclosure policy
- [License](LICENSE) — Apache 2.0

**Design**
- [Deterministic Accounting Invariants](docs/design/koshi-v1-deterministic-accounting-invariants.md)
- [Enforcement Boundary](docs/design/koshi-v1-enforcement-boundary.md)
- [Operator Trust Guarantees](docs/design/koshi-v1-operator-trust-guarantees.md)
- [Why Koshi Exists](docs/design/koshi-v1-why-koshi-exists.md)

## GenOps Compatibility

Built against the [GenOps Governance Specification](https://github.com/koshihq/genops-spec). Spec version: `0.1.0`. Exposed at `GET /status` via the `genops_spec_version` field.

## Development

```bash
make build        # Build binary
make test-race    # Run tests with race detector
make lint         # Run linter
make docker       # Build Docker image
```

## Known v1 Limitations

- **In-memory budget state.** Each sidecar maintains its own budget. State is lost on restart.
- **No cross-replica coordination.** Budget enforcement is per-replica.
- **Single policy per workload.** Only the first `policy_ref` is used.
- **Listener mode accounting is policy-scoped per sidecar.** Shadow accounting uses a shared policy key (`_default` or `listener_policy/<id>`) rather than a per-workload tracker key. This keeps accounting bounded, but does not provide cross-replica or cluster-wide budget simulation. Listener events still include namespace, workload kind, and workload name for observability; only the in-memory accounting key is policy-scoped.

# Koshi Runtime

Koshi Runtime is a workload-scoped governance plane for AI systems. It deploys as a Kubernetes sidecar that enforces deterministic policy at the workload boundary — token budgets, per-request guards, and tiered enforcement decisions — using reservation-first accounting.

**Discover your governance posture before enforcing it.** Koshi ships in **listener mode** by default: the full enforcement pipeline — identity resolution, policy lookup, guard evaluation, budget accounting — executes on every request, but no traffic is blocked. Shadow decisions (`would_reject`, `would_throttle`, `would_kill`) reveal exactly where your policies would intervene. When the posture matches your intent, enforcement uses the same binary and Helm chart. For standalone deployments, enabling enforcement is a config change. For injected sidecars, sidecar-level enforcement is planned for a future release — see [Enabling Enforcement](#enabling-enforcement).

**No repo clone required.** Install Koshi directly from the published OCI Helm chart and container image.

## Quick Start: Kubernetes (Listener Mode)

```bash
# 1. Install into koshi-system
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --version 0.2.12 \
  --namespace koshi-system --create-namespace

# 2. Opt namespaces in — only labeled namespaces get the sidecar
kubectl label namespace my-namespace runtime.getkoshi.ai/inject=true

# 3. Restart workloads to pick up the sidecar
kubectl rollout restart deployment -n my-namespace

# 4. Observe shadow events
kubectl logs -n my-namespace deploy/my-app -c koshi-listener --tail=100 | \
  jq 'select(.stream == "event")'

# 5. Check metrics (default sidecar port; adjust if you changed sidecar.port)
kubectl port-forward -n my-namespace deploy/my-app 15080:15080
curl http://localhost:15080/metrics | grep koshi_listener
```

### What You Can Do on Day One

- Install Koshi in listener mode — one Helm command, no repo clone or config files required
- Label any namespace and restart workloads to get sidecars injected
- Collect structured JSON events from `koshi-listener` container logs
- Scrape Prometheus metrics from `/metrics` on the sidecar port (default `15080`, configurable via `sidecar.port`)
- Observe real shadow decisions (`allow`, `would_throttle`, `would_kill`, `would_reject`) on live traffic without blocking anything

### What Traffic Produces Signal

Workloads produce governance signal when they send OpenAI- or Anthropic-compatible API requests through the sidecar. The webhook injects `OPENAI_BASE_URL` and `ANTHROPIC_BASE_URL` env vars pointing at the sidecar (only if the container does not already set them). The sidecar evaluates the request against policy, emits a shadow event, and proxies the request to the real upstream transparently.

**Prerequisites for signal:**
- The workload's SDK or HTTP client must honor `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` env vars. The official OpenAI and Anthropic SDKs do this by default.
- The workload must not already set these env vars in its pod spec — if present, the webhook will not overwrite them.
- The workload must not hardcode provider URLs in application code or config files, bypassing the env vars entirely.

**No signal? Check these first:**
1. Verify the sidecar container exists: `kubectl get pod <pod> -o jsonpath='{.spec.containers[*].name}'` — look for `koshi-listener`
2. Verify the env vars were injected: `kubectl get pod <pod> -o jsonpath='{.spec.containers[0].env[*].name}'` — look for `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL`
3. If the env vars are missing, the workload's pod spec likely already defines them — check the Deployment manifest
4. If the env vars are present but no events appear, the SDK may not be honoring them — check whether the app uses a custom HTTP client or hardcoded base URL

## What Gets Installed

| Component | Namespace | Purpose |
|-----------|-----------|---------|
| Injector Deployment | `koshi-system` | Mutating admission webhook — injects sidecar into labeled namespaces |
| MutatingWebhookConfiguration | cluster-scoped | Intercepts pod CREATE in namespaces with `runtime.getkoshi.ai/inject: "true"` |
| ConfigMap | `koshi-system` | Runtime config for the control-plane deployment (mode, upstreams, default policy). Injected sidecars use the built-in default listener config, not this ConfigMap. |
| TLS Secret | `koshi-system` | Webhook serving certificate (self-signed by default) |
| NetworkPolicy | `koshi-system` | Restricts injector ingress to apiserver, sidecar egress to upstreams |

The sidecar (`koshi-listener`) is injected into workload pods automatically. It:
- Listens on `:15080` by default (configurable via `sidecar.port` Helm value)
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

Both modes execute the same enforcement pipeline. Listener mode surfaces governance posture without affecting traffic; enforcement mode acts on it.

| Behavior | Listener | Enforcement |
|----------|----------|-------------|
| Identity failure | Emit `would_reject`, proxy through | Return 403 |
| Budget exceeded | Emit `would_throttle`, proxy through | Return 429 with `Retry-After` |
| Kill decision | Emit `would_kill`, proxy through | Return 503 |
| Metrics | `koshi_listener_*` series | `koshi_enforcement_*` series |
| Default listen addr | `:15080` | `:8080` |

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

Config is loaded from `KOSHI_CONFIG_PATH` when set. If `KOSHI_CONFIG_PATH` is unset, the runtime uses a built-in default listener config.

**Sidecar config behavior:** The control-plane deployment (main runtime and injector) uses the charted ConfigMap via `KOSHI_CONFIG_PATH`. Injected sidecars in workload pods use the built-in default listener config — no ConfigMap dependency in the workload namespace.

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
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --version 0.2.12 \
  --namespace koshi-system --create-namespace
```

> **Version pinning:** Always pin `--version` in production to avoid unexpected upgrades. The `appVersion` field in the chart metadata determines the default image tag when `image.tag` is unset.

> **Docker Hub mirror:** If you prefer Docker Hub, add `--set image.repository=docker.io/koshihq/koshi-runtime`. The chart and configuration are identical.

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
| `runtime.getkoshi.ai/policy` | policy ID | Per-workload policy override. Not functional for injected sidecars in the current default install path — the built-in default config does not include named policies, so setting this annotation will cause the sidecar to fail at startup. Planned for a future release. |

## Health Endpoints

| Endpoint | Method | Response |
|----------|--------|----------|
| `/healthz` | GET | 200 OK / 503 degraded |
| `/readyz` | GET | 200 ready / 503 degraded |
| `/status` | GET | Runtime diagnostics and budget state (JSON) |
| `/metrics` | GET | Prometheus metrics |

## Deployment Models

Koshi supports two deployment models today. They serve different purposes and have different operational characteristics.

| | Injected sidecar (listener) | Standalone deployment (enforcement) |
|---|---|---|
| **Purpose** | Low-risk governance audit and posture discovery | Centralized runtime — current path to live enforcement |
| **Identity** | Pod metadata (injected by webhook at admission) | HTTP header (`x-genops-workload-id` default) |
| **Config source** | Built-in default listener config | File-based runtime config (`KOSHI_CONFIG_PATH` / ConfigMap) |
| **Policy** | Single built-in default policy | Named policies with per-workload binding |
| **Traffic effect** | None — shadow decisions only, all traffic proxied | Active — 403 / 429 / 503 on policy violations |
| **Blast radius** | Per pod (each sidecar is independent) | Centralized (all routed traffic shares one deployment) |
| **Availability** | Sidecar lifecycle follows the workload pod | Operator-managed; default chart runs a single replica |
| **Metrics** | `koshi_listener_*` series | `koshi_enforcement_*` series |

### Current Adoption Path

1. **Audit** — install Koshi in listener mode, inject sidecars, collect shadow decisions on live traffic. This reveals which workloads generate AI API traffic, what their token patterns look like, and where the default policy boundary sits.

2. **Validate** — use shadow outcomes, identity coverage, and token pressure to finalize policy intent. See [Posture Discovery](#posture-discovery-in-listener-mode) and the [pre-enforcement checklist](docs/kubernetes-observability.md#pre-enforcement-checklist).

3. **Enforce** — operationalize live blocking through a standalone Koshi deployment. This is a **deployment-model handoff**, not an in-place sidecar config flip. See [From Audit to Enforcement](#from-audit-to-enforcement) and the [onboarding guide](docs/onboarding.md#from-listener-audit-to-standalone-enforcement) for concrete steps.

**Why the handoff?** Injected sidecars use the built-in default listener config and do not read the chart ConfigMap. Sidecar config delivery and in-place sidecar enforcement are planned for a future release. Until then, moving from audit to enforcement means deploying Koshi as a standalone service and routing application traffic through it.

### Standalone Availability Considerations

Standalone enforcement introduces a centralized traffic path that differs from sidecar listener mode:

- The default chart runs a single runtime replica. Operators should evaluate replica count, resource allocation, and disruption budget for their availability requirements.
- Scaling replicas improves availability, but does **not** provide globally shared budget state or cross-replica coordination in v1. Each replica maintains independent in-memory accounting.
- Application traffic must be routed to the standalone deployment (e.g., via a Kubernetes Service), and callers must send the configured workload identity header.

These are standard operational concerns — not blockers — but they should be considered as part of the enforcement rollout design.

## From Audit to Enforcement

Enforcement mode uses the same binary and Helm chart but changes how identity is resolved and how decisions are acted on. Moving from sidecar listener audits to standalone enforcement involves translating audit findings into enforcement config:

1. **Observed workloads become explicit `workloads` entries.** Each workload that appeared in your listener audit gets an `id`, `identity.mode: "header"`, and `policy_refs`.
2. **Choose the identity header key.** Standalone enforcement resolves identity from an HTTP header (`HeaderResolver`), not pod metadata. The default key is `x-genops-workload-id`. Your application or API gateway must send this header on each request.
3. **Convert the validated listener baseline into named `policies`.** Shadow outcomes from your audit tell you what budget limits, guards, and tier configurations are appropriate.
4. **Attach `policy_refs` to workloads.** Bind each workload to one or more named policies.
5. **Deploy Koshi as a standalone service** and route application traffic through it.

This is currently a **manual policy operationalization step**, not an automatic migration.

See the [enforcement mode config reference](#enforcement-mode) and the [pre-enforcement checklist](docs/kubernetes-observability.md#pre-enforcement-checklist) before switching.

## Posture Discovery in Listener Mode

Listener mode is designed for discovering your governance posture. Injected sidecars evaluate every AI API request against the built-in default policy and emit shadow decisions. This reveals:

- Which workloads generate AI API traffic and at what volume
- Where the default policy boundary sits relative to real usage patterns
- Whether workload identity is resolving correctly across namespaces

**Interpreting shadow outcomes as coverage signals:**

| Shadow outcome | What it means |
|---|---|
| `allow` | Request passed all checks under the built-in policy |
| `would_throttle` | Built-in budget or guard is tighter than this workload's traffic pattern |
| `would_kill` | Severe budget pressure under the built-in policy |
| `would_reject` + `identity_missing` | Sidecar couldn't resolve workload identity — check webhook injection |
| `would_reject` + `policy_not_found` | No usable policy context — relevant when explicit workload mappings are configured without a default fallback |

**What is not yet supported:** Custom per-workload policy for injected sidecars (custom budgets, guards, or tier configurations). The control-plane deployment supports full policy configuration via the chart ConfigMap, but that config does not propagate to injected sidecars. Sidecar-level policy customization is planned for a future release.

## Architecture

- **One binary, two roles.** `KOSHI_ROLE=injector` starts the admission webhook server. Default starts the proxy.
- **No Kubernetes API calls on the request path.** Pod identity is normalized at admission time by the webhook and read from env vars by the sidecar.
- **Webhook `failurePolicy: Ignore`.** If the injector is down, pods still create — they just don't get the sidecar.
- **Base-URL injection is safe.** `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` are only set on app containers if not already present. See [What Traffic Produces Signal](#what-traffic-produces-signal) for implications when these vars are already defined.
- **Reservation-first accounting.** Tokens are reserved before the request and reconciled with actual usage after the response.
- **Fail open on infrastructure, fail closed on policy.** A panic triggers degraded pass-through mode. In enforcement mode, an unknown workload gets 403. In listener mode, unknown workloads emit `would_reject` and traffic proxies through.

## Uninstall / Rollback

```bash
# Remove Koshi entirely
helm uninstall koshi -n koshi-system

# Remove the auto-generated TLS secret (created by cert-gen hook, not managed by Helm release)
kubectl delete secret koshi-koshi-webhook-tls -n koshi-system 2>/dev/null || true

# Remove namespace label (stops new pods from getting sidecars)
kubectl label namespace my-namespace runtime.getkoshi.ai/inject-

# Restart workloads to remove existing sidecars
kubectl rollout restart deployment -n my-namespace
```

Existing pods with sidecars continue to function until restarted. Removing the namespace label only affects future pod creation.

## Documentation

For setup, evaluation, contribution, and architectural context:

**Start here**
- [Koshi onboarding](docs/onboarding.md) — install, verify, collect signal, interpret shadow outcomes
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

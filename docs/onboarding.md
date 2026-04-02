# Koshi Onboarding

This guide walks through installing Koshi in listener mode, verifying the sidecar is active, collecting governance signal, and interpreting shadow outcomes — all without blocking any traffic.

This flow uses published release artifacts. No local repo checkout is required.

## Install

```bash
# Install Koshi in listener mode (default)
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --version 0.2.12 \
  --namespace koshi-system --create-namespace

# Opt a namespace in for sidecar injection
kubectl label namespace <namespace> runtime.getkoshi.ai/inject=true

# Restart workloads to pick up the sidecar
kubectl rollout restart deployment -n <namespace>
```

> **Version pinning:** Always pin `--version` in production to avoid unexpected upgrades. The `appVersion` field in the chart metadata determines the default image tag when `image.tag` is unset.

> The default image is `ghcr.io/koshihq/koshi-runtime`. To use the Docker Hub mirror, add `--set image.repository=docker.io/koshihq/koshi-runtime`.

## Verify

**Confirm the sidecar is injected:**

```bash
kubectl get pod -n <namespace> -l app=<your-app> \
  -o jsonpath='{.items[0].spec.containers[*].name}'
# Should include "koshi-listener"
```

**Confirm structured events are flowing:**

```bash
kubectl logs -n <namespace> deploy/<your-app> -c koshi-listener --tail=50 | \
  jq 'select(.stream == "event")'
```

**Confirm the metrics endpoint is reachable:**

```bash
# Default sidecar port; adjust if you changed sidecar.port in your Helm values
kubectl port-forward -n <namespace> deploy/<your-app> 15080:15080
curl -s http://localhost:15080/metrics | grep koshi_listener
```

## Troubleshooting: No Events Appearing

If the sidecar is injected but you see no governance events:

1. Verify the sidecar container exists: `kubectl get pod <pod> -o jsonpath='{.spec.containers[*].name}'` — look for `koshi-listener`
2. Verify the env vars were injected into the app container: `kubectl get pod <pod> -o jsonpath='{.spec.containers[0].env[*].name}'` — look for `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL`
3. If the env vars are missing, the workload's pod spec likely already defines them — the webhook will not overwrite existing values. Check the Deployment manifest.
4. If the env vars are present but no events appear, the workload's SDK may not be honoring them — check whether the app uses a custom HTTP client or hardcoded base URL. The official OpenAI and Anthropic SDKs honor these env vars by default.

## Collect

Koshi outputs two signal types. Any observability tool that ingests these formats works — no vendor-specific agent or plugin required.

**Structured events (JSON logs):**
- Source: stdout from container `koshi-listener`
- Filter: `stream == "event"`
- Fields: `namespace`, `workload_kind`, `workload_name`, `provider`, `decision_shadow`, `reason_code`, `estimated_tokens`, `actual_tokens`

**Prometheus metrics:**
- Source: `/metrics` on each sidecar (default port `15080`, configurable via `sidecar.port`)
- Series: `koshi_listener_decisions_total`, `koshi_listener_tokens_total`, `koshi_listener_latency_seconds`
- Labels: `namespace`, `decision_shadow`, `reason_code`, `provider`, `phase`

Works with: Datadog, Splunk, Elastic, CloudWatch, Grafana stack, or any tool that ingests container JSON logs and Prometheus-format metrics.

## Observe and Refine Policy

Listener mode is a policy design sketchpad at the execution boundary. The full enforcement pipeline runs on every request in shadow mode — no traffic is blocked. Use it to observe how policy constructs intersect with real traffic and iteratively refine your intended enforcement posture.

### Shadow decisions as policy feedback

Each shadow outcome maps to a specific policy construct:

| Shadow outcome | Policy construct tested | What to refine |
|---|---|---|
| `allow` | All checks passed | Baseline acceptable for this traffic |
| `would_throttle` + `guard_max_tokens` | `guards.max_tokens_per_request` | Per-request guard tighter than actual request sizes |
| `would_throttle` + `budget_exhausted_throttle` | `budgets.rolling_tokens` | Rolling budget tighter than sustained consumption |
| `would_kill` + `budget_exhausted_kill` | `decision_tiers.tier3_platform` | Severe budget pressure — review consumption or widen budget |
| `would_reject` + `identity_missing` | Identity resolution | Webhook not injecting identity env vars — check injection |
| `would_reject` + `policy_not_found` | Policy lookup | No usable policy context — add default or explicit mapping |

### Refinement workflow

1. **Observe** — collect shadow decisions on live traffic with the built-in default listener policy
2. **Identify pressure points** — which `reason_code` values appear? Which namespaces or workloads show `would_throttle` / `would_kill`?
3. **Refine policy intent** — decide what guard limits, budget windows, and tier actions are appropriate for each workload class
4. **Repeat** — continue until shadow posture matches your intended enforcement posture

This loop is the primary value of listener mode. Shadow decisions are the feedback signal for designing production policy.

## Current Scope

This release supports **posture discovery**, **built-in sidecar policy selection**, and **custom sidecar policy via ConfigMap**: the built-in default policy provides a fixed baseline for listener audit, operators can select from the built-in sidecar policy catalog (`sidecar-baseline`, `sidecar-strict`, `sidecar-high-throughput`) via the `runtime.getkoshi.ai/policy` pod annotation, and operators can deliver arbitrary custom policy via namespace-local ConfigMap using the `runtime.getkoshi.ai/configmap` annotation. All of these work in both listener and enforcement modes.

## Adoption Ladder

After installing Koshi, teams typically progress through these stages:

1. **Listener audit** — install in listener mode, collect shadow decisions on live traffic. No blocking, no risk. Start here.
2. **Built-in enforcement** or **custom ConfigMap sidecar** — choose based on whether the built-in policy presets fit:
   - **Built-in enforcement** (Path A): add `runtime.getkoshi.ai/mode: "enforcement"` and optionally select a preset. Best when standard limits are sufficient.
   - **Custom ConfigMap sidecar** (Path C): deliver operator-authored budgets/guards via ConfigMap. Works in both listener and enforcement modes — shadow-test custom policy before activating blocking.
3. **Standalone enforcement** (Path B) — only if you need centralized budget coordination across workloads, header-based identity, or a shared enforcement point. This is a deployment-model handoff, not a config change.

The ladder is not strictly sequential — after listener audit, choose the path that fits your requirements. Most teams stay on sidecar enforcement (Paths A or C).

## First Saved Searches / First Dashboards

Start with these queries to build your initial governance picture:

- **Top shadow outcomes:** Group events by `decision_shadow` — are you seeing mostly `allow`, or is there `would_throttle` / `would_kill` pressure?
- **Top reason codes:** Group non-`allow` events by `reason_code` — what's driving the shadow decisions?
- **Token burn by namespace:** Sum `koshi_listener_tokens_total{phase="reservation"}` by `namespace` — which namespaces consume the most?
- **Token burn by provider:** Sum `koshi_listener_tokens_total{phase="reservation"}` by `provider` — OpenAI vs Anthropic split

See [Kubernetes Observability Guide](kubernetes-observability.md) for detailed Prometheus queries, Grafana dashboard patterns, and Loki log queries.

## From Listener Audit to Enforcement

Your listener audit produces the raw inputs for enforcement decisions:

- **Workload inventory:** which `namespace` / `workload_kind` / `workload_name` tuples appear in structured events
- **Token pressure:** `koshi_listener_tokens_total` by namespace and provider shows consumption patterns
- **Policy boundary fit:** shadow outcomes (`would_throttle`, `would_kill`) show where the default policy is tighter than real usage
- **Identity coverage:** `would_reject` + `identity_missing` events show where identity injection failed

**Which path?** If the built-in policy presets fit your workload, use Path A — it's the fastest path to enforcement. If you need custom budgets, guards, or tier configurations, use Path C to deliver operator-authored policy via ConfigMap. Both preserve per-pod blast radius with no routing or identity changes. Use Path B only if you need centralized enforcement, header-based identity, or a shared enforcement point.

### Path A: Sidecar enforcement (in-place)

The simplest path. Add pod annotations and enforcement is active on the next pod restart. No routing change, no identity change, no config file.

1. Review your listener shadow outcomes and choose the closest built-in policy:
   - `sidecar-baseline` — 100k tokens/hr, 4096 max/request, tier1 throttle + tier3 kill
   - `sidecar-strict` — 25k tokens/hr, 2048 max/request, tier1 throttle + tier3 kill
   - `sidecar-high-throughput` — 500k tokens/hr, 32768 max/request, tier1 throttle only
2. Add `runtime.getkoshi.ai/mode: "enforcement"` to your pod template annotations
3. Optionally add `runtime.getkoshi.ai/policy: "<policy-id>"` (defaults to `sidecar-baseline`)
4. Restart the workload

**What you get:** live enforcement with per-pod blast radius, pod-derived identity, and built-in policy selection.

**What you don't get:** arbitrary custom policy (custom budgets, guards, tier configs) — for that, use [Path C (sidecar custom config via ConfigMap)](#path-c-sidecar-custom-config-via-configmap). For centralized budget coordination or header-based identity, use standalone enforcement.

**Rollback:** remove the mode annotation and restart the workload — it returns to listener audit mode.

See [`examples/enforcement-sidecar-deployment.yaml`](../examples/enforcement-sidecar-deployment.yaml) for a complete example.

### Path B: Standalone enforcement (deployment handoff)

Standalone enforcement is a **deployment-model handoff** — not a config change. It involves three distinct transitions. Use this path when you need centralized enforcement, explicit per-workload mapping, or header-based identity. (For arbitrary custom policy with per-pod isolation, see [Path C](#path-c-sidecar-custom-config-via-configmap) instead.)

#### Policy: map audit findings into standalone config

- [ ] Map each observed workload into an explicit `workloads` entry with `id`, `identity.mode: "header"`, and `policy_refs`
- [ ] Define named `policies` with `limit_tokens`, `window_seconds`, `max_tokens_per_request`, and tier actions — use shadow outcomes to inform appropriate limits
- [ ] Attach `policy_refs` to each workload entry
- [ ] This is a manual translation — audit results do not automatically become enforcement config

See the [README enforcement mode config reference](../README.md#enforcement-mode) for the full config shape.

#### Identity: switch from pod metadata to header-based resolution

- [ ] Sidecar audits used pod-derived identity (`namespace`, `workload_kind`, `workload_name`). Standalone enforcement uses `HeaderResolver`.
- [ ] Choose a deployment-wide identity header key (default: `x-genops-workload-id`)
- [ ] Ensure application code, SDK wrapper, API gateway, or service mesh sends the identity header on every request
- [ ] In v1, all header-mode workloads share the same identity key — plan your header convention accordingly

#### Traffic: reroute from sidecar-local to standalone runtime

- [ ] Sidecar listener mode redirected traffic locally to `localhost`. Standalone enforcement requires routing through the self-hosted Koshi runtime (in Kubernetes, exposed via a Service — not a third-party hosted endpoint).
- [ ] Point application HTTP clients at the standalone Koshi runtime instead of AI provider APIs
- [ ] For workloads moving to standalone, remove the sidecar injection namespace label and restart workloads

#### Rollout considerations

This handoff is a traffic-path change, not just a config change:

- [ ] All routed traffic flows through a **shared self-hosted Koshi runtime** — test with a narrow subset of workloads before shifting production traffic
- [ ] A misconfiguration in the standalone runtime or its routing affects all workloads routed through it — validate connectivity first
- [ ] Rollback means re-enabling sidecar injection and restarting workloads, not changing a mode flag — plan this path before cutting over
- [ ] **Recommendation:** start small, keep a clear rollback path. The sidecar listener namespace label can be re-applied at any time.

Review the [pre-enforcement checklist](kubernetes-observability.md#pre-enforcement-checklist) before activating.

### Worked example: one audited workload to standalone enforcement

**Listener audit observed:**

```
namespace:        "prod"
workload_kind:    "Deployment"
workload_name:    "payments-api"
provider:         "openai"
decision_shadow:  "would_throttle"
reason_code:      "guard_max_tokens"
```

**Standalone enforcement config:**

```yaml
mode:
  type: "enforcement"

upstreams:
  openai: "https://api.openai.com"

workloads:
  - id: "prod/payments-api"
    type: "service"
    owner_team: "payments"
    environment: "production"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    model_targets:
      - provider: "openai"
        model: "gpt-4"
    policy_refs:
      - "payments-standard"

policies:
  - id: "payments-standard"
    budgets:
      rolling_tokens:
        window_seconds: 300
        limit_tokens: 250000
        burst_tokens: 10000
    guards:
      max_tokens_per_request: 8192
    decision_tiers:
      tier1_auto:
        action: "throttle"
      tier3_platform:
        action: "kill_workload"
```

**Traffic and identity change:**

```bash
# Before: sidecar listener audit (webhook-injected env var)
OPENAI_BASE_URL=http://localhost:15080

# After: standalone enforcement (operator-configured)
OPENAI_BASE_URL=http://koshi-koshi.koshi-system.svc.cluster.local:8080
# Identity header sent on every request:
X-GenOps-Workload-Id: prod/payments-api
```

**What came from the audit vs what the operator chose:**

From audit events:
- [ ] `namespace: "prod"`, `workload_kind: "Deployment"`, `workload_name: "payments-api"`, `provider: "openai"` — observed directly
- [ ] `would_throttle` + `guard_max_tokens` — told the operator that per-request token limits needed attention

Operator decisions (not in audit output):
- [ ] Standalone workload ID convention: `prod/payments-api` — operator choice
- [ ] `type`, `owner_team`, `environment` — organizational metadata, supplied by operator
- [ ] Identity header key: `x-genops-workload-id` (v1 constraint: all header-mode workloads must share the same key)
- [ ] Policy values (`limit_tokens: 250000`, `max_tokens_per_request: 8192`) — informed by audit pressure, not a direct translation from built-in listener defaults

Traffic change:
- [ ] Rerouted from sidecar-local `localhost:15080` to standalone Koshi Service on port 8080
- [ ] Ensured application sends `X-GenOps-Workload-Id` header on every request

### Path C: Sidecar custom config via ConfigMap

Custom config works in both listener and enforcement modes. A sidecar with `configmap` + `policy` annotations and no `mode` annotation runs in listener mode with the custom policy (shadow decisions against custom budgets/guards). Adding `mode: "enforcement"` activates blocking.

1. Create a ConfigMap in the workload namespace with a `config.yaml` data key containing custom policies. See [`examples/sidecar-custom-configmap.yaml`](../examples/sidecar-custom-configmap.yaml).
2. Add annotations to the pod template:
   - `runtime.getkoshi.ai/configmap: "<configmap-name>"` — mounts the namespace-local ConfigMap (**required**)
   - `runtime.getkoshi.ai/policy: "<policy-id>"` — selects which policy from the ConfigMap to use (**required** when configmap is set)
   - `runtime.getkoshi.ai/mode: "enforcement"` — activates blocking (**optional**, defaults to listener)
3. Restart the workload

**ConfigMap contract:**
- The ConfigMap must contain a `config.yaml` data key — the sidecar loads from `/etc/koshi-sidecar/config.yaml`
- Do **not** define `workloads` in the ConfigMap config — the sidecar synthesizes its own workload from pod identity at startup
- `mode.type` in the config file is ignored — mode comes from the annotation only
- Pod restart is required after ConfigMap content changes or annotation changes

**What you get:** arbitrary custom policy (operator-authored budgets, guards, tier configs) with per-pod blast radius and pod-derived identity.

**Rollback:** remove the configmap and policy annotations and restart the workload — it returns to built-in policy behavior.

See [`examples/sidecar-custom-deployment.yaml`](../examples/sidecar-custom-deployment.yaml) for a complete example.

## Next Steps

- Review the [pre-enforcement checklist](kubernetes-observability.md#pre-enforcement-checklist) when you're ready to move from posture discovery to enforcement
- See the [README](../README.md) for full configuration reference and architecture details

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

This release is for **posture discovery**: the built-in default policy provides a fixed baseline that reveals traffic patterns, identity coverage, and policy boundary alignment across your workloads.

Custom per-workload policy for injected sidecars (custom budgets, guards, or tier configurations) is not yet supported. The control-plane deployment supports full policy configuration via the chart ConfigMap, but that config does not propagate to injected sidecars. Sidecar-level policy customization is planned for a future release.

## First Saved Searches / First Dashboards

Start with these queries to build your initial governance picture:

- **Top shadow outcomes:** Group events by `decision_shadow` — are you seeing mostly `allow`, or is there `would_throttle` / `would_kill` pressure?
- **Top reason codes:** Group non-`allow` events by `reason_code` — what's driving the shadow decisions?
- **Token burn by namespace:** Sum `koshi_listener_tokens_total{phase="reservation"}` by `namespace` — which namespaces consume the most?
- **Token burn by provider:** Sum `koshi_listener_tokens_total{phase="reservation"}` by `provider` — OpenAI vs Anthropic split

See [Kubernetes Observability Guide](kubernetes-observability.md) for detailed Prometheus queries, Grafana dashboard patterns, and Loki log queries.

## From Listener Audit to Standalone Enforcement

Moving from sidecar listener audits to live enforcement is a **deployment-model handoff** — not a config change. This section walks through what changes, what you need to build, and what to watch out for.

### What your audit gives you

Your listener audit produces the raw inputs for enforcement policy:

- **Workload inventory:** which `namespace` / `workload_kind` / `workload_name` tuples appear in structured events
- **Token pressure:** `koshi_listener_tokens_total` by namespace and provider shows consumption patterns
- **Policy boundary fit:** shadow outcomes (`would_throttle`, `would_kill`) show where the default policy is tighter than real usage
- **Identity coverage:** `would_reject` + `identity_missing` events show where identity injection failed

### Policy: map audit findings into standalone config

- [ ] Map each observed workload into an explicit `workloads` entry with `id`, `identity.mode: "header"`, and `policy_refs`
- [ ] Define named `policies` with `limit_tokens`, `window_seconds`, `max_tokens_per_request`, and tier actions — use shadow outcomes to inform appropriate limits
- [ ] Attach `policy_refs` to each workload entry
- [ ] This is a manual translation — audit results do not automatically become enforcement config

See the [README enforcement mode config reference](../README.md#enforcement-mode) for the full config shape.

### Identity: switch from pod metadata to header-based resolution

- [ ] Sidecar audits used pod-derived identity (`namespace`, `workload_kind`, `workload_name`). Standalone enforcement uses `HeaderResolver`.
- [ ] Choose a deployment-wide identity header key (default: `x-genops-workload-id`)
- [ ] Ensure application code, SDK wrapper, API gateway, or service mesh sends the identity header on every request
- [ ] In v1, all header-mode workloads share the same identity key — plan your header convention accordingly

### Traffic: reroute from sidecar-local to standalone runtime

- [ ] Sidecar listener mode redirected traffic locally to `localhost`. Standalone enforcement requires routing through the self-hosted Koshi runtime (in Kubernetes, exposed via a Service — not a third-party hosted endpoint).
- [ ] Point application HTTP clients at the standalone Koshi runtime instead of AI provider APIs
- [ ] For workloads moving to standalone, remove the sidecar injection namespace label and restart workloads
- [ ] The path to enforcement is "move traffic from sidecar-local routing to standalone routing" — not "flip the same sidecars to enforcement mode"

### Rollout considerations

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

### Future direction

Planned sidecar config delivery would remove the need for this standalone routing handoff by allowing policy to be delivered to the existing injected sidecar. That capability is not available in the current release.

## Next Steps

- Review the [pre-enforcement checklist](kubernetes-observability.md#pre-enforcement-checklist) when you're ready to move from posture discovery to enforcement
- See the [README](../README.md) for full configuration reference and architecture details

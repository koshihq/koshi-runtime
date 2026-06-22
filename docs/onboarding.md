# Koshi Onboarding

This guide installs Koshi in listener mode, proves the sidecar works end-to-end against a throwaway **canary** namespace, and only then opts a real workload in. Listener mode collects shadow governance decisions on live traffic without blocking any requests.

This flow uses published release artifacts. No local repo checkout is required. It is **canary-first**: install the chart → wait for the injector → validate against a canary → adopt your real workload.

## Prerequisites

- **Helm with OCI support** (Helm ≥ 3.8) — the chart is pulled from an `oci://` registry.
- **Cluster-scoped install permissions** — the chart creates a `MutatingWebhookConfiguration` and cluster RBAC, so a namespace-only role is not sufficient.
- **Image-registry access** — nodes must be able to pull from `ghcr.io/koshihq/...` (or the Docker Hub mirror, see below).
- **Provider egress** (only for the optional real-upstream check) — nodes need outbound HTTPS to `api.openai.com`.

## Install

```bash
# Install Koshi in listener mode (default), waiting for resources to be ready.
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --version 0.2.12 \
  --namespace koshi-system --create-namespace \
  --wait --timeout 120s
```

> **Version pinning:** Always pin `--version` in production to avoid unexpected upgrades. The `appVersion` field in the chart metadata determines the default image tag when `image.tag` is unset.

> The default image is `ghcr.io/koshihq/koshi-runtime`. To use the Docker Hub mirror, add `--set image.repository=docker.io/koshihq/koshi-runtime`.

**Wait for the injector before opting any workload in.** The webhook uses `failurePolicy: Ignore`, so a workload restarted before the injector is ready comes up *without* a sidecar:

```bash
kubectl rollout status -n koshi-system deploy/koshi-koshi-injector --timeout=120s
```

> **Release name:** these commands assume the release is named `koshi`, so the injector Deployment is `koshi-koshi-injector`. Adjust the name if you used a different release name.

## Canary verification

Prove the full path — injection, base-URL rewrite, shadow events, and metrics — against a disposable namespace before touching any real workload.

```bash
CANARY_NS=koshi-eval
CANARY_APP=koshi-canary
```

Create and opt in the canary namespace, then deploy a tiny canary workload:

```bash
kubectl create namespace "$CANARY_NS"
kubectl label namespace "$CANARY_NS" runtime.getkoshi.ai/inject=true

kubectl apply -n "$CANARY_NS" -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $CANARY_APP
  labels:
    app: $CANARY_APP
spec:
  replicas: 1
  selector:
    matchLabels:
      app: $CANARY_APP
  template:
    metadata:
      labels:
        app: $CANARY_APP
    spec:
      containers:
        - name: app
          image: curlimages/curl:8.11.1
          command: ["sleep", "infinity"]
EOF

kubectl rollout status deploy/"$CANARY_APP" -n "$CANARY_NS" --timeout=120s
```

Confirm the sidecar was injected:

```bash
kubectl get pod -n "$CANARY_NS" -l app="$CANARY_APP" \
  -o jsonpath='{.items[0].spec.containers[*].name}'
# Should include "koshi-listener"
```

> If you see only the `app` container, the injector webhook was not yet serving when the pod was admitted (`failurePolicy: Ignore` admits un-injected pods silently). Re-roll and re-check: `kubectl rollout restart deploy/"$CANARY_APP" -n "$CANARY_NS" && kubectl rollout status deploy/"$CANARY_APP" -n "$CANARY_NS"`.

Send one request with a **distinctive `max_tokens`** and assert both the shadow event and a metric delta. Everything runs inside a subshell, so the port-forward cleanup trap is scoped to that subshell and never disturbs your own shell's traps:

```bash
(
  set -e
  kubectl port-forward -n "$CANARY_NS" deploy/"$CANARY_APP" 15080:15080 >/dev/null 2>&1 &
  PF_PID=$!
  trap 'kill "$PF_PID" 2>/dev/null; wait "$PF_PID" 2>/dev/null' EXIT

  # Bounded wait for the port-forward, checking it stays alive.
  for i in $(seq 1 30); do
    kill -0 "$PF_PID" 2>/dev/null || { echo "port-forward died"; exit 1; }
    curl -fsS http://127.0.0.1:15080/metrics >/dev/null 2>&1 && break
    [ "$i" = 30 ] && { echo "FAIL: /metrics never became reachable"; exit 1; }
    sleep 1
  done

  before=$(curl -fsS http://127.0.0.1:15080/metrics \
    | awk '/^koshi_listener_decisions_total/ { s += $NF } END { printf "%d", s+0 }')

  # 4242 is below the default guard (max_tokens_per_request: 32768), so the
  # shadow decision is deterministically "allow".
  kubectl exec -n "$CANARY_NS" deploy/"$CANARY_APP" -c app -- \
    curl -s --connect-timeout 2 --max-time 5 \
      -X POST http://localhost:15080/v1/chat/completions \
      -H "Content-Type: application/json" -H "Host: api.openai.com" \
      -d '{"model":"gpt-4","max_tokens":4242,"messages":[{"role":"user","content":"canary"}]}' \
    >/dev/null 2>&1 || true

  # Poll both signals on one bounded deadline (the emitter is async), with
  # separate PASS/FAIL messages so it is clear which signal failed.
  events_ok=false; metrics_ok=false
  for i in $(seq 1 15); do
    if [ "$events_ok" != true ]; then
      kubectl logs -n "$CANARY_NS" deploy/"$CANARY_APP" -c koshi-listener --tail=50 2>/dev/null \
        | jq -e --arg ns "$CANARY_NS" --arg app "$CANARY_APP" '
            select(.stream=="event"
                   and .event_type=="listener_shadow"
                   and .namespace==$ns and .workload_name==$app
                   and .provider=="openai"
                   and .estimated_tokens==4242
                   and .decision_shadow=="allow")' >/dev/null 2>&1 \
        && events_ok=true
    fi
    if [ "$metrics_ok" != true ]; then
      after=$(curl -fsS http://127.0.0.1:15080/metrics \
        | awk '/^koshi_listener_decisions_total/ { s += $NF } END { printf "%d", s+0 }')
      [ "$((after - before))" -ge 1 ] && metrics_ok=true
    fi
    [ "$events_ok" = true ] && [ "$metrics_ok" = true ] && break
    sleep 1
  done

  [ "$events_ok" = true ]  && echo "PASS: listener_shadow allow event with expected identity + estimated_tokens=4242" \
                           || { echo "FAIL: expected canary shadow event not found"; exit 1; }
  [ "$metrics_ok" = true ] && echo "PASS: koshi_listener_decisions_total increased" \
                           || { echo "FAIL: decisions counter did not increase"; exit 1; }
)
```

The assertion checks **identity and types**, not "every field populated": on an `allow` decision `reason_code` is legitimately empty, `model` is currently emitted empty, and `actual_tokens` is `0`. In listener mode `estimated_tokens` echoes the request's `max_tokens`, so asserting `4242` gives a distinctive, unambiguous match.

### Optional: prove a real upstream call (OpenAI)

The check above proves Koshi emits governance signal — but because the listener emits *before* proxying, it would pass even if the upstream call failed. To prove a successful end-to-end round-trip, make one real OpenAI call.

```bash
OPENAI_MODEL=gpt-4o-mini   # any model your key can call

# Read the key without echoing it or leaving it in shell history.
read -rs -p "OpenAI API key: " OPENAI_API_KEY; echo
kubectl create secret generic koshi-canary-openai -n "$CANARY_NS" \
  --from-literal=OPENAI_API_KEY="$OPENAI_API_KEY"
unset OPENAI_API_KEY

# Inject the key (from the Secret) and the model. This triggers a new rollout,
# so wait for it and run the call against the NEW pod.
kubectl set env deploy/"$CANARY_APP" -n "$CANARY_NS" \
  --from=secret/koshi-canary-openai OPENAI_MODEL="$OPENAI_MODEL"
kubectl rollout status deploy/"$CANARY_APP" -n "$CANARY_NS" --timeout=120s
```

```bash
kubectl exec -n "$CANARY_NS" deploy/"$CANARY_APP" -c app -- sh -c '
  curl -sS -w "\nHTTP %{http_code}\n" \
    -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" -H "Host: api.openai.com" \
    -H "Authorization: Bearer $OPENAI_API_KEY" \
    -d "{\"model\":\"$OPENAI_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"say hi\"}]}"'
# Expect HTTP 2xx and a JSON body whose usage.total_tokens is > 0.
```

Do **not** treat a missing `budget_reconciled` event as a failure — it only fires when actual tokens differ from the reserved tokens, so it is conditional.

### Clean up the canary

```bash
kubectl delete namespace "$CANARY_NS"
# Removes the canary Deployment and the Secret together.
```

## Adopt for a real workload

Once the canary passes, opt your real workload in. Fill in your own values (no angle brackets):

```bash
NS=your-namespace      # replace with your values
APP=your-deployment    # replace with your values

kubectl label namespace "$NS" runtime.getkoshi.ai/inject=true

# Restart only the target Deployment (not every Deployment in the namespace).
kubectl rollout restart deployment/"$APP" -n "$NS"
kubectl rollout status deployment/"$APP" -n "$NS" --timeout=120s
```

Verify it the same way the canary did:

```bash
# Sidecar injected
kubectl get pod -n "$NS" -l app="$APP" \
  -o jsonpath='{.items[0].spec.containers[*].name}'
# Should include "koshi-listener"

# Structured events flowing
kubectl logs -n "$NS" deploy/"$APP" -c koshi-listener --tail=50 | \
  jq 'select(.stream == "event")'

# Metrics endpoint reachable (default sidecar port; adjust if you changed sidecar.port)
kubectl port-forward -n "$NS" deploy/"$APP" 15080:15080 &
curl -s http://localhost:15080/metrics | grep koshi_listener
```

## Troubleshooting: No Events Appearing

If the sidecar is injected but you see no governance events, capture the pod name once and inspect it:

```bash
POD=$(kubectl get pod -n "$NS" -l app="$APP" -o jsonpath='{.items[0].metadata.name}')
```

1. Verify the sidecar container exists: `kubectl get pod -n "$NS" "$POD" -o jsonpath='{.spec.containers[*].name}'` — look for `koshi-listener`
2. Verify the env vars were injected into the app container: `kubectl get pod -n "$NS" "$POD" -o jsonpath='{.spec.containers[0].env[*].name}'` — look for `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL`
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

> **GenOps compatibility:** Koshi emits governance events and metadata conforming to the [GenOps Governance Specification](https://github.com/koshihq/genops-spec). You do not need to learn the GenOps spec to use Koshi — it matters mainly if you are integrating governance telemetry into broader compliance or observability tooling. See the [README GenOps section](../README.md#genops-compatibility) for details.

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

1. **Listener audit** — install in listener mode, collect shadow decisions on live traffic. Listener mode does **not block by policy**, but it is not risk-free: it injects a sidecar container, rewrites `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL`, places Koshi in the live provider request path, and requires restarting the target workload to take effect. It is lower risk than enforcement, not zero risk — validate against a canary namespace first (see [Canary verification](#canary-verification)). Start here.
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
- Use the [agent-assisted evaluation guide](agent-assisted-evaluation.md) to run this onboarding through a coding agent under explicit human approval

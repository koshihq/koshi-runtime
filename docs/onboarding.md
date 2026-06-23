# Koshi Onboarding

This guide installs Koshi in listener mode, validates the sidecar against a throwaway **canary** namespace, and only then opts a real workload in. Listener mode **does not block requests by policy**, but it does place the sidecar in the request path (it rewrites the provider base URL and proxies traffic) — it changes the request path, it does not leave traffic untouched.

This flow uses published release artifacts. No local repo checkout is required. It is **canary-first**: install the chart → wait for the injector → validate against a canary → adopt your real workload.

## Scope variables

Every command in this guide is explicitly scoped — set these once and reuse them so nothing relies on the ambient kube context:

```bash
KUBE_CONTEXT=your-cluster-context   # kubectl config get-contexts
RELEASE=koshi                       # Helm release name (resource names derive from it)
KOSHI_NS=koshi-system               # namespace Koshi installs into

# Confirm you are targeting the intended cluster before mutating anything.
kubectl config get-contexts "$KUBE_CONTEXT"
kubectl --context "$KUBE_CONTEXT" cluster-info | head -1
```

## Prerequisites

- **Helm with OCI support** (Helm ≥ 3.8) — the chart is pulled from an `oci://` registry.
- **Cluster-scoped install permissions** — the chart creates a `MutatingWebhookConfiguration` and cluster RBAC, so a namespace-only role is not sufficient.
- **Image-registry access** — nodes must be able to pull from `ghcr.io/koshihq/...` (or the Docker Hub mirror, see below).
- **Provider egress** (only for the optional real-upstream check) — nodes need outbound HTTPS to `api.openai.com`.

## Install

```bash
# Record whether the namespace already existed (rollback only deletes it if WE created it).
kubectl --context "$KUBE_CONTEXT" get ns "$KOSHI_NS" >/dev/null 2>&1 \
  && KOSHI_NS_PREEXISTING=true || KOSHI_NS_PREEXISTING=false

# Install Koshi in listener mode (default), waiting for resources to be ready.
helm --kube-context "$KUBE_CONTEXT" install "$RELEASE" oci://ghcr.io/koshihq/charts/koshi \
  --version 0.2.12 \
  --namespace "$KOSHI_NS" --create-namespace \
  --wait --timeout 120s
```

> **Version pinning:** Always pin `--version` in production to avoid unexpected upgrades. The `appVersion` field in the chart metadata determines the default image tag when `image.tag` is unset.

> The default image is `ghcr.io/koshihq/koshi-runtime`. To use the Docker Hub mirror, add `--set image.repository=docker.io/koshihq/koshi-runtime`.

> **`KOSHI_NS_PREEXISTING` is set in this shell.** If you run rollback from a different terminal session, re-record it (or leave it unset — rollback preserves the namespace when it is unknown). See [Stop evaluating Koshi](#stop-evaluating-koshi).

**Wait for the injector before opting any workload in.** The webhook uses `failurePolicy: Ignore`, so a workload restarted before the injector is ready comes up *without* a sidecar:

```bash
kubectl --context "$KUBE_CONTEXT" rollout status -n "$KOSHI_NS" \
  deploy/"${RELEASE}-koshi-injector" --timeout=120s
```

> **Release name:** resource names derive from `$RELEASE` (e.g. the injector Deployment is `${RELEASE}-koshi-injector`). With the default `RELEASE=koshi` that is `koshi-koshi-injector`.

## Canary verification

Validate the **listener path** against a disposable namespace before touching any real workload: sidecar **injection**, **base-URL rewrite**, **listener evaluation**, **shadow events**, and **metrics**. This is **not** an end-to-end upstream test — the listener emits *before* proxying, so it would pass even if the upstream call failed. For a real round-trip, see the [optional OpenAI step](#optional-prove-a-real-upstream-call-openai).

```bash
CANARY_NS=koshi-eval
CANARY_APP=koshi-canary
```

Create and opt in the canary namespace, then deploy a tiny canary workload:

```bash
kubectl --context "$KUBE_CONTEXT" create namespace "$CANARY_NS"
kubectl --context "$KUBE_CONTEXT" label namespace "$CANARY_NS" runtime.getkoshi.ai/inject=true

kubectl --context "$KUBE_CONTEXT" apply -n "$CANARY_NS" -f - <<EOF
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

kubectl --context "$KUBE_CONTEXT" rollout status deploy/"$CANARY_APP" -n "$CANARY_NS" --timeout=120s
```

**Pin one concrete pod** and run every check against it (a re-roll/rollout replaces the pod, so re-pin after each):

```bash
POD=$(kubectl --context "$KUBE_CONTEXT" get pod -n "$CANARY_NS" -l app="$CANARY_APP" \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')
echo "Pinned pod: $POD"

# 1. Sidecar injected?
kubectl --context "$KUBE_CONTEXT" get pod -n "$CANARY_NS" "$POD" \
  -o jsonpath='{.spec.containers[*].name}'; echo   # expect to include "koshi-listener"

# 2. Base URL rewritten to point at the sidecar?
kubectl --context "$KUBE_CONTEXT" get pod -n "$CANARY_NS" "$POD" \
  -o jsonpath='{.spec.containers[?(@.name=="app")].env[?(@.name=="OPENAI_BASE_URL")].value}'; echo
# expect: http://localhost:15080
```

> If you see only the `app` container, the injector webhook was not yet serving when the pod was admitted (`failurePolicy: Ignore` admits un-injected pods silently). Re-roll, then **re-pin `$POD`**:
> `kubectl --context "$KUBE_CONTEXT" rollout restart deploy/"$CANARY_APP" -n "$CANARY_NS" && kubectl --context "$KUBE_CONTEXT" rollout status deploy/"$CANARY_APP" -n "$CANARY_NS"`, then re-run the `POD=$(...)` capture above.

Send one request with a **distinctive `max_tokens`** through the **injected** `$OPENAI_BASE_URL` (read inside the pod, so the test exercises the rewrite), then assert the shadow event and a metric delta. All reads are in-pod (no host port-forward), pinned to `$POD`:

```bash
metrics() {
  kubectl --context "$KUBE_CONTEXT" exec -n "$CANARY_NS" "$POD" -c app -- \
    curl -fsS --max-time 5 http://localhost:15080/metrics 2>/dev/null \
    | awk '/^koshi_listener_decisions_total/ { s += $NF } END { printf "%d", s+0 }'
}

before=$(metrics)

# 4242 is below the default guard (max_tokens_per_request: 32768), so the shadow
# decision is deterministically "allow". Send through the injected base URL.
kubectl --context "$KUBE_CONTEXT" exec -n "$CANARY_NS" "$POD" -c app -- sh -c '
  curl -s --connect-timeout 2 --max-time 5 \
    -X POST "$OPENAI_BASE_URL/v1/chat/completions" \
    -H "Content-Type: application/json" -H "Host: api.openai.com" \
    -d "{\"model\":\"gpt-4\",\"max_tokens\":4242,\"messages\":[{\"role\":\"user\",\"content\":\"canary\"}]}"' \
  >/dev/null 2>&1 || true

# Poll both signals on one bounded deadline (the emitter is async), with separate
# PASS/FAIL messages so it is clear which signal failed.
events_ok=false; metrics_ok=false
for i in $(seq 1 15); do
  if [ "$events_ok" != true ]; then
    kubectl --context "$KUBE_CONTEXT" logs -n "$CANARY_NS" "$POD" -c koshi-listener --tail=50 2>/dev/null \
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
    [ "$(( $(metrics) - before ))" -ge 1 ] && metrics_ok=true
  fi
  [ "$events_ok" = true ] && [ "$metrics_ok" = true ] && break
  sleep 1
done

failed=false
[ "$events_ok" = true ]  && echo "PASS: listener_shadow allow event with expected identity + estimated_tokens=4242" \
                         || { echo "FAIL: expected canary shadow event not found"; failed=true; }
[ "$metrics_ok" = true ] && echo "PASS: koshi_listener_decisions_total increased" \
                         || { echo "FAIL: decisions counter did not increase"; failed=true; }
# Sets a non-zero exit status on failure without exiting your shell (safe to paste).
[ "$failed" = false ] && echo "Canary verification PASSED" || echo "Canary verification FAILED (\$? is non-zero)"
[ "$failed" = false ]
```

The assertion checks **identity and types**, not "every field populated": on an `allow` decision `reason_code` is legitimately empty, `model` is currently emitted empty, and `actual_tokens` is `0`. In listener mode `estimated_tokens` echoes the request's `max_tokens`, so asserting `4242` gives a distinctive, unambiguous match.

### Optional: prove a real upstream call (OpenAI)

The check above proves Koshi emits governance signal — but because the listener emits *before* proxying, it would pass even if the upstream call failed. To prove a successful end-to-end round-trip, make one real OpenAI call.

```bash
OPENAI_MODEL=gpt-4o-mini   # any model your key can call

# Enter the key with no echo, and create the Secret WITHOUT putting the key in
# process arguments (visible via `ps`). `printf` is a shell builtin, so the value
# never appears in argv; kubectl only sees /dev/stdin. The human runs this block.
read -rs -p "OpenAI API key: " OPENAI_API_KEY; echo
printf '%s' "$OPENAI_API_KEY" | kubectl --context "$KUBE_CONTEXT" create secret generic \
  koshi-canary-openai -n "$CANARY_NS" --from-file=OPENAI_API_KEY=/dev/stdin
unset OPENAI_API_KEY

# Inject the key (from the Secret) and the model. This triggers a new rollout, so
# wait for it and RE-PIN the pod — the old $POD is replaced.
kubectl --context "$KUBE_CONTEXT" set env deploy/"$CANARY_APP" -n "$CANARY_NS" \
  --from=secret/koshi-canary-openai OPENAI_MODEL="$OPENAI_MODEL"
kubectl --context "$KUBE_CONTEXT" rollout status deploy/"$CANARY_APP" -n "$CANARY_NS" --timeout=120s
POD=$(kubectl --context "$KUBE_CONTEXT" get pod -n "$CANARY_NS" -l app="$CANARY_APP" \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')
```

```bash
# Run against the re-pinned pod, through the injected base URL.
kubectl --context "$KUBE_CONTEXT" exec -n "$CANARY_NS" "$POD" -c app -- sh -c '
  curl -sS -w "\nHTTP %{http_code}\n" \
    -X POST "$OPENAI_BASE_URL/v1/chat/completions" \
    -H "Content-Type: application/json" -H "Host: api.openai.com" \
    -H "Authorization: Bearer $OPENAI_API_KEY" \
    -d "{\"model\":\"$OPENAI_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"say hi\"}]}"'
# Expect HTTP 2xx and a JSON body whose usage.total_tokens is > 0.
```

Do **not** treat a missing `budget_reconciled` event as a failure — it only fires when actual tokens differ from the reserved tokens, so it is conditional.

### Clean up the canary

```bash
kubectl --context "$KUBE_CONTEXT" delete namespace "$CANARY_NS"
# Removes the canary Deployment and the Secret together.
```

## Adopt for a real workload

Once the canary passes, opt your real workload in. Fill in your own values (no angle brackets):

```bash
NS=your-namespace      # replace with your values
APP=your-deployment    # replace with your values
```

> **Namespace-label blast radius.** Labeling a namespace
> `runtime.getkoshi.ai/inject=true` injects the sidecar into **every pod created or
> scaled afterward** in that namespace — not only `$APP`. Prefer a namespace that holds
> just the workloads you intend to evaluate, and account for the others at rollback time.

```bash
kubectl --context "$KUBE_CONTEXT" label namespace "$NS" runtime.getkoshi.ai/inject=true

# Restart only the named target Deployment (never a namespace-wide restart).
kubectl --context "$KUBE_CONTEXT" rollout restart deployment/"$APP" -n "$NS"
kubectl --context "$KUBE_CONTEXT" rollout status deployment/"$APP" -n "$NS" --timeout=120s
```

**Pin one pod for verification.** Real Deployments do not always use `app=<name>` as
their pod selector, so supply an explicit `POD_SELECTOR` rather than assuming it:

```bash
# Show the Deployment's pod selector to help you write POD_SELECTOR. kubectl -l supports
# set-based expressions, so matchExpressions selectors are fine here.
kubectl --context "$KUBE_CONTEXT" get deploy "$APP" -n "$NS" -o jsonpath='{.spec.selector}'; echo

POD_SELECTOR='app.kubernetes.io/name=your-app'   # set to YOUR Deployment's pod labels

# Newest Ready pod matching the selector.
POD=$(kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" -l "$POD_SELECTOR" \
  --field-selector=status.phase=Running --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1:].metadata.name}')

# Validate the pinned pod actually belongs to $APP (pod -> ReplicaSet -> Deployment).
# Use the CONTROLLER owner reference — ownerReferences ordering is not guaranteed.
RS=$(kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" "$POD" -o jsonpath='{.metadata.ownerReferences[?(@.controller==true)].name}')
OWNER=$(kubectl --context "$KUBE_CONTEXT" get rs -n "$NS" "$RS" -o jsonpath='{.metadata.ownerReferences[?(@.controller==true)].name}' 2>/dev/null)
[ "$OWNER" = "$APP" ] || { echo "STOP: $POD is not owned by Deployment $APP (got '$OWNER') — fix POD_SELECTOR"; }
```

Verify against that one pinned pod (same checks the canary ran, in-pod — no port-forward):

```bash
# Sidecar injected + base URL rewritten (read OPENAI_BASE_URL from whichever
# container has it — the app container name is not necessarily "$APP")
kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" "$POD" -o jsonpath='{.spec.containers[*].name}'; echo
kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" "$POD" \
  -o jsonpath='{.spec.containers[*].env[?(@.name=="OPENAI_BASE_URL")].value}'; echo

# Structured events flowing (from the pinned pod's sidecar)
kubectl --context "$KUBE_CONTEXT" logs -n "$NS" "$POD" -c koshi-listener --tail=50 | \
  jq 'select(.stream == "event")'

# Metrics via in-pod curl (use your app container's name if it is not the first container)
kubectl --context "$KUBE_CONTEXT" exec -n "$NS" "$POD" -- \
  curl -fsS --max-time 5 http://localhost:15080/metrics | grep koshi_listener
```

## Troubleshooting: No Events Appearing

If the sidecar is injected but you see no governance events, pin the pod once and inspect it:

```bash
POD=$(kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" -l "$POD_SELECTOR" \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')
```

1. Verify the sidecar container exists: `kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" "$POD" -o jsonpath='{.spec.containers[*].name}'` — look for `koshi-listener`
2. Verify the env vars were injected into the app container: `kubectl --context "$KUBE_CONTEXT" get pod -n "$NS" "$POD" -o jsonpath='{.spec.containers[0].env[*].name}'` — look for `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL`
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

## Stop evaluating Koshi

A full rollback. Which path applies depends on whether you only ran the canary or also adopted a real workload. All commands are context-pinned and use the `$RELEASE`/`$KOSHI_NS` variables from [Scope variables](#scope-variables).

### Before real-workload adoption (only the canary was touched)

```bash
kubectl --context "$KUBE_CONTEXT" delete namespace "$CANARY_NS" --ignore-not-found
```

Then run **Uninstall + leftover cleanup** below.

### After adoption — restart injected workloads first (rollback invariant)

Removing the namespace label stops *future* injection but does not remove sidecars from running pods. Because the label injected the sidecar into **every** pod created or scaled during the evaluation, restarting only `$APP` may leave sidecars in other workloads.

```bash
# Stop future injection.
kubectl --context "$KUBE_CONTEXT" label namespace "$NS" runtime.getkoshi.ai/inject-

# Inventory ALL sidecar-bearing pods and their controllers (any kind, not just Deployments).
# Resolve the CONTROLLER owner reference (ordering is not guaranteed); pods with no
# controller resolve as Pod/<name>.
kubectl --context "$KUBE_CONTEXT" get pods -n "$NS" -o json \
  | jq -r '.items[]
           | select(any(.spec.containers[]; .name=="koshi-listener"))
           | (.metadata.ownerReferences // [] | map(select(.controller==true)) | .[0]) as $o
           | if $o == null then "Pod/\(.metadata.name)" else "\($o.kind)/\($o.name)" end' \
  | sort -u
```

Resolve each owner (a `ReplicaSet/<rs>` belongs to a Deployment — look up its owner) and **report** the list. **Do not restart automatically.** For each workload the operator approves, restart it with the controller-appropriate action:

- **Deployment / StatefulSet / DaemonSet:** `kubectl --context "$KUBE_CONTEXT" rollout restart <kind>/<name> -n "$NS"` then `rollout status`.
- **Job / standalone Pod:** delete and recreate from its source manifest (no `rollout restart`).
- **Unknown or ambiguous ownership:** stop and resolve manually before continuing.

> **Rollback is incomplete** until every injected workload has been restarted **or** you explicitly accept leaving residual Koshi sidecars running. Do not proceed to uninstall otherwise.

### Uninstall + leftover cleanup

```bash
helm --kube-context "$KUBE_CONTEXT" uninstall "$RELEASE" -n "$KOSHI_NS"
```

`helm uninstall` removes the Helm-release-managed resources (the injector Deployment/Service, the `${RELEASE}-koshi-injector` ServiceAccount/ClusterRole/ClusterRoleBinding, and the `${RELEASE}-koshi-injector` MutatingWebhookConfiguration). Two classes are **not** removed and must be deleted explicitly:

- **Cert-gen hook RBAC** `${RELEASE}-koshi-cert-gen` (ServiceAccount/ClusterRole/ClusterRoleBinding) — these are Helm hook resources annotated `hook-delete-policy: before-hook-creation` only (no `hook-succeeded`), so `helm uninstall` leaves them behind.
- **Webhook TLS Secret** `${RELEASE}-koshi-webhook-tls` — in the default flow this is **created at runtime by the cert-gen hook Job** (the chart's `secret.yaml` template renders only when you supply `webhook.certPEM`/`webhook.keyPEM`), so it is hook-created cleanup state, not Helm-release-managed.

Delete them with a separate command per resource type (`kubectl delete` does not switch resource type mid-list):

```bash
kubectl --context "$KUBE_CONTEXT" delete clusterrole \
  "${RELEASE}-koshi-cert-gen" "${RELEASE}-koshi-injector" --ignore-not-found
kubectl --context "$KUBE_CONTEXT" delete clusterrolebinding \
  "${RELEASE}-koshi-cert-gen" "${RELEASE}-koshi-injector" --ignore-not-found
kubectl --context "$KUBE_CONTEXT" delete serviceaccount \
  "${RELEASE}-koshi-cert-gen" -n "$KOSHI_NS" --ignore-not-found
kubectl --context "$KUBE_CONTEXT" delete secret \
  "${RELEASE}-koshi-webhook-tls" -n "$KOSHI_NS" --ignore-not-found
```

> **Known issue (chart `0.2.12`):** the cert-gen hook RBAC and TLS Secret are not cleaned up by `helm uninstall` because the hook resources lack a `hook-succeeded` delete policy. The explicit deletes above are the supported workaround for this release; the chart hook lifecycle is slated to be fixed and tested across install/upgrade/uninstall in a future chart release.

### Delete the namespace (only if the evaluation created it)

```bash
# Fail-safe: only delete $KOSHI_NS if WE created it during install. If
# KOSHI_NS_PREEXISTING is unset (e.g. a different shell session), preserve it.
if [ "${KOSHI_NS_PREEXISTING:-true}" = "false" ]; then
  echo "About to delete namespace $KOSHI_NS (recorded as created by this evaluation)."
  read -rp "Type the namespace name to confirm deletion: " CONFIRM
  [ "$CONFIRM" = "$KOSHI_NS" ] && kubectl --context "$KUBE_CONTEXT" delete namespace "$KOSHI_NS"
else
  echo "Preserving namespace $KOSHI_NS (pre-existing or ownership unknown)."
fi
```

### Verify Koshi is gone (exact names, no fuzzy matching)

```bash
kubectl --context "$KUBE_CONTEXT" get mutatingwebhookconfiguration "${RELEASE}-koshi-injector"   # NotFound
kubectl --context "$KUBE_CONTEXT" get clusterrole "${RELEASE}-koshi-injector" "${RELEASE}-koshi-cert-gen"          # NotFound
kubectl --context "$KUBE_CONTEXT" get clusterrolebinding "${RELEASE}-koshi-injector" "${RELEASE}-koshi-cert-gen"   # NotFound
kubectl --context "$KUBE_CONTEXT" get serviceaccount "${RELEASE}-koshi-cert-gen" -n "$KOSHI_NS"   # NotFound
kubectl --context "$KUBE_CONTEXT" get secret "${RELEASE}-koshi-webhook-tls" -n "$KOSHI_NS"        # NotFound
helm --kube-context "$KUBE_CONTEXT" status "$RELEASE" -n "$KOSHI_NS"                              # release: not found

# No residual sidecars remain in the adopted namespace (empty unless you accepted residuals):
kubectl --context "$KUBE_CONTEXT" get pods -n "$NS" -o json \
  | jq -r '.items[] | select(any(.spec.containers[]; .name=="koshi-listener")) | .metadata.name'
```

## Tested compatibility

This records only what has actually been exercised in this repository. Other environments are **not yet validated** — that means untested, not unsupported.

- **Runtime-tested (local validation of the demo/onboarding flow):** macOS 26.3.1 on Apple Silicon; **Docker CLI 29.4.0** with Docker Desktop (the Docker Engine/Desktop-app versions were not separately recorded); **kind 0.31.0**; **kubectl client 1.34.1**; **Kubernetes server/node 1.35.0** (kind node image `kindest/node:v1.35.0`).
- **Build-configured only (not a tested cluster runtime):** the release workflow builds the runtime image for `linux/amd64` and `linux/arm64`.
- **Not yet validated:** managed distributions (EKS, GKE, AKS), other Kubernetes versions, and restricted Pod Security Standards / admission configurations.

## Next Steps

- Review the [pre-enforcement checklist](kubernetes-observability.md#pre-enforcement-checklist) when you're ready to move from posture discovery to enforcement
- See the [README](../README.md) for full configuration reference and architecture details
- Use the [agent-assisted evaluation guide](agent-assisted-evaluation.md) to run this onboarding through a coding agent under explicit human approval

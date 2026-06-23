#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="koshi-demo"
CONTEXT="kind-${CLUSTER_NAME}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
ORIG_CONTEXT=""

# All cluster commands are pinned to the demo context so they can never act on
# the operator's current/other cluster, regardless of which context is active.
KUBECTL=(kubectl --context "$CONTEXT")
HELM_CTX=(--kube-context "$CONTEXT")

# Restore the operator's original kube context on exit (kind export kubeconfig
# switches it as a side effect). Preserve the script's exit status, and handle
# the case where there was no current context originally.
on_exit() {
    local rc=$?
    if [ -n "$ORIG_CONTEXT" ]; then
        kubectl config use-context "$ORIG_CONTEXT" >/dev/null 2>&1 || true
    else
        kubectl config unset current-context >/dev/null 2>&1 || true
    fi
    exit "$rc"
}
trap on_exit EXIT

# Fail fast with clear messages before doing any work.
preflight() {
    local missing=()
    local tool
    for tool in docker kind helm kubectl jq curl; do
        command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        echo "ERROR: missing required tools: ${missing[*]}" >&2
        exit 1
    fi
    if ! docker info >/dev/null 2>&1; then
        echo "ERROR: Docker daemon is not running. Start Docker and retry." >&2
        exit 1
    fi
    # No host-port check needed: verification reads metrics via in-pod curl, not a
    # host port-forward, so localhost:15080 on the host is irrelevant.
    echo "Preflight OK (kind needs roughly 2 CPU and 2-4 GB free for Docker)."
}

echo "=== Koshi Runtime Local Demo ==="
echo ""
echo "This demo validates listener-sidecar injection, structured events,"
echo "and Prometheus metrics on a local kind cluster."
echo ""

preflight

# Remember the operator's current context before kind mutates it.
ORIG_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"

# 1. Create kind cluster.
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "Cluster ${CLUSTER_NAME} already exists, reusing."
else
  echo "Creating kind cluster: ${CLUSTER_NAME}"
  kind create cluster --name "$CLUSTER_NAME"
fi

# Ensure the kubeconfig entry exists for the demo context (also makes it current,
# which on_exit will revert).
kind export kubeconfig --name "$CLUSTER_NAME" >/dev/null 2>&1

# 2. Build and load image.
echo ""
echo "Building koshi image..."
docker build -t koshi:latest "$REPO_ROOT"
echo "Loading image into kind..."
kind load docker-image koshi:latest --name "$CLUSTER_NAME"

# 3. Install Koshi via Helm.
echo ""
echo "Installing Koshi via Helm..."
helm upgrade --install koshi "$REPO_ROOT/deploy/helm/koshi/" \
  "${HELM_CTX[@]}" \
  --namespace koshi-system --create-namespace \
  -f "$SCRIPT_DIR/values.yaml" \
  --wait --timeout 120s

# On a reused cluster the koshi:latest tag is unchanged, so neither helm upgrade
# nor running pods restart to pick up the freshly built image. Force a restart
# and wait for the rollout so the demo actually tests what was just built.
echo ""
echo "Restarting Koshi control-plane and injector to pick up the rebuilt image..."
"${KUBECTL[@]}" rollout restart deploy/koshi-koshi -n koshi-system
"${KUBECTL[@]}" rollout restart deploy/koshi-koshi-injector -n koshi-system
"${KUBECTL[@]}" rollout status deploy/koshi-koshi -n koshi-system --timeout=120s
"${KUBECTL[@]}" rollout status deploy/koshi-koshi-injector -n koshi-system --timeout=120s

# 4. Label namespace and deploy workload.
echo ""
echo "Labeling default namespace for injection..."
"${KUBECTL[@]}" label namespace default runtime.getkoshi.ai/inject=true --overwrite

echo "Deploying demo workload..."
"${KUBECTL[@]}" apply -f "$SCRIPT_DIR/workload.yaml"

# 5. Restart the workload and verify the sidecar was injected, retrying the
# rollout if needed. `rollout status` on the injector does NOT guarantee its
# mutating-webhook endpoint is already serving, and with failurePolicy: Ignore a
# workload pod created in that gap is admitted WITHOUT a sidecar. The only
# reliable signal is whether the newest pod actually carries the sidecar, so we
# re-roll until it does (bounded).
echo ""
echo "Restarting demo workload so the sidecar runs the rebuilt image..."
injected=false
CONTAINERS=""
for attempt in $(seq 1 6); do
  "${KUBECTL[@]}" rollout restart deploy/demo-workload -n default >/dev/null
  "${KUBECTL[@]}" rollout status deploy/demo-workload -n default --timeout=120s >/dev/null
  CONTAINERS=$("${KUBECTL[@]}" get pod -l app=demo-workload \
    --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{.items[-1:].spec.containers[*].name}' 2>/dev/null || true)
  if echo "${CONTAINERS}" | grep -q "koshi-listener"; then
    injected=true
    break
  fi
  echo "  Sidecar not injected yet (attempt ${attempt}/6); injector webhook may not"
  echo "  have been serving. Re-rolling the workload..."
  sleep 3
done

if [ "$injected" = true ]; then
  # Pin every later read to this exact pod. `kubectl exec/logs deploy/X` can
  # resolve to DIFFERENT pods while old replicas are still terminating, which
  # would split traffic, metric reads, and log reads across pods.
  POD=$("${KUBECTL[@]}" get pod -l app=demo-workload \
    --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{.items[-1:].metadata.name}')
  echo "PASS: Sidecar injected (pod ${POD}, containers: ${CONTAINERS})"
else
  echo "FAIL: Sidecar not injected after retries (containers: ${CONTAINERS})"
  exit 1
fi

# 6. Record baselines BEFORE traffic.
# A reused cluster can already hold old events and a non-zero counter, so
# presence proves nothing — we assert a delta instead. Metrics are read by
# exec-ing curl inside the pod (the app container shares the sidecar's network
# namespace), which is far more robust than a port-forward to a just-restarted
# pod.
echo ""
echo "Recording listener baselines..."

read_decisions_total() {
  # Sum all koshi_listener_decisions_total samples; 0 if unavailable.
  "${KUBECTL[@]}" exec "$POD" -c app -- \
    curl -fsS --max-time 5 http://localhost:15080/metrics 2>/dev/null \
    | awk '/^koshi_listener_decisions_total/ { sum += $NF } END { printf "%d", sum+0 }'
}

count_events() {
  "${KUBECTL[@]}" logs "$POD" -c koshi-listener --tail=500 2>/dev/null \
    | grep -c '"stream":"event"' || true
}

BEFORE_DECISIONS=$(read_decisions_total)
BEFORE_EVENTS=$(count_events)
echo "Baseline: decisions_total=${BEFORE_DECISIONS}, event records=${BEFORE_EVENTS}"

# 7. Send synthetic traffic.
# The listener emits shadow events and metrics before proxying upstream, so
# these requests only need to trigger listener-side evaluation. Timeouts are
# bounded to avoid waiting on upstream auth/connectivity.
echo ""
echo "Sending synthetic requests through the sidecar..."
echo "(Listener events and metrics are emitted before upstream proxying."
echo " Timeouts are bounded — upstream auth or connectivity failures do not"
echo " affect demo success.)"
echo ""
for i in 1 2 3; do
  echo "  Request $i..."
  "${KUBECTL[@]}" exec "$POD" -c app -- \
    curl -s --connect-timeout 2 --max-time 5 \
      -X POST http://localhost:15080/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "Host: api.openai.com" \
      -d "{\"model\":\"gpt-4\",\"max_tokens\":100,\"messages\":[{\"role\":\"user\",\"content\":\"hello $i\"}]}" \
    > /dev/null 2>&1 || true
  sleep 1
done

# 8. Assert deltas (emitter is async and kubectl logs lag, so poll with a
# bounded deadline).
echo ""
echo "=== Listener Signal Deltas ==="
events_ok=false
metrics_ok=false
for attempt in $(seq 1 20); do
  AFTER_EVENTS=$(count_events)
  AFTER_DECISIONS=$(read_decisions_total)
  if [ "$events_ok" != true ] && [ "${AFTER_EVENTS}" -gt "${BEFORE_EVENTS}" ]; then
    events_ok=true
  fi
  if [ "$metrics_ok" != true ] && [ "$((AFTER_DECISIONS - BEFORE_DECISIONS))" -ge 3 ]; then
    metrics_ok=true
  fi
  [ "$events_ok" = true ] && [ "$metrics_ok" = true ] && break
  sleep 1
done

if [ "$events_ok" = true ]; then
  echo "PASS: New structured events emitted (${BEFORE_EVENTS} -> ${AFTER_EVENTS})"
  "${KUBECTL[@]}" logs "$POD" -c koshi-listener --tail=50 2>/dev/null \
    | grep '"stream":"event"' | head -3 | jq -c . 2>/dev/null || true
else
  echo "FAIL: No new structured events after requests (${BEFORE_EVENTS} -> ${AFTER_EVENTS})"
  exit 1
fi

if [ "$metrics_ok" = true ]; then
  echo "PASS: koshi_listener_decisions_total increased by >=3 (${BEFORE_DECISIONS} -> ${AFTER_DECISIONS})"
else
  echo "FAIL: koshi_listener_decisions_total did not grow by 3 (${BEFORE_DECISIONS} -> ${AFTER_DECISIONS})"
  exit 1
fi

echo ""
echo "=== Demo Complete ==="
echo ""
echo "What this demo validated:"
echo "  - Sidecar injection via mutating webhook"
echo "  - Structured governance events (stream: event) emitted by the listener"
echo "  - Prometheus metrics (koshi_listener_*) exposed by the sidecar"
echo ""
echo "Note: Injected sidecars use the built-in default listener config, which"
echo "routes to real AI provider APIs. Upstream responses may fail due to auth"
echo "or connectivity — this does not affect the listener audit signal. Events"
echo "and metrics are emitted regardless of upstream response status."
echo ""
echo "To explore further (commands are pinned to the demo cluster context):"
echo "  kubectl --context ${CONTEXT} logs deploy/demo-workload -c koshi-listener | jq 'select(.stream == \"event\")'"
echo "  kubectl --context ${CONTEXT} port-forward deploy/demo-workload 15080:15080"
echo "  # with the port-forward above running:"
echo "  curl http://localhost:15080/metrics | grep koshi_listener"
echo "  curl http://localhost:15080/status | jq ."
echo ""
if [ "$PWD" = "$SCRIPT_DIR" ]; then
  echo "To clean up: ./teardown.sh"
else
  echo "To clean up: ./demo/local/teardown.sh  (or ./teardown.sh from the demo/local directory)"
fi

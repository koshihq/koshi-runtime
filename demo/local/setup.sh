#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="koshi-demo"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
PORT_FWD_PID=""

cleanup_port_forward() {
    [ -n "${PORT_FWD_PID}" ] && kill "${PORT_FWD_PID}" 2>/dev/null || true
    [ -n "${PORT_FWD_PID}" ] && wait "${PORT_FWD_PID}" 2>/dev/null || true
    PORT_FWD_PID=""
}
trap cleanup_port_forward EXIT

echo "=== Koshi Runtime Local Demo ==="
echo ""
echo "This demo validates listener-sidecar injection, structured events,"
echo "and Prometheus metrics on a local kind cluster."
echo ""

# 1. Create kind cluster.
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "Cluster ${CLUSTER_NAME} already exists, reusing."
else
  echo "Creating kind cluster: ${CLUSTER_NAME}"
  kind create cluster --name "$CLUSTER_NAME"
fi

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
  --namespace koshi-system --create-namespace \
  -f "$SCRIPT_DIR/values.yaml" \
  --wait --timeout 120s

# 4. Label namespace and deploy workload.
echo ""
echo "Labeling default namespace for injection..."
kubectl label namespace default runtime.getkoshi.ai/inject=true --overwrite

echo "Deploying demo workload..."
kubectl apply -f "$SCRIPT_DIR/workload.yaml"

echo "Waiting for demo workload to be ready..."
kubectl wait --for=condition=available deploy/demo-workload -n default --timeout=120s

# 5. Verify sidecar injection.
echo ""
CONTAINERS=$(kubectl get pod -l app=demo-workload -o jsonpath='{.items[0].spec.containers[*].name}')
if echo "${CONTAINERS}" | grep -q "koshi-listener"; then
  echo "PASS: Sidecar injected (containers: ${CONTAINERS})"
else
  echo "FAIL: Sidecar not found in pod containers: ${CONTAINERS}"
  exit 1
fi

# 6. Send synthetic traffic.
# The listener emits shadow events and metrics before proxying upstream,
# so these requests only need to trigger listener-side evaluation.
# Timeouts are bounded to avoid waiting on upstream auth/connectivity.
echo ""
echo "Sending synthetic requests through the sidecar..."
echo "(Listener events and metrics are emitted before upstream proxying."
echo " Timeouts are bounded — upstream auth or connectivity failures do not"
echo " affect demo success.)"
echo ""
for i in 1 2 3; do
  echo "  Request $i..."
  kubectl exec deploy/demo-workload -c app -- \
    curl -s --connect-timeout 2 --max-time 5 \
      -X POST http://localhost:15080/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "Host: api.openai.com" \
      -d "{\"model\":\"gpt-4\",\"max_tokens\":100,\"messages\":[{\"role\":\"user\",\"content\":\"hello $i\"}]}" \
    > /dev/null 2>&1 || true
  sleep 1
done

# 7. Validate structured events.
echo ""
echo "=== Structured Events (stream: event) ==="
EVENTS=$(kubectl logs deploy/demo-workload -c koshi-listener --tail=50 2>/dev/null | \
  grep '"stream":"event"' || true)
if [ -n "${EVENTS}" ]; then
  EVENT_COUNT=$(echo "${EVENTS}" | wc -l | tr -d ' ')
  echo "PASS: Found ${EVENT_COUNT} structured events"
  echo "${EVENTS}" | head -3 | jq -c . 2>/dev/null || echo "${EVENTS}" | head -3
else
  echo "FAIL: No structured events found in sidecar logs"
  exit 1
fi

# 8. Validate metrics via port-forward.
echo ""
echo "=== Listener Metrics ==="
kubectl port-forward deploy/demo-workload 15080:15080 >/dev/null 2>&1 &
PORT_FWD_PID=$!

METRICS=""
for attempt in $(seq 1 10); do
  METRICS=$(curl -fsS http://127.0.0.1:15080/metrics 2>/dev/null) && break
  METRICS=""
  sleep 1
done

cleanup_port_forward

if echo "${METRICS}" | grep -q "koshi_listener_decisions_total"; then
  echo "PASS: Listener metrics present"
  echo "${METRICS}" | grep "^koshi_listener" | head -5
else
  echo "FAIL: koshi_listener_decisions_total not found in metrics output"
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
echo "To explore further:"
echo "  kubectl logs deploy/demo-workload -c koshi-listener | jq 'select(.stream == \"event\")'"
echo "  kubectl port-forward deploy/demo-workload 15080:15080"
echo "  curl http://localhost:15080/metrics | grep koshi_listener"
echo "  curl http://localhost:15080/status | jq ."
echo ""
echo "To clean up: ./teardown.sh"

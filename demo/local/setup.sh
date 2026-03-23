#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="koshi-demo"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "=== Koshi Runtime Local Demo ==="
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

# 4. Deploy mock upstream.
echo ""
echo "Deploying mock upstream..."
kubectl apply -f "$SCRIPT_DIR/mock-upstream.yaml"
kubectl wait --for=condition=available deploy/mock-upstream -n default --timeout=120s

# 5. Label namespace and deploy workload.
echo ""
echo "Labeling default namespace for injection..."
kubectl label namespace default koshi.io/inject=true --overwrite

echo "Deploying demo workload..."
kubectl apply -f "$SCRIPT_DIR/workload.yaml"

echo "Waiting for demo workload to be ready..."
kubectl wait --for=condition=available deploy/demo-workload -n default --timeout=120s

# 6. Send synthetic traffic.
echo ""
echo "Sending synthetic requests..."
for i in 1 2 3; do
  echo "  Request $i..."
  kubectl exec deploy/demo-workload -c app -- \
    curl -s -X POST http://localhost:15080/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "Host: api.openai.com" \
      -d "{\"model\":\"gpt-4\",\"max_tokens\":100,\"messages\":[{\"role\":\"user\",\"content\":\"hello $i\"}]}" \
    || echo "  (request $i failed — sidecar may not be injected yet)"
  sleep 1
done

# 7. Validate events.
echo ""
echo "=== Structured Events (stream: event) ==="
kubectl logs deploy/demo-workload -c koshi-listener --tail=20 2>/dev/null | \
  jq -c 'select(.stream == "event")' 2>/dev/null || \
  echo "(no koshi-listener container found — webhook may not have injected the sidecar)"

# 8. Validate metrics.
echo ""
echo "=== Listener Metrics ==="
kubectl exec deploy/demo-workload -c koshi-listener -- \
  wget -qO- http://localhost:15080/metrics 2>/dev/null | \
  grep "^koshi_listener" || \
  echo "(no listener metrics found)"

echo ""
echo "=== Demo Complete ==="
echo "To explore further:"
echo "  kubectl logs deploy/demo-workload -c koshi-listener | jq 'select(.stream == \"event\")'"
echo "  kubectl port-forward deploy/demo-workload 15080:15080"
echo "  curl http://localhost:15080/metrics | grep koshi_listener"
echo "  curl http://localhost:15080/status | jq ."
echo ""
echo "To clean up: ./teardown.sh"

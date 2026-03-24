#!/usr/bin/env bash
set -euo pipefail

# Golden install validation for Koshi Runtime releases.
# Validates that published chart + image work end-to-end on a clean kind cluster.
#
# Usage:
#   ./scripts/validate-release.sh <chart-version> <image-tag> [image-repo]
#
# Arguments:
#   chart-version  Helm chart version (e.g. 1.0.0)
#   image-tag      Image tag (e.g. v1.0.0)
#   image-repo     Image repository (default: ghcr.io/koshihq/koshi-runtime)
#
# Examples:
#   ./scripts/validate-release.sh 1.0.0 v1.0.0
#   ./scripts/validate-release.sh 1.0.0 v1.0.0 docker.io/koshihq/koshi-runtime

CHART_VERSION="${1:?Usage: validate-release.sh <chart-version> <image-tag> [image-repo]}"
IMAGE_TAG="${2:?Usage: validate-release.sh <chart-version> <image-tag> [image-repo]}"
IMAGE_REPO="${3:-ghcr.io/koshihq/koshi-runtime}"

CLUSTER_NAME="koshi-release-test"
NAMESPACE="koshi-system"
DEMO_DIR="$(cd "$(dirname "$0")/../demo/local" && pwd)"

info()  { echo "==> $*"; }
fail()  { echo "FAIL: $*" >&2; exit 1; }
pass()  { echo "PASS: $*"; }

cleanup() {
    info "Cleaning up..."
    kubectl delete -f "${DEMO_DIR}/workload.yaml" --ignore-not-found 2>/dev/null || true
    kubectl delete -f "${DEMO_DIR}/mock-upstream.yaml" --ignore-not-found 2>/dev/null || true
    kubectl label namespace default runtime.getkoshi.ai/inject- 2>/dev/null || true
    helm uninstall koshi -n "${NAMESPACE}" 2>/dev/null || true
    kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
}

trap cleanup EXIT

# --- 1. Create kind cluster ---
info "Creating kind cluster: ${CLUSTER_NAME}"
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    info "Cluster already exists, reusing"
else
    kind create cluster --name "${CLUSTER_NAME}"
fi

info "Waiting for cluster nodes to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=120s

# --- 2. Install Koshi from OCI chart ---
info "Installing Koshi chart ${CHART_VERSION} with image ${IMAGE_REPO}:${IMAGE_TAG}"
helm install koshi "oci://ghcr.io/koshihq/charts/koshi" \
    --version "${CHART_VERSION}" \
    --namespace "${NAMESPACE}" --create-namespace \
    --set "image.repository=${IMAGE_REPO}" \
    --set "image.tag=${IMAGE_TAG}" \
    --wait --timeout 10m

# --- 3. Verify injector is ready ---
info "Waiting for injector deployment..."
kubectl rollout status deployment -n "${NAMESPACE}" -l app=koshi-injector --timeout=60s 2>/dev/null || \
    kubectl wait --for=condition=available deployment -n "${NAMESPACE}" -l app=koshi-injector --timeout=60s

# --- 4. Deploy mock upstream and demo workload ---
info "Deploying mock upstream and demo workload..."
kubectl apply -f "${DEMO_DIR}/mock-upstream.yaml"
kubectl label namespace default runtime.getkoshi.ai/inject=true --overwrite
kubectl apply -f "${DEMO_DIR}/workload.yaml"

info "Waiting for demo workload..."
kubectl wait --for=condition=ready pod -l app=demo-workload -n default --timeout=120s

# --- 5. Verify sidecar injection ---
CONTAINERS=$(kubectl get pod -l app=demo-workload -o jsonpath='{.items[0].spec.containers[*].name}')
if echo "${CONTAINERS}" | grep -q "koshi-listener"; then
    pass "Sidecar injected (containers: ${CONTAINERS})"
else
    fail "Sidecar not found in pod containers: ${CONTAINERS}"
fi

# --- 6. Send synthetic traffic ---
info "Sending synthetic requests..."
for i in 1 2 3; do
    kubectl exec deploy/demo-workload -c app -- \
        curl -s -X POST http://localhost:15080/v1/chat/completions \
        -H "Content-Type: application/json" \
        -H "Host: api.openai.com" \
        -d '{"model":"gpt-4","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}' \
        > /dev/null
done
sleep 2

# --- 7. Validate structured events ---
info "Checking structured events..."
EVENTS=$(kubectl logs deploy/demo-workload -c koshi-listener --tail=100 2>/dev/null | \
    grep '"stream":"event"' || true)
if [ -n "${EVENTS}" ]; then
    EVENT_COUNT=$(echo "${EVENTS}" | wc -l | tr -d ' ')
    pass "Found ${EVENT_COUNT} structured events"
else
    fail "No structured events found in sidecar logs"
fi

# --- 8. Validate metrics ---
info "Checking metrics endpoint..."
METRICS=$(kubectl exec deploy/demo-workload -c koshi-listener -- \
    wget -qO- http://localhost:15080/metrics 2>/dev/null || true)
if echo "${METRICS}" | grep -q "koshi_listener_decisions_total"; then
    pass "Listener metrics present"
else
    fail "koshi_listener_decisions_total not found in metrics output"
fi

echo ""
info "All validations passed for ${IMAGE_REPO}:${IMAGE_TAG} (chart ${CHART_VERSION})"
echo ""
echo "To validate with Docker Hub mirror:"
echo "  $0 ${CHART_VERSION} ${IMAGE_TAG} docker.io/koshihq/koshi-runtime"

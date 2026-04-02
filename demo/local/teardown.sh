#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="koshi-demo"

echo "=== Tearing down Koshi demo ==="

# Remove workloads.
kubectl delete -f "$(dirname "$0")/workload.yaml" --ignore-not-found 2>/dev/null || true
# Defensive: clean up mock-upstream if it was deployed manually.
kubectl delete -f "$(dirname "$0")/mock-upstream.yaml" --ignore-not-found 2>/dev/null || true

# Remove namespace label.
kubectl label namespace default runtime.getkoshi.ai/inject- 2>/dev/null || true

# Uninstall Helm release.
helm uninstall koshi -n koshi-system 2>/dev/null || true
kubectl delete namespace koshi-system --ignore-not-found 2>/dev/null || true

# Delete kind cluster.
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "Deleting kind cluster: ${CLUSTER_NAME}"
  kind delete cluster --name "$CLUSTER_NAME"
else
  echo "Cluster ${CLUSTER_NAME} not found, nothing to delete."
fi

echo "=== Teardown complete ==="

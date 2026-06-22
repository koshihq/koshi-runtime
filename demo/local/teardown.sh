#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="koshi-demo"

echo "=== Tearing down Koshi demo ==="

# Deleting the named kind cluster removes every namespace, Helm release, and
# label with it. We intentionally run no kubectl/helm cleanup here: those would
# act on the current context and could touch the operator's other clusters.
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "Deleting kind cluster: ${CLUSTER_NAME}"
  kind delete cluster --name "$CLUSTER_NAME"
else
  echo "Cluster ${CLUSTER_NAME} not found, nothing to delete."
fi

echo "=== Teardown complete ==="

# Local Cluster Demo

This demo creates a local kind cluster, installs Koshi in listener mode, injects a sidecar into a sample workload, sends synthetic traffic, and validates that structured events and Prometheus metrics are emitted by the listener sidecar.

> **This demo is for local development and validation only.** It requires a repo checkout, a local Docker build, and a kind cluster. To install Koshi on a real cluster using published artifacts, see [Koshi Onboarding](../../docs/onboarding.md).

## What This Demo Validates

- Sidecar injection via the mutating admission webhook
- Structured governance events (`stream: "event"`) emitted by the listener
- Prometheus metrics (`koshi_listener_*`) exposed by the sidecar

## What This Demo Does Not Validate

- Real upstream AI provider responses — the sidecar routes to `api.openai.com` by default. Upstream responses may fail due to auth or connectivity. Demo success is based on listener events and metrics, not upstream response status.
- Enforcement mode, custom policy, or standalone deployment — this demo is listener-only.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [Helm](https://helm.sh/docs/intro/install/)
- [jq](https://jqlang.github.io/jq/download/)

## Quick Start

```bash
# Run the full demo (creates cluster, installs, sends traffic, validates)
./setup.sh

# Clean up when done
./teardown.sh
```

## Step-by-Step Walkthrough

### 1. Create the cluster and build the image

```bash
kind create cluster --name koshi-demo
docker build -t koshi:latest ../../
kind load docker-image koshi:latest --name koshi-demo
```

### 2. Install Koshi

```bash
helm install koshi ../../deploy/helm/koshi/ \
  --namespace koshi-system --create-namespace \
  -f values.yaml
```

### 3. Label the namespace and deploy a workload

```bash
kubectl label namespace default runtime.getkoshi.ai/inject=true
kubectl apply -f workload.yaml
kubectl wait --for=condition=available deploy/demo-workload -n default --timeout=120s
```

### 4. Verify sidecar injection

```bash
kubectl get pod -l app=demo-workload -o jsonpath='{.items[0].spec.containers[*].name}'
# Should include "koshi-listener"
```

### 5. Send synthetic traffic

```bash
kubectl exec deploy/demo-workload -c app -- \
  curl -s -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    -d '{"model":"gpt-4","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}'
```

The sidecar routes to `api.openai.com` by default. Upstream responses may fail due to auth or connectivity — the listener emits structured events and metrics before upstream proxying, so failures do not affect demo validation. The setup script bounds request time (`--connect-timeout 2 --max-time 5`) for the same reason.

### 6. Validate structured events

```bash
kubectl logs deploy/demo-workload -c koshi-listener --tail=50 | \
  jq 'select(.stream == "event")'
```

You should see events with `decision_shadow` and fields like `namespace`, `workload_kind`, `provider`.

### 7. Check metrics

```bash
kubectl port-forward deploy/demo-workload 15080:15080 &
curl -s http://localhost:15080/metrics | grep koshi_listener
```

Expected metrics:
- `koshi_listener_decisions_total` — shadow decision counts
- `koshi_listener_tokens_total` — token reservation/actual counts
- `koshi_listener_latency_seconds` — enforcement pipeline latency

### 8. Check the status endpoint

```bash
curl -s http://localhost:15080/status | jq .
```

## Notes

`mock-upstream.yaml` is retained in this directory but is not part of the default demo flow. Injected sidecars use `DefaultListenerConfig()`, which routes to real AI provider APIs — the mock upstream is not reachable without overriding sidecar upstream routing, which is not supported for injected sidecars in the current release.

## Using Released Artifacts

To use published artifacts on a real cluster with real AI API providers:

```bash
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --version 0.2.12 \
  --namespace koshi-system --create-namespace
```

For onboarding on a real cluster, see [Koshi Onboarding](../../docs/onboarding.md).

## Cleanup

```bash
./teardown.sh
```

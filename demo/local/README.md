# Local Cluster Demo

This demo creates a local Kubernetes cluster with kind, installs Koshi in listener mode, deploys a mock upstream and sample workload, sends synthetic traffic, and validates structured events and metrics.

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

### 3. Deploy the mock upstream

The mock upstream returns OpenAI-shaped responses with token usage data:

```bash
kubectl apply -f mock-upstream.yaml
```

### 4. Label the namespace and deploy a workload

```bash
kubectl label namespace default runtime.getkoshi.ai/inject=true
kubectl apply -f workload.yaml
```

### 5. Wait for pods and send traffic

```bash
kubectl wait --for=condition=ready pod -l app=demo-workload -n default --timeout=120s

# Send a few requests through the sidecar
kubectl exec deploy/demo-workload -c app -- \
  curl -s -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    -d '{"model":"gpt-4","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}'
```

### 6. Validate structured events

```bash
kubectl logs deploy/demo-workload -c koshi-listener --tail=50 | \
  jq 'select(.stream == "event")'
```

You should see events with `decision_shadow: "allow"` and shadow fields like `namespace`, `workload_kind`, `provider`.

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

## What to Look For

- **Events with `"stream": "event"`** — these are the structured governance events
- **`decision_shadow: "allow"`** — traffic is flowing and passing all checks
- **`koshi_listener_decisions_total`** — Prometheus counter with namespace and shadow decision labels
- **No 403/429/503 responses** — listener mode never blocks traffic

## Using Released Artifacts

Instead of building locally, you can use the published image and chart:

```bash
kind create cluster --name koshi-demo

# Install from OCI chart with GHCR image (chart version = X.Y.Z, image tag = vX.Y.Z)
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --version 1.0.0 \
  --namespace koshi-system --create-namespace \
  -f values-released.yaml

# Then follow steps 3–8 above
```

Create a `values-released.yaml` that overrides the mock upstream URL but uses the default published image:

```yaml
mode: listener
config:
  upstreams:
    openai: http://mock-upstream.default.svc.cluster.local:8080
    anthropic: http://mock-upstream.default.svc.cluster.local:8080
```

## Cleanup

```bash
./teardown.sh
```

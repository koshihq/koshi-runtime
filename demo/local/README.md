# Local Cluster Demo

This demo creates a local kind cluster, installs Koshi in listener mode, injects a sidecar into a sample workload, sends synthetic traffic, and validates that structured events and Prometheus metrics are emitted by the listener sidecar.

> **This demo is for local development and validation only.** It requires a repo checkout, a local Docker build, and a kind cluster. To install Koshi on a real cluster using published artifacts, see [Koshi Onboarding](../../docs/onboarding.md).

## What This Demo Validates

- Sidecar injection via the mutating admission webhook
- Structured governance events (`stream: "event"`) emitted by the listener
- Prometheus metrics (`koshi_listener_*`) exposed by the sidecar

## What This Demo Does Not Validate

- Real upstream AI provider responses — the sidecar routes to `api.openai.com` by default. Upstream responses may fail due to auth or connectivity. Demo success is based on listener events and metrics, not upstream response status.
- Real enforcement blocking or standalone deployment. Optional extensions below demonstrate sidecar enforcement and custom ConfigMap policy as manual walkthroughs.

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

The base demo proves listener audit: injection, events, and metrics. The next two optional extensions show how the same sidecar path moves from shadow-only audit to live enforcement — same workload, same sidecar, only annotations change.

## Optional Extension 1: Built-in Sidecar Enforcement

**When to use:** prove that the same workload can move from listener audit to live enforcement with a single annotation change.

This extension patches the demo workload in place. The key proof: the same over-guard request that produced a shadow `would_throttle` event in listener mode returns a `429` in enforcement mode.

### 1. Select the strict policy in listener mode

First, apply the `sidecar-strict` policy while staying in listener mode. This lets you see shadow decisions against the strict guard (2048 max_tokens) before turning on enforcement:

```bash
kubectl patch deploy demo-workload -n default -p \
  '{"spec":{"template":{"metadata":{"annotations":{"runtime.getkoshi.ai/policy":"sidecar-strict"}}}}}'
kubectl wait --for=condition=available deploy/demo-workload -n default --timeout=120s
```

### 2. Establish the listener baseline — send an over-guard request

The `sidecar-strict` policy has a 2048 max_tokens guard. Send a request that exceeds it:

```bash
kubectl exec deploy/demo-workload -c app -- \
  curl -s --connect-timeout 2 --max-time 5 \
    -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    -d '{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}'
```

The request proxies through (listener mode never blocks). Check the shadow event:

```bash
kubectl logs deploy/demo-workload -c koshi-listener --tail=5 | \
  jq 'select(.stream == "event") | {decision_shadow, reason_code}'
```

You should see `decision_shadow: "would_throttle"` with `reason_code: "guard_max_tokens"` — the listener flagged it, but traffic flowed.

### 3. Flip to enforcement mode

Add the enforcement annotation — same policy, now blocking:

```bash
kubectl patch deploy demo-workload -n default -p \
  '{"spec":{"template":{"metadata":{"annotations":{"runtime.getkoshi.ai/mode":"enforcement"}}}}}'
kubectl wait --for=condition=available deploy/demo-workload -n default --timeout=120s
```

### 4. Resend the same over-guard request

```bash
kubectl exec deploy/demo-workload -c app -- \
  curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    -d '{"model":"gpt-4","max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}'
# Expected: 429 (throttled — guard exceeded)
```

Same request, different mode, different outcome. The sidecar now blocks.

### 5. Confirm a within-limits request still passes

```bash
kubectl exec deploy/demo-workload -c app -- \
  curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    --connect-timeout 2 --max-time 5 \
    -d '{"model":"gpt-4","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}'
# Expected: 200 (or upstream error — the sidecar allowed the request)
```

### 6. Check enforcement metrics

```bash
kubectl port-forward deploy/demo-workload 15081:15080 &
curl -s http://localhost:15081/metrics | grep koshi_enforcement
```

Expected metrics: `koshi_enforcement_decisions_total`, `koshi_enforcement_tokens_total`.

### 7. Reset to listener mode

```bash
kubectl patch deploy demo-workload -n default --type=json -p \
  '[{"op":"remove","path":"/spec/template/metadata/annotations/runtime.getkoshi.ai~1mode"},{"op":"remove","path":"/spec/template/metadata/annotations/runtime.getkoshi.ai~1policy"}]'
kubectl wait --for=condition=available deploy/demo-workload -n default --timeout=120s
```

## Optional Extension 2: Custom ConfigMap Sidecar Policy

**When to use:** prove that operator-authored budgets and guards work without standalone deployment — custom policy delivered via namespace-local ConfigMap.

This extension deploys a separate workload (`demo-custom-config`) with a custom ConfigMap. It shows both listener (shadow) and enforcement (blocking) modes with the same custom policy.

### Part A: Custom listener (shadow with custom policy)

#### 1. Deploy the ConfigMap and workload

```bash
kubectl apply -f custom-config-demo.yaml
kubectl wait --for=condition=available deploy/demo-custom-config -n default --timeout=120s
```

The deployment has `configmap` + `policy` annotations but no `mode` annotation, so the sidecar runs in **listener mode** with the custom policy. Shadow decisions are computed against the custom guards and budgets, but no traffic is blocked.

#### 2. Send a request that exceeds the custom guard (max_tokens > 128)

```bash
kubectl exec deploy/demo-custom-config -c app -- \
  curl -s --connect-timeout 2 --max-time 5 \
    -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    -d '{"model":"gpt-4","max_tokens":500,"messages":[{"role":"user","content":"hello"}]}'
```

The request proxies through (listener mode). Check the shadow event:

```bash
kubectl logs deploy/demo-custom-config -c koshi-listener --tail=5 | \
  jq 'select(.stream == "event") | {decision_shadow, reason_code}'
```

You should see `decision_shadow: "would_throttle"` with `reason_code: "guard_max_tokens"` — the custom guard flagged it, but traffic flowed.

### Part B: Custom enforcement (blocking with custom policy)

#### 3. Add the enforcement annotation

Same ConfigMap, same policy — just add the mode annotation:

```bash
kubectl patch deploy demo-custom-config -n default -p \
  '{"spec":{"template":{"metadata":{"annotations":{"runtime.getkoshi.ai/mode":"enforcement"}}}}}'
kubectl wait --for=condition=available deploy/demo-custom-config -n default --timeout=120s
```

#### 4. Resend the same over-guard request

```bash
kubectl exec deploy/demo-custom-config -c app -- \
  curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    -d '{"model":"gpt-4","max_tokens":500,"messages":[{"role":"user","content":"hello"}]}'
# Expected: 429 (throttled — custom guard exceeded)
```

Same request, same custom policy, different mode, different outcome.

#### 5. Confirm a within-limits request still passes

```bash
kubectl exec deploy/demo-custom-config -c app -- \
  curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:15080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Host: api.openai.com" \
    --connect-timeout 2 --max-time 5 \
    -d '{"model":"gpt-4","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}'
# Expected: 200 (or upstream error — the sidecar allowed the request)
```

See [`examples/sidecar-custom-configmap.yaml`](../../examples/sidecar-custom-configmap.yaml) and [`examples/sidecar-custom-deployment.yaml`](../../examples/sidecar-custom-deployment.yaml) for production-ready examples.

> **Separate manifests:** `enforcement-demo.yaml` and `custom-config-demo.yaml` are also available as standalone reference manifests if you prefer deploying separate workloads rather than patching annotations.

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

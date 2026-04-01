# Design Partner Onboarding

This guide walks through installing Koshi in listener mode, verifying the sidecar is active, collecting governance signal, and interpreting shadow outcomes — all without blocking any traffic.

## Install

```bash
# Install Koshi in listener mode (default)
helm install koshi oci://ghcr.io/koshihq/charts/koshi \
  --namespace koshi-system --create-namespace

# Opt a namespace in for sidecar injection
kubectl label namespace <namespace> runtime.getkoshi.ai/inject=true

# Restart workloads to pick up the sidecar
kubectl rollout restart deployment -n <namespace>
```

## Verify

**Confirm the sidecar is injected:**

```bash
kubectl get pod -n <namespace> -l app=<your-app> \
  -o jsonpath='{.items[0].spec.containers[*].name}'
# Should include "koshi-listener"
```

**Confirm structured events are flowing:**

```bash
kubectl logs -n <namespace> deploy/<your-app> -c koshi-listener --tail=50 | \
  jq 'select(.stream == "event")'
```

**Confirm the metrics endpoint is reachable:**

```bash
kubectl port-forward -n <namespace> deploy/<your-app> 15080:15080
curl -s http://localhost:15080/metrics | grep koshi_listener
```

## Collect

Koshi outputs two signal types. Any observability tool that ingests these formats works — no vendor-specific agent or plugin required.

**Structured events (JSON logs):**
- Source: stdout from container `koshi-listener`
- Filter: `stream == "event"`
- Fields: `namespace`, `workload_kind`, `workload_name`, `provider`, `decision_shadow`, `reason_code`, `estimated_tokens`, `actual_tokens`

**Prometheus metrics:**
- Source: `:15080/metrics` on each sidecar
- Series: `koshi_listener_decisions_total`, `koshi_listener_tokens_total`, `koshi_listener_latency_seconds`
- Labels: `namespace`, `decision_shadow`, `reason_code`, `provider`, `phase`

Works with: Datadog, Splunk, Elastic, CloudWatch, Grafana stack, or any tool that ingests container JSON logs and Prometheus-format metrics.

## Observe Baseline Posture

Injected sidecars use the built-in default listener policy automatically. With normal traffic, expect mostly `allow` outcomes. This baseline reveals:

- Which workloads generate AI API traffic
- What volume of tokens they consume
- Whether workload identity is being resolved correctly

Shadow outcomes like `would_throttle` indicate the built-in budget or guard is tighter than a workload's actual traffic pattern. This is expected and informative — it shows where the default policy boundary sits relative to real usage.

## Interpret Shadow Outcomes

| Shadow outcome | What it means | What to investigate |
|---|---|---|
| `allow` | Request passed all checks | Baseline posture is acceptable for this traffic |
| `would_throttle` | Budget or guard exceeded | Compare workload traffic volume against the built-in policy limits |
| `would_kill` | Severe budget pressure | Review whether this workload's token consumption is expected |
| `would_reject` + `identity_missing` | Identity not resolved | Check that the webhook is injecting identity env vars |
| `would_reject` + `policy_not_found` | No usable policy context | Relevant when explicit workload mappings are configured without a default fallback |

## Current Policy Experimentation Scope

The built-in default policy provides a fixed baseline for posture discovery. This is the intended starting point for design partner engagements.

Custom per-workload policy experimentation for injected sidecars (custom budgets, guards, or tier configurations) is not yet supported in this release. The control-plane deployment supports full policy configuration via the chart ConfigMap, but that config does not propagate to injected sidecars. Richer sidecar-level policy customization is planned for a future release.

## First Saved Searches / First Dashboards

Start with these queries to build your initial governance picture:

- **Top shadow outcomes:** Group events by `decision_shadow` — are you seeing mostly `allow`, or is there `would_throttle` / `would_kill` pressure?
- **Top reason codes:** Group non-`allow` events by `reason_code` — what's driving the shadow decisions?
- **Token burn by namespace:** Sum `koshi_listener_tokens_total{phase="reservation"}` by `namespace` — which namespaces consume the most?
- **Token burn by provider:** Sum `koshi_listener_tokens_total{phase="reservation"}` by `provider` — OpenAI vs Anthropic split

See [Kubernetes Observability Guide](kubernetes-observability.md) for detailed Prometheus queries, Grafana dashboard patterns, and Loki log queries.

## Next Steps

- Review the [pre-enforcement checklist](kubernetes-observability.md#pre-enforcement-checklist) when you're ready to move from posture discovery to enforcement
- See the [README](../README.md) for full configuration reference and architecture details

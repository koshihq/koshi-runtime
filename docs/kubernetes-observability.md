# Kubernetes Observability Guide

This guide covers how to observe AI workload traffic through Koshi Runtime's listener mode — structured events, Prometheus metrics, and latency analysis.

## Prerequisites

- Koshi installed via Helm in listener mode (default)
- At least one namespace labeled `runtime.getkoshi.ai/inject: "true"`
- Workloads restarted to pick up the sidecar

## Structured Events

Koshi emits two log streams to stdout, distinguished by the `stream` field:

| Stream | Purpose |
|--------|---------|
| `"event"` | Structured governance events — decisions, budget accounting, shadow outcomes |
| `"runtime"` | Operational logs — startup, config loading, errors, shutdown |

### Filtering Events

```bash
# All governance events from a workload
kubectl logs -n my-namespace deploy/my-app -c koshi-listener | \
  jq 'select(.stream == "event")'

# Shadow decisions only
kubectl logs -n my-namespace deploy/my-app -c koshi-listener | \
  jq 'select(.stream == "event" and .event_type == "shadow_decision")'

# Would-reject decisions (identity/policy failures)
kubectl logs -n my-namespace deploy/my-app -c koshi-listener | \
  jq 'select(.stream == "event" and .decision_shadow == "would_reject")'
```

### Event Fields Reference

Shadow decision events include these stable fields:

| Field | Type | Description |
|-------|------|-------------|
| `stream` | string | Always `"event"` |
| `event_type` | string | Event type (e.g., `shadow_decision`, `budget_reconciled`) |
| `mode` | string | `"listener"` |
| `namespace` | string | Pod's Kubernetes namespace |
| `workload_kind` | string | Normalized owner kind (`Deployment`, `StatefulSet`, etc.) |
| `workload_name` | string | Normalized owner name |
| `provider` | string | Detected provider (`openai`, `anthropic`) |
| `decision_shadow` | string | Shadow outcome: `allow`, `would_reject`, `would_throttle`, `would_kill` |
| `reason_code` | string | Machine-readable reason (see table below) |
| `estimated_tokens` | integer | Tokens reserved for this request |
| `actual_tokens` | integer | Actual tokens used (populated after response) |
| `genops.spec.version` | string | GenOps spec version (`0.1.0`) |

### Reason Codes

| Code | Shadow Decision | Meaning |
|------|----------------|---------|
| `identity_missing` | `would_reject` | Could not resolve workload identity and no default policy fallback available |
| `policy_not_found` | `would_reject` | Identity resolved but no explicit or default policy available for evaluation |
| `guard_max_tokens` | `would_throttle` | Request `max_tokens` exceeds the resolved policy's per-request guard |
| `budget_exhausted_throttle` | `would_throttle` | Resolved policy's rolling window budget exceeded → would throttle |
| `budget_exhausted_kill` | `would_kill` | Resolved policy's rolling window budget exceeded → would kill workload |
| (empty) | `allow` | Request passed all checks |

## Prometheus Metrics

### Scraping the Sidecar

The webhook injects scrape annotations on workload pods:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "15080"
prometheus.io/path: "/metrics"
```

If your Prometheus is configured for annotation-based discovery, sidecars are scraped automatically.

### Listener Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `koshi_listener_decisions_total` | counter | `namespace`, `decision_shadow`, `reason_code` | Shadow decision count |
| `koshi_listener_tokens_total` | counter | `namespace`, `provider`, `phase` | Token count by phase (`reservation`, `actual`) |
| `koshi_listener_latency_seconds` | histogram | (none) | Enforcement pipeline latency |
| `koshi_emitter_dropped_total` | gauge | (none) | Events dropped due to backpressure |

### Sample Prometheus Queries

**Shadow rejection rate by namespace:**

```promql
sum by (namespace) (
  rate(koshi_listener_decisions_total{decision_shadow=~"would_reject|would_throttle|would_kill"}[5m])
)
```

**What would be blocked, broken down by reason:**

```promql
sum by (reason_code) (
  rate(koshi_listener_decisions_total{decision_shadow!="allow"}[5m])
)
```

**Token consumption rate by namespace and provider:**

```promql
sum by (namespace, provider) (
  rate(koshi_listener_tokens_total{phase="reservation"}[5m])
)
```

**Enforcement pipeline latency (p99):**

```promql
histogram_quantile(0.99, rate(koshi_listener_latency_seconds_bucket[5m]))
```

**Ratio of shadow-rejected to total requests:**

```promql
sum(rate(koshi_listener_decisions_total{decision_shadow!="allow"}[5m]))
/
sum(rate(koshi_listener_decisions_total[5m]))
```

**Top namespaces by token consumption:**

```promql
topk(10,
  sum by (namespace) (
    rate(koshi_listener_tokens_total{phase="reservation"}[5m])
  )
)
```

## Latency Overhead

The `koshi_listener_latency_seconds` histogram measures the time spent in the enforcement pipeline — from identity resolution through the tier decision, before proxying to the upstream.

**Expected overhead:** Sub-millisecond for most requests. The pipeline is pure computation (map lookups, arithmetic) with no I/O.

**Investigating latency:**

```promql
# p50 latency
histogram_quantile(0.5, rate(koshi_listener_latency_seconds_bucket[5m]))

# p99 latency
histogram_quantile(0.99, rate(koshi_listener_latency_seconds_bucket[5m]))

# Request rate (to correlate with latency)
sum(rate(koshi_listener_decisions_total[5m]))
```

If you observe high p99 latency, check for:
- CPU throttling on the sidecar container (increase resource limits)
- High request rate causing GC pressure (check Go runtime metrics)

## Grafana Dashboard Patterns

### Panel: Shadow Decision Rate

```
Type: Time series
Query A: sum by (decision_shadow) (rate(koshi_listener_decisions_total[5m]))
Legend: {{decision_shadow}}
```

### Panel: Token Burn Rate

```
Type: Time series
Query A: sum by (namespace) (rate(koshi_listener_tokens_total{phase="reservation"}[5m]))
Legend: {{namespace}}
Unit: tokens/sec
```

### Panel: Would-Block Percentage

```
Type: Stat
Query A: sum(rate(koshi_listener_decisions_total{decision_shadow!="allow"}[5m])) / sum(rate(koshi_listener_decisions_total[5m])) * 100
Unit: percent
Thresholds: 0=green, 5=yellow, 20=red
```

### Panel: Enforcement Latency

```
Type: Histogram
Query A: histogram_quantile(0.5, rate(koshi_listener_latency_seconds_bucket[5m]))
Query B: histogram_quantile(0.99, rate(koshi_listener_latency_seconds_bucket[5m]))
Legend A: p50
Legend B: p99
Unit: seconds
```

## Loki Queries (if using Loki for log aggregation)

```logql
# All shadow events from a namespace
{namespace="my-namespace", container="koshi-listener"} | json | stream="event"

# Would-reject decisions
{namespace="my-namespace", container="koshi-listener"} | json | stream="event" | decision_shadow="would_reject"

# Budget exhaustion events
{namespace="my-namespace", container="koshi-listener"} | json | stream="event" | reason_code=~"budget_exhausted.*"
```

## Listener Accounting Scope

Listener mode uses a policy-scoped in-memory accounting key per sidecar instance, not a per-workload tracker key. Shadow counters and `would_throttle` / `would_kill` decisions reflect pressure against the configured listener policy within that sidecar process.

This does not mean all pods in a namespace or cluster share one live budget bucket. Koshi v1 has no cross-replica coordination. Each sidecar maintains its own independent accounting state.

Use listener mode to validate policy shape, guard pressure, and local runtime overhead. Do not interpret it as fleet-wide enforcement simulation.

## Interpreting Results Before Enabling Enforcement

### Interpreting Shadow Decisions

Shadow outcomes are meaningful only relative to the policy Koshi is evaluating. The recommended starting configuration uses a `default_policy`, which means most traffic will emit `allow`, `would_throttle`, or `would_kill` depending on policy pressure.

A `would_reject` event means Koshi could not find a usable policy context for the request — not merely that the runtime is in listener mode. If you see unexpected `would_reject` events, check that identity env vars are being injected and that a `default_policy` or explicit workload-to-policy mapping is configured.

Listener mode without explicit workloads is valid only when a `default_policy` is configured. Without either, config validation fails and the runtime does not start.

### Pre-Enforcement Checklist

Before switching from listener to enforcement mode, verify:

1. **No unexpected `would_reject` events.** If `identity_missing` or `policy_not_found` appear, check that workload identity env vars are being injected correctly.

2. **`would_throttle` rate is acceptable.** This is what your users would experience as 429s. If the rate is too high, increase `limit_tokens` or `window_seconds` in your policy.

3. **`would_kill` decisions are rare or expected.** Kill decisions are severe — the workload would be refused service entirely.

4. **Latency overhead is negligible.** p99 should be well under 1ms for typical request rates.

5. **Token accounting matches expectations.** Compare `koshi_listener_tokens_total{phase="reservation"}` against your provider's billing dashboard.

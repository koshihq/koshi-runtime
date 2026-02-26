# Koshi Runtime

Koshi Runtime is a workload-scoped runtime enforcement plane for AI systems. It is an HTTP reverse proxy sidecar that enforces deterministic runtime policy at the workload boundary using reservation-first accounting. Policy attaches at the workload level; enforcement executes per replica.

## How It Works

Koshi Runtime sits between your workloads and upstream AI providers. Every request passes through an enforcement pipeline:

```
Request → Identify workload (header) → Resolve policy → Extract max_tokens →
  Per-request guard check → Reserve tokens (rolling window) → Tier decision →
  Proxy to upstream → Record actual usage on response
```

**Key behaviors:**
- **Fail closed** on policy/identity violations — unknown workloads get 403
- **Fail open** on infrastructure failures — requests proxy through, reservations stand
- **Degraded mode** — a single panic disables enforcement; `/healthz` and `/readyz` return 503 so Kubernetes restarts the container
- **Reservation-first accounting** — reserves estimated tokens before the request, reconciles with actual usage after the response; enforcement state bounded at zero; exactly-once reconciliation per reservation

## Quick Start

```bash
# Build
make build

# Run tests (with race detector)
make test-race

# Lint
make lint
make check-genops-spec  # validate GenOps v0.1.0 contract constants

# Run locally
KOSHI_CONFIG_PATH=examples/config.yaml bin/koshi
```

The server listens on `:8080` by default (configurable via `listen_addr` in config).

## Configuration

Config is loaded from `KOSHI_CONFIG_PATH` (default: `/etc/koshi/config.yaml`). See [`examples/config.yaml`](examples/config.yaml) for a fully annotated reference.

### Structure

```yaml
upstreams:
  openai: "https://api.openai.com"
  anthropic: "https://api.anthropic.com"

workloads:
  - id: "my-agent"
    identity: { mode: "header", key: "x-genops-workload-id" }
    policy_refs: ["standard"]
    # ... type, owner_team, environment, model_targets

policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 300
        limit_tokens: 100000
        burst_tokens: 10000
    guards:
      max_tokens_per_request: 4096
    decision_tiers:
      tier1_auto: { action: "throttle" }
      tier3_platform: { action: "kill_workload" }
```

### Optional Settings

| Field | Default | Description |
|-------|---------|-------------|
| `default_policy` | none | Policy applied to unknown workloads (opt-in fail-open) |
| `strict_mode` | `false` | Reject unknown workloads even if `default_policy` is set |
| `sse_extraction` | `true` | Extract actual token usage from OpenAI SSE streams |
| `listen_addr` | `:8080` | Server listen address |

## Deployment

### Docker

```bash
make docker
# Produces koshi:latest — distroless nonroot image
```

### Kubernetes (Helm)

```bash
helm install koshi deploy/helm/koshi/ \
  --set image.repository=your-registry/koshi \
  --set image.tag=v1.0.0
```

Config is injected via ConfigMap mounted at `/etc/koshi/config.yaml`. The chart configures:
- Liveness probe on `/healthz` (returns 503 when degraded, triggering container restart)
- Readiness probe on `/readyz` (returns 503 when degraded, removing pod from endpoints)
- Default resources: 100m–500m CPU, 64Mi–128Mi memory

## Health Endpoints

| Endpoint | Method | Response |
|----------|--------|----------|
| `/healthz` | GET | 200 when healthy, 503 when degraded |
| `/readyz` | GET | 200 when healthy, 503 when degraded |

### /status

`GET /status` returns runtime diagnostics and per-workload budget state.

| Field | Type | Description |
|-------|------|-------------|
| `version` | string | Koshi Runtime version |
| `genops_spec_version` | string | GenOps spec version implemented (`0.1.0`) |
| `dropped_events` | integer | Count of dropped telemetry events |
| `workloads` | object | Per-workload budget/enforcement state |

## Design Documents

Core architectural framing for Koshi Runtime v1:

- [Deterministic Accounting Invariants](docs/design/koshi-v1-deterministic-accounting-invariants.md) — 12 runtime invariants that define the enforcement boundary
- [Enforcement Boundary](docs/design/koshi-v1-enforcement-boundary.md) — layer model, state ownership, why workload is the policy primitive
- [Operator Trust Guarantees](docs/design/koshi-v1-operator-trust-guarantees.md) — externally observable contract surfaces stable across v1.x
- [Why Koshi Exists](docs/design/koshi-v1-why-koshi-exists.md) — the structural problem, the missing primitive, why not application enforcement

## GenOps Compatibility

Koshi Runtime is built against the [GenOps Governance Specification](https://github.com/koshihq/genops-spec) and emits GenOps-compatible governance telemetry.

- Spec version (working draft): `0.1.0`

Koshi Runtime exposes the implemented spec version at `GET /status` via the top-level `genops_spec_version` field.

## Response Codes

| Code | Meaning |
|------|---------|
| 403 | Unknown workload or no matching policy |
| 429 | Per-request guard exceeded or budget throttled (includes `Retry-After` header) |
| 502 | No upstream configured for detected provider |
| 503 | Kill decision or degraded mode |
| 504 | Upstream timeout |

All error responses (403, 429, 502, 503, 504) include:

- **`X-GenOps-Decision` header** — machine-readable decision: `reject`, `throttle`, `kill`, or `degraded`
- **Structured JSON body:**

| Field | Type | Present |
|-------|------|---------|
| `error` | string | Always — error code (e.g. `identity_required`, `rate_limited`, `workload_killed`) |
| `category` | string | Always — `"enforcement"` or `"system"` |
| `reason` | string | Always — human-readable explanation |
| `tokens_used` | integer | 429/503 enforcement responses only |
| `tokens_limit` | integer | 429/503 enforcement responses only |

## Deployment & Operational Semantics (v1)

For regulated enterprise platform leads evaluating Koshi Runtime.

### Enforcement Behavior

A request receives **429 Too Many Requests** when either:

1. **Per-request guard exceeded.** The request's `max_tokens` field exceeds the policy's `guards.max_tokens_per_request`. No `Retry-After` header is set because this is a per-request property, not a transient condition.

2. **Rolling window budget exhausted.** The workload's cumulative token reservations within the configured `window_seconds` exceed `limit_tokens`, and no burst capacity remains. The response includes a `Retry-After` header set to `window_seconds / 2`.

Both the 429 response body and the `enforcement` log event include budget state:

| Field | Type | Meaning |
|-------|------|---------|
| `tokens_used` | integer | Total tokens currently reserved in the rolling window for this workload |
| `tokens_limit` | integer | The policy's `limit_tokens` ceiling for this window |
| `burst_remaining` | integer | Burst tokens still available (log event only) |
| `reserved_tokens` | integer | Tokens the rejected request attempted to reserve (log event only) |

**Reservation vs actual accounting.** Koshi Runtime pre-deducts `max_tokens` from the budget before proxying (the reservation). After the upstream responds, it reconciles with actual usage: `delta = actual_tokens - reserved_tokens`. If the model generated fewer tokens than `max_tokens`, the difference is returned to the budget.

**Streaming vs non-streaming:**

| Scenario | Accounting |
|----------|------------|
| Non-streaming (any provider) | Actual usage extracted from response body. Budget reconciled. |
| OpenAI streaming, SSE extraction enabled | Actual usage extracted from final SSE chunk. Budget reconciled. |
| OpenAI streaming, SSE extraction disabled | Reservation stands. No reconciliation. |
| Anthropic streaming, SSE extraction enabled | Actual usage extracted from Anthropic SSE events (`message_start` + `message_delta`). Budget reconciled. |

### Degraded Mode

A panic anywhere in the enforcement pipeline triggers degraded mode. This is irreversible without a container restart.

**Sequence:**
1. Panic recovered. `degraded_panic` event emitted with stack trace.
2. All subsequent requests bypass enforcement and proxy directly to upstream.
3. `/healthz` returns 503. `/readyz` returns 503.
4. Kubernetes liveness probe fails after `failureThreshold` consecutive checks.
5. Container is restarted. Enforcement resumes with fresh (zeroed) budget state.

**With default Helm probe settings** (`periodSeconds: 10`, `failureThreshold: 3`):
- Time from panic to container restart: **21–63 seconds** depending on probe timing.
- Hard downtime (connection refused on localhost): **1–5 seconds** during container restart.
- All in-memory budget state is lost on restart. Every workload starts with a fresh budget window. There is no persistence layer, no checkpoint, and no recovery mechanism.

### Logging Schema

Events are emitted as structured JSON to stdout via `slog.JSONHandler`. Numeric fields (`tokens_used`, `tokens_limit`, `burst_remaining`, `reserved_tokens`) are native JSON numbers, not strings.

**Event types:**

| `event_type` | Severity | When |
|---|---|---|
| `request_allowed` | info | Request passed all enforcement checks |
| `enforcement` | warn | Request denied (throttle or kill) |
| `guard_rejected` | warn | Per-request `max_tokens` exceeded guard |
| `identity_rejected` | warn | Unknown workload, no default policy |
| `policy_rejected` | warn | Known workload, no matching policy |
| `degraded_panic` | error | Panic in enforcement path (includes stack trace) |
| `degraded_passthrough` | warn | Request proxied in degraded mode |
| `upstream_error` | warn | Upstream returned 5xx |
| `upstream_timeout` | error | Upstream did not respond |

**Schema stability:** Event types and field names are stable across v1.x. New fields or event types may be added in future versions. Existing fields will not be removed or renamed without a version bump.

### Known v1 Limitations

- **In-memory budget state.** Each Koshi Runtime replica maintains its own budget in memory. State is lost on restart — pod restart creates a new enforcement epoch with zeroed budget. There is no cross-replica synchronization; each replica enforces independently.
- **No cross-replica coordination.** Budget enforcement is per-replica. Cluster-wide aggregate enforcement is not provided in v1.
- **Single policy per workload.** Only the first `policy_ref` is used if multiple are configured.
- **Restart resets enforcement epoch.** Rolling deployments reset all budgets. Budget continuity across restarts is not guaranteed. This does not violate invariants because no persistence is claimed in v1.

### What v1 Is Designed For

- Production-grade, single-replica sidecar deployment per workload.
- Deterministic enforcement correctness: fail-closed identity, per-request guards, rolling window budget, tier decisions.
- Reservation-first accounting with exactly-once reconciliation across non-streaming, OpenAI streaming, and Anthropic streaming.
- Kubernetes-native operation: probe behavior, restart semantics, graceful shutdown, PodDisruptionBudget.
- Observability: Prometheus `/metrics`, `/status` endpoint, structured JSON logging.

v1 is not designed for multi-replica budget coordination, cross-instance enforcement, or environments requiring persistent audit trails across restarts.

# Koshi v1 — Operator Trust Guarantees

## Scope

This document defines externally observable operational guarantees provided by Koshi v1.

These guarantees apply per replica unless explicitly stated.
Only behaviors explicitly listed here are considered contract surfaces.

## 1. Enforcement Guarantees

### 1.1 Fail-Closed Identity

- Unknown workloads are rejected unless `default_policy` is configured.
- There is no implicit allow behavior.
- Identity resolution failure results in rejection.

### 1.2 Reservation-First Enforcement

- Upstream execution does not occur without successful reservation.
- Enforcement decisions occur before upstream execution.
- Reconciliation does not retroactively alter enforcement decisions.

### 1.3 Non-Negative Enforcement State

- Enforcement budget state never becomes negative.
- Reservation fails if insufficient tokens are available.
- Reconciliation cannot create negative enforcement state.

### 1.4 Exactly-Once Reconciliation

- Each successful reservation is reconciled at most once.
- Parser failure does not trigger refund behavior.
- Reservation state is never silently released.

### 1.5 Degraded Mode

When degraded:

- `/healthz` and `/readyz` return HTTP 503.
- Requests are rejected.
- No upstream proxying occurs.
- Enforcement state is not mutated.

Telemetry backpressure alone does not trigger degraded mode.

### 1.6 Deterministic Decisions (Per Replica)

Given identical policy configuration, identity resolution, and enforcement state:

- Identical requests produce identical enforcement decisions within a replica.

Cluster-wide determinism is not provided in v1.

## 2. Observability Guarantees

### 2.1 Structured Logging

- JSON log format is stable for fields documented in this section.
- Documented numeric budget fields are emitted as native JSON numbers.
- Field removals or renames require version bump.

### 2.2 Enforcement Response Bodies

For 429 and 503 responses:

- `tokens_used` is included.
- `tokens_limit` is included.
- Both fields are numeric.

### 2.3 Status Endpoint

`/status` exposes per-workload:

- `tokens_used`
- `tokens_limit`
- `burst_remaining`
- `dropped_events`

Values reflect enforcement state at request time.

### 2.4 Metrics Endpoint

Prometheus `/metrics` exposes:

- `koshi_requests_total`
- `koshi_tokens_used_total`
- `koshi_enforcement_decisions_total`
- `koshi_enforcement_latency_seconds`
- `koshi_emitter_dropped_total`

Metric names are stable across v1.x.

### 2.5 Telemetry Independence

- Enforcement decisions do not depend on telemetry success.
- Event emission is best-effort.
- Dropped event counts are surfaced.

## 3. Operational Guarantees

### 3.1 Graceful Shutdown

On SIGTERM:

- Readiness is withdrawn.
- No new reservations are accepted after readiness is withdrawn.
- In-flight requests are allowed to complete within configured grace period (default 30 seconds).

### 3.2 Startup Configuration Validation

- Configuration validation failures prevent startup.
- Unsupported upstream providers are rejected.
- Multiple `policy_refs` are rejected.
- Identity key mismatches prevent startup.
- There is no runtime silent fallback.

### 3.3 Health Semantics

- `/healthz` reflects process liveness.
- `/readyz` reflects enforcement readiness.
- Degraded state causes readiness failure.
- Health endpoints reflect Koshi process state only.

## 4. Explicit Non-Guarantees

Koshi v1 does not provide:

- Cross-replica budget coordination
- Persistent budget state across restarts
- Budget continuity across restarts
- SLA-level atomic enforcement
- Hot configuration reload
- Durable audit storage

## 5. Versioning Commitment

The following surfaces are considered stable in v1.x:

- Reservation-first enforcement behavior
- Non-negative enforcement state
- Enforcement decision ordering
- Exactly-once reconciliation semantics
- Response body structure for enforcement failures
- Documented log fields
- Documented metric names
- Health endpoint semantics

Breaking changes to these surfaces require version bump.

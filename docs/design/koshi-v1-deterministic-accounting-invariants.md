# Koshi v1 — Deterministic Accounting Invariants

## Purpose

This document defines the runtime accounting and enforcement invariants guaranteed by Koshi v1.

These invariants define the enforcement boundary of the system.
If any invariant is violated, Koshi is not operating correctly.

Koshi v1 defines a reservation-first, non-negative, per-replica enforcement boundary for runtime AI workloads.

All guarantees apply per replica unless explicitly stated.
Cross-replica coordination is out of scope for v1.

## System State Definition

Per-replica determinism applies to identical system state.

System state includes:

- Policy configuration
- Identity resolution result
- In-memory enforcement budget state
- Time-indexed rolling window state
- Request metadata
- Degraded mode status

Time and degraded status are explicitly part of system state.

## Enforcement Decision Boundary

An enforcement decision is the allow or deny outcome produced immediately after guard evaluation and reservation attempt.

Enforcement decisions:

- Occur before upstream execution
- Are not retroactively modified
- Are not altered by reconciliation
- Are independent of telemetry success

This boundary defines the moment at which runtime enforcement is applied.

## 1. Per-Replica Deterministic Enforcement

### Invariant

Given identical system state, Koshi produces the same enforcement decision within a replica.

Cluster-wide determinism is not guaranteed in v1.

### SLA Clarification

Workload-to-SLA mapping is conceptual in v1.
Enforcement operates strictly at the replica level.
SLA-level guarantees across replicas are not provided.

## 2. Reservation-First Accounting

### Invariant

Budget is reserved before upstream execution.
No upstream call occurs without successful reservation.
Post-hoc-only accounting is never used.

### Enforcement Sequence

1. Guard validation
2. Reservation attempt
3. Enforcement decision
4. Upstream execution
5. Reconciliation

Reservation and mutation of enforcement state occur as a single atomic, linearizable operation within a replica.

Concurrent requests may observe different outcomes due to ordering, but enforcement state will never violate invariants.

## 3. Budget Floor at Zero (Enforcement State)

### Invariant

The enforcement budget state can never become negative.
Reservation fails if insufficient tokens are available.
Reconciliation cannot create a negative enforcement balance.

### Enforcement State vs Observed Usage

Two accounting domains exist:

**Enforcement State**

- Governs allow/deny decisions
- Bounded at zero
- Cannot go negative
- Drives future reservation eligibility

**Observed Usage State**

- Records actual upstream usage
- May exceed reserved amount
- Used for telemetry and visibility
- Does not further reduce enforcement capacity beyond clamping to zero

If upstream usage exceeds reservation:

- Observed usage records actual tokens consumed.
- Enforcement state remains clamped at zero.
- No additional enforcement reduction occurs beyond exhaustion.
- Overspend cannot be retroactively denied.

## 4. Guard-Before-Reservation

### Invariant

All per-request guards execute before reservation.

Example:

- `max_tokens_per_request`

Guards prevent pathological single-request amplification even when budget exists.

## 5. Reconciliation Completeness

### Invariant

Every successful reservation resolves to exactly one of:

- **Reconciled** (actual usage applied)
- **Released** (upstream 5xx or execution failure)

Reconciliation occurs exactly once per reservation.

**Exception:**

- Pod restart (see Section 10)

No reservation may remain unresolved during normal operation.

### Streaming Semantics (SSE)

For streaming providers:

- Usage is accumulated incrementally.
- Reconciliation occurs on:
  - Stream completion
  - Context cancellation
  - Client disconnect
  - Upstream termination

**Partial Extraction Rules**

If partial usage is available:

- Reconcile accumulated usage.
- Do not refund unused reservation.

If usage extraction fails entirely:

- Reserved amount is treated as actual usage.
- Reservation is not released.

**Parser Failure Precedence**

If partial usage was accumulated and a parser failure occurs later:

- Reconcile accumulated usage.
- Do not double count.
- Do not refund unused reservation.

Parser failure must never create refund behavior or double reconciliation.

## 6. Degraded Mode Is Fail-Closed

### Invariant

When degraded:

- `/healthz` and `/readyz` return 503.
- Requests are rejected.
- No upstream proxying occurs.
- No reservation is performed.
- Enforcement state is not mutated.

Degraded status is part of system state.

### Trigger Scope

Degraded mode is triggered by internal safety conditions.
Telemetry backpressure alone must not trigger degraded mode.
Enforcement availability is independent of observability pipeline health.

## 7. Fail-Closed Identity

### Invariant

Identity resolution is explicit.
Unknown workloads are rejected unless `default_policy` is explicitly configured.

If `default_policy` is configured:

- It applies deterministically.
- There is no implicit allow behavior.

Identity resolution output is part of system state.

## 8. Time-Window Determinism

Rolling window budgets are time-indexed.
Determinism applies to identical window state.
Requests evaluated at different times may produce different decisions.
Time is part of system state.

Restart resets rolling window state.
Budget continuity across restarts is not guaranteed in v1.

## 9. Concurrency Semantics

Reservation logic is atomic and linearizable within a replica.

Under concurrent reservation attempts:

- Ordering may determine which request consumes remaining tokens.
- Near-exhaustion scenarios may produce order-dependent success.

This does not violate determinism because system state differs between evaluations.

Strict global ordering across concurrent requests is not guaranteed.

## 10. Restart Epoch Semantics

### Invariant

Pod restart creates a new enforcement epoch.

- In-memory enforcement budget state is lost.
- Rolling window state resets.
- In-flight requests are terminated.
- No enforcement continues across epochs.

Restart does not violate invariants because no persistence is claimed in v1.

## 11. Observable State Integrity

While a replica is healthy, enforcement state is externally inspectable.

`/status` reflects enforcement state at time of request.

v1 guarantees exposure of:

- `tokens_used`
- `tokens_limit`
- `burst_remaining`
- `dropped_events`

Prometheus `/metrics` exposes:

- Request totals
- Token totals
- Enforcement decisions
- Enforcement latency
- Dropped events

### Telemetry Clarification

Event emission is best-effort.

If events are dropped:

- Enforcement decisions remain unaffected.
- Dropped count is surfaced.

Enforcement does not depend on telemetry success.

## 12. Config Determinism

Invalid configuration fails at startup.
There is no runtime silent fallback.

v1 guarantees:

- Identity key validation at load
- Unsupported upstream rejection
- Multiple `policy_refs` rejected at validation

## Accepted v1 Limitations

These do not violate invariants:

- No cross-replica coordination
- No persistent budget storage
- No durable audit backend
- No hot config reload
- Restart resets enforcement epoch
- SLA-level budget guarantees are not provided

## Changes That Require Versioned Architectural Shift

The following would alter invariants:

- Post-hoc-only accounting
- Allowing negative enforcement budgets
- Fail-open degraded behavior
- Cross-replica mutation without deterministic protocol
- Silent policy fallback
- Reconciliation that refunds on parser failure
- Enforcement dependent on telemetry success

Such changes require architectural versioning.

## Summary

Koshi v1 guarantees, per replica:

- Reservation-first deterministic enforcement
- Atomic and linearizable reservation logic
- Enforcement budget bounded at zero
- Clear separation of enforcement state and observed usage
- Exactly-once reconciliation
- Conservative reconciliation under parser failure
- Fail-closed degraded and identity behavior
- Explicit concurrency semantics
- Explicit restart epoch semantics
- Observable enforcement state while healthy
- Startup configuration validation

These invariants define the runtime enforcement boundary.
Everything else is implementation detail.

## Forward Compatibility: Distributed Coordination Constraints

Koshi v1 operates with replica-scoped enforcement state and no cross-replica coordination.

Future versions may introduce shared state or distributed coordination.
Such extensions must preserve the core enforcement invariants defined in this document.

The following constraints are non-negotiable.

### Invariants That Must Survive Distribution

Any distributed coordination mechanism must preserve:

**Reservation-first accounting**

- Upstream execution must not occur without prior reservation.

**Enforcement budget floor at zero**

- Enforcement state must never become negative.

**No silent pass-through**

- Every request must result in an observable allow, deny, or degraded outcome.

**Conservative reconciliation semantics**

- Parser or extraction failure must not create refund behavior.

**Deterministic decision under identical global state**

- Given identical distributed state and identical inputs, enforcement decisions must remain deterministic.

These properties define the enforcement boundary.
Distribution must extend them, not weaken them.

Any distributed model that permits temporary negative enforcement state violates the enforcement boundary defined in this document.

### Invariants That Are Replica-Scoped in v1

The following properties are implementation boundaries of v1 and may evolve:

- Replica independence
- Restart epoch semantics
- Rolling window state locality
- SLA-level non-guarantee across replicas

Changes to these areas may introduce shared state, persistence, or coordination protocols.
However, such changes must not compromise the foundational invariants above.

### Distributed Determinism Requirement

If global coordination is introduced, the system must ensure:

- Reservation remains atomic with respect to shared budget state.
- Concurrent distributed reservations cannot violate the enforcement budget floor.
- Distributed mutation preserves linearizable enforcement behavior.

Eventual consistency models that allow temporary negative enforcement state violate the enforcement boundary.

### Architectural Implication

Distributed coordination, if introduced, must behave as an extension of enforcement state — not as a post-hoc accounting system.

The enforcement boundary must remain runtime-first, not billing-first.

If distributed coordination cannot preserve these constraints, it constitutes a versioned architectural shift.

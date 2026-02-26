# Koshi — Enforcement Boundary (v1)

## Executive Framing

Koshi Runtime enforces policy at the workload boundary, executing deterministically per replica using reservation-first accounting.

- **Workload** is the smallest stable runtime identity to which policy attaches.
- **Replica** is the execution surface where enforcement state mutates.
- **SLA** and **service** layers remain conceptual in v1.

## I. Conceptual Enforcement Layers

```
┌──────────────────────────────────────────────┐
│                SLA Boundary                  │
│   (Business KPI / Spend Objective / SLO)    │
│   Conceptual in v1                          │
└──────────────────────────────────────────────┘
                     │
                     ▼
┌──────────────────────────────────────────────┐
│             Workload Boundary               │
│   (Identity-resolved runtime execution)     │
│                                              │
│   ← Policy attaches here                    │
│   ← Budget defined here                     │
└──────────────────────────────────────────────┘
                     │
                     ▼
┌──────────────────────────────────────────────┐
│              Replica Boundary               │
│   (Pod instance + Koshi sidecar)            │
│                                              │
│   ← Enforcement executes here               │
│   ← Reservation mutates state here          │
└──────────────────────────────────────────────┘
```

## II. Layer Definitions

### SLA Boundary

- Represents business intent.
- May span multiple services and workloads.
- Has no runtime enforcement state in v1.
- SLA-level guarantees are not enforced atomically in v1.

### Service Boundary

- Logical grouping of functionality.
- May contain multiple workloads.
- Not a policy attachment surface.
- Not an enforcement execution surface.

### Workload Boundary (Policy Primitive)

Workload is:

- Identity-resolved
- Policy-attached
- Budget-defined
- Independent of replica scaling
- Independent of vendor routing

Policy and budget configuration bind here.
Workload is the atomic enforcement key.

### Replica Boundary (Execution Surface)

Each replica contains:

- Application container
- Koshi Runtime sidecar
- In-memory enforcement state

Replica owns:

- Rolling window budget state
- Reservation mutation
- Reconciliation mutation
- Degraded state

Replica executes enforcement decisions but does not define policy.

## III. Enforcement Flow

```
Incoming Request
    ↓
Identity Resolution
    ↓
Workload Identification
    ↓
Guard Evaluation
    ↓
Atomic Reservation (Replica-local state)
    or DENY  ← Enforcement Decision Boundary
    ↓
Upstream Execution (if allowed)
    ↓
Reconciliation (exactly once)
```

Critical properties:

- Enforcement decision occurs before upstream execution.
- Reservation mutation is atomic per replica.
- Reconciliation never retroactively alters decision.
- Enforcement state never becomes negative.

The enforcement boundary is crossed at reservation.

## IV. State Ownership

| Layer | Owns Policy | Owns Runtime Budget State |
|---|---|---|
| SLA | No | No |
| Service | No | No |
| Workload | Yes | Logical definition only |
| Replica | No | Yes (mutation surface) |

Important:

- Workload defines budget.
- Replica mutates budget.

## V. Why Enforcement Lives at the Workload Boundary

Workload is:

- More granular than service
- More stable than model
- More infra-native than user identity
- Independent of scaling topology
- Aligned with deployment failure domains
- Suitable as a distributed coordination key (per workload)

Workload is the lowest stable abstraction that preserves deterministic enforcement semantics.

## VI. Failure Domain Alignment

- Workloads are stable.
- Replicas churn.
- Services evolve.
- Models change.

Workload boundary remains stable across these events.
Replica boundary aligns with enforcement state mutation.

This preserves isolation, deterministic behavior under scaling, and clear enforcement authority.

## VII. Distributed Survivability (v1 Context)

Although v1 enforces per replica, workload identity is suitable as a distributed coordination key.

Any future shared-state coordination must:

- Key reservation state by workload identity
- Preserve reservation-first semantics
- Maintain non-negative enforcement state

Cross-workload atomic enforcement is not provided in v1.
Workload remains the atomic enforcement unit.

## Core Boundary Statement

Policy attaches at the workload boundary.
Enforcement executes at the replica boundary.
Reservation-first accounting defines the runtime decision surface.

Everything above workload is intent.
Everything below replica is execution detail.

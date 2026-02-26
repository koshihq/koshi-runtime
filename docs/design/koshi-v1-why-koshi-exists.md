# Why Koshi Exists

Koshi is a workload-scoped runtime enforcement plane for AI systems.

## The Structural Problem

AI workloads introduce a new class of runtime mutation:

- Token consumption is not intrinsically bounded by local infrastructure.
- Model parameters dynamically alter execution behavior.
- Concurrency amplifies aggregate runtime mutation.
- Once upstream execution begins, enforcement authority is lost.
- Guardrails are inconsistently implemented in application code.
- Observability systems do not enforce pre-execution control.

The failure mode is not lack of visibility.
The failure mode is absence of deterministic runtime enforcement authority at the execution boundary.

Existing infrastructure layers do not own this boundary.

## Where Existing Systems Attach

| Layer | What It Controls | Why It Fails for AI Runtime Enforcement |
|---|---|---|
| Billing | Post-hoc spend tracking | Observes after execution |
| FinOps | Cost attribution | No runtime authority |
| API Gateway | Routing, auth, rate limits | Does not perform reservation-based workload policy mutation |
| Application Logic | Business rules | Enforcement depends on application correctness |
| Model Router | Vendor selection | Does not enforce workload-level invariants |
| Kubernetes | Scheduling and scaling | Does not understand AI request semantics |
| OpenTelemetry | Telemetry | Observes but does not enforce |

Rate limiting constrains request rate.
Koshi mutates shared workload policy state before execution.

These are distinct control surfaces.

None of the above provide reservation-based, workload-scoped runtime enforcement before upstream execution.

## The Missing Primitive

AI systems require:

- A deterministic enforcement boundary.
- Pre-execution policy state mutation where limits apply.
- Non-negative enforcement guarantees.
- Guard evaluation independent of application logic.
- Policy attachment at a stable infrastructure identity.

This primitive is not present in the current stack.

Without it:

- Enforcement depends on application implementation.
- Reactive controls attempt to correct post-execution effects.
- Concurrency multiplies exposure.
- Scaling increases enforcement drift.

## Why Not Application Enforcement?

Application-level guardrails are:

- Optional per codebase.
- Language-specific.
- Bypassable during rapid iteration.
- Dependent on deploy cadence.

Infrastructure-level enforcement is:

- Uniform across workloads.
- Independent of application correctness.
- Enforced at the execution boundary.
- Not subject to feature-flag drift.

Koshi enforces policy regardless of application implementation.

## Why the Workload Boundary

- Service is too coarse.
- User identity is application-scoped.
- Model is vendor-coupled.
- Cluster is too broad.
- Namespace is an organizational grouping.
- Workload is an execution identity.

Workload is:

- Identity-resolved
- Stable across replica churn
- Independent of vendor routing
- Infrastructure-native
- Aligned with failure domains

Workload is the smallest stable unit suitable for deterministic runtime policy attachment.

Higher-level tenancy and SLA constructs may compose above workload.
Workload remains the atomic enforcement unit.

## What Koshi Introduces

Koshi introduces a workload-scoped enforcement plane that:

- Attaches policy at the workload boundary.
- Executes enforcement per replica.
- Evaluates guards before execution.
- Performs reservation-based policy state mutation.
- Guarantees non-negative enforcement state.
- Produces deterministic enforcement decisions per replica.

Enforcement executes within the replica boundary and does not require external coordination in v1.

Replica-scoped enforcement provides deterministic isolation within each workload instance.

Policy dimensions may include:

- Token ceilings
- Request parameter constraints
- Concurrency bounds
- Model usage limits
- Budget windows
- Guard evaluation rules

## Replica Scope and Coordination

Replica-scoped enforcement ensures:

- Enforcement authority is never lost at the execution boundary.
- No replica can silently bypass policy.
- Runtime mutation remains bounded within a failure domain.

Global aggregate enforcement across replicas is not provided in v1.

Cluster-level aggregation may be composed externally but is not required for per-replica correctness.

## Why This Does Not Live in the Cloud Vendor

Cloud providers can enforce account-level and service-level controls.
Koshi enforces workload-scoped runtime policy at the execution boundary.

Vendor-native controls:

- Are provider-scoped.
- Attach to service APIs, not Kubernetes workload identity.
- Do not enforce cross-provider policy consistency.
- Do not provide uniform enforcement across managed and self-hosted models.
- Do not follow workloads across heterogeneous execution environments.

Cloud vendors control services.
Enterprises control workloads.

Enforcement authority must exist where execution occurs.
Koshi operates at that boundary.

## The Architectural Inevitability

As AI workloads scale:

- Concurrency increases.
- Parameter surfaces expand.
- Replica counts grow.
- Runtime mutation compounds.

Without deterministic pre-execution enforcement, scaling multiplies exposure.

The enforcement primitive must:

- Operate below service boundaries.
- Be vendor-agnostic.
- Mutate policy state before execution.
- Remain deterministic under concurrency.

Koshi provides a workload-scoped implementation of that primitive.

## What Happens Without It

Organizations implement:

- Guardrails in application code.
- Spend alerts.
- Gateway rate limits.
- Vendor-specific throttling.
- Manual audit loops.

These mechanisms:

- React after execution.
- Depend on application correctness.
- Do not provide deterministic workload-level guarantees.
- Do not scale cleanly with replica growth.

They are compensating controls, not enforcement primitives.

## Core Claim

AI workloads introduce runtime mutation surfaces that existing infrastructure does not deterministically control.

Koshi provides a workload-scoped, reservation-based enforcement boundary for AI runtime policy.

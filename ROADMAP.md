# Koshi Runtime Roadmap

This roadmap describes the intended direction of Koshi Runtime — an open runtime substrate for platform and SRE teams governing AI workloads in Kubernetes. It is directional, not a delivery schedule or commercial commitment.

## What Koshi Runtime Is For

Koshi Runtime is a Kubernetes-native runtime substrate that sits at the workload boundary between application containers and upstream AI providers. It provides:

- **Listener-first adoption.** Discover your governance posture at the execution boundary before enforcing it. The full enforcement pipeline runs on every request in listener mode, emitting structured events and metrics that reveal exactly where policies would intervene — without blocking traffic.
- **Structured governance signals.** Every request produces a machine-readable decision with a stable reason code — identity resolution, policy lookup, per-request guards, rolling budget accounting, and tiered enforcement.
- **Config-driven mode switching.** The same binary and image supports both listener and enforcement modes. For standalone deployments, moving from observation to enforcement is a config change. For injected sidecars, enforcement with built-in policy selection is available via pod annotations; arbitrary custom policy is available via namespace-local ConfigMap delivery.
- **Auditable, reversible, bounded behavior.** Reservation-first token accounting with reconciliation. Deterministic enforcement decisions. Safe defaults that fail open on infrastructure and fail closed on policy.

## Available Now

- **Listener mode (governance posture discovery)** — full enforcement pipeline runs on every request without blocking traffic. Shadow decisions (`would_reject`, `would_throttle`, `would_kill`) reveal where policies would intervene, letting teams validate their governance posture before enabling enforcement.
- **Sidecar injection** — mutating admission webhook injects the koshi-listener sidecar into pods in labeled namespaces. Webhook `failurePolicy: Ignore` ensures pods still create if the injector is down.
- **Structured events and metrics** — JSON events on stdout with `stream: event` for governance decisions, `stream: runtime` for operational logs. Prometheus metrics (`koshi_listener_decisions_total`, `koshi_listener_tokens_total`, `koshi_listener_latency_seconds`) for dashboard and alerting.
- **Reservation-first token accounting** — budget pre-deducted before proxying, reconciled with actual usage after response. Rolling window with optional burst. Budget floor at zero.
- **Stable reason codes** — machine-readable codes on all decisions and error responses: `identity_missing`, `policy_not_found`, `guard_max_tokens`, `budget_exhausted_throttle`, `budget_exhausted_kill`, and others.
- **Helm chart with safe defaults** — NetworkPolicy, PodDisruptionBudget, security context (read-only root, non-root, drop all capabilities), self-signed webhook cert generation.
- **Enforcement mode** — same binary, activated by config. Available for standalone deployments (header-based identity, per-workload policy binding) and injected sidecars (pod-derived identity, built-in policy catalog via `runtime.getkoshi.ai/mode` and `runtime.getkoshi.ai/policy` annotations). Tiered decisions: allow, throttle, kill.
- **Sidecar config delivery** — namespace-local ConfigMap delivery for arbitrary custom sidecar policy. Operators create a ConfigMap with custom policies in the workload namespace and reference it via `runtime.getkoshi.ai/configmap` and `runtime.getkoshi.ai/policy` pod annotations. Works in both listener and enforcement modes.
- **Design documentation** — formal specifications for [deterministic accounting invariants](docs/design/koshi-v1-deterministic-accounting-invariants.md), [enforcement boundary](docs/design/koshi-v1-enforcement-boundary.md), [operator trust guarantees](docs/design/koshi-v1-operator-trust-guarantees.md), and [why Koshi exists](docs/design/koshi-v1-why-koshi-exists.md).

## Current Product Shape

| Capability | Sidecar (injected) | Standalone deployment |
|---|---|---|
| Listener / posture discovery | **Available** — primary adoption path | Available |
| Configurable policy | Built-in policy catalog via annotation; arbitrary custom policy via namespace-local ConfigMap (`runtime.getkoshi.ai/configmap`) | Full file-based runtime config via `KOSHI_CONFIG_PATH` / ConfigMap |
| Enforcement (live blocking) | **Available** — via `runtime.getkoshi.ai/mode` annotation with built-in policies | **Available** |
| Config delivery | **Available** — namespace-local ConfigMap via `runtime.getkoshi.ai/configmap` annotation | Via `KOSHI_CONFIG_PATH` |

**Today:** sidecar listener audits are the low-risk entry point for governance posture discovery. Sidecar enforcement with built-in policy selection is available as an in-place enforcement path. Namespace-local ConfigMap delivery enables arbitrary custom sidecar policy without switching to standalone. Standalone deployment provides centralized enforcement with full config. See the [README](README.md#from-audit-to-enforcement).

## Next

- Richer provider coverage and policy expressiveness
- Operator-facing Kubernetes integration improvements
- Expanded documentation and evaluation guides
- Optional downstream telemetry integration (e.g., Cribl as a reference track for structured event forwarding)

## Later

- Cross-replica budget coordination (shared state backend)
- Persistent audit trail (budget events to durable store)
- Hot config reload (no pod restart required)
- Multi-policy composition per workload
- Extended identity modes beyond header and pod metadata

## Out of Scope for the Open Runtime

These are excluded by design, not deferred. They define the boundary between the open runtime and commercial layers:

- Hosted control plane or fleet governance coordination
- Advanced policy experimentation, advisory/candidate evaluation, and centralized rollout safety
- Compliance automation layers
- TLS interception or invasive traffic capture
- Design-partner-specific orchestration

Sidecar enforcement and sidecar config delivery are **open runtime** capabilities. Commercial value sits above that line in fleet-wide operations, advanced policy experimentation, and centralized governance coordination.

## Principles

- **One runtime.** Listener and enforcement are modes of the same binary, not separate products.
- **Safe by default.** Kubernetes installation is opt-in per namespace, webhook failure is non-blocking, base-URL injection never clobbers existing env vars.
- **Transparent behavior.** Every decision is observable. Listener mode surfaces the same governance posture enforcement would act on, without affecting traffic.
- **Portable observability.** Structured events and Prometheus metrics work with any log aggregator or monitoring stack. No ecosystem lock-in required.
- **Additive evolution.** New capabilities are added without hiding or replacing core mechanics. The runtime's accounting model, enforcement pipeline, and event schema are stable public surfaces.

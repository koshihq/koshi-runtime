# Koshi Runtime Roadmap

This roadmap describes the intended direction of Koshi Runtime — an open runtime substrate for platform and SRE teams governing AI workloads in Kubernetes. It is directional, not a delivery schedule or commercial commitment.

## What Koshi Runtime Is For

Koshi Runtime is a Kubernetes-native runtime substrate that sits at the workload boundary between application containers and upstream AI providers. It provides:

- **Listener-first adoption.** Start by observing traffic without blocking anything. The full enforcement pipeline runs in shadow mode, emitting structured events and metrics that show exactly what would happen under enforcement.
- **Structured governance signals.** Every request produces a machine-readable decision with a stable reason code — identity resolution, policy lookup, per-request guards, rolling budget accounting, and tiered enforcement.
- **Config-driven mode switching.** The same binary and image supports both listener and enforcement modes. Moving from observation to enforcement is a config change, not a new deployment.
- **Auditable, reversible, bounded behavior.** Reservation-first token accounting with reconciliation. Deterministic enforcement decisions. Safe defaults that fail open on infrastructure and fail closed on policy.

## Available Now

- **Listener (shadow) mode** — full enforcement pipeline runs on every request without blocking traffic. Shadow decisions (`would_reject`, `would_throttle`, `would_kill`) are emitted as structured events.
- **Sidecar injection** — mutating admission webhook injects the koshi-listener sidecar into pods in labeled namespaces. Webhook `failurePolicy: Ignore` ensures pods still create if the injector is down.
- **Structured events and metrics** — JSON events on stdout with `stream: event` for governance decisions, `stream: runtime` for operational logs. Prometheus metrics (`koshi_listener_decisions_total`, `koshi_listener_tokens_total`, `koshi_listener_latency_seconds`) for dashboard and alerting.
- **Reservation-first token accounting** — budget pre-deducted before proxying, reconciled with actual usage after response. Rolling window with optional burst. Budget floor at zero.
- **Stable reason codes** — machine-readable codes on all decisions and error responses: `identity_missing`, `policy_not_found`, `guard_max_tokens`, `budget_exhausted_throttle`, `budget_exhausted_kill`, and others.
- **Helm chart with safe defaults** — NetworkPolicy, PodDisruptionBudget, security context (read-only root, non-root, drop all capabilities), self-signed webhook cert generation.
- **Enforcement mode** — same binary, activated by config. Identity via HTTP header, per-workload policy binding, tiered decisions (allow, throttle, kill).
- **Design documentation** — formal specifications for [deterministic accounting invariants](docs/design/koshi-v1-deterministic-accounting-invariants.md), [enforcement boundary](docs/design/koshi-v1-enforcement-boundary.md), [operator trust guarantees](docs/design/koshi-v1-operator-trust-guarantees.md), and [why Koshi exists](docs/design/koshi-v1-why-koshi-exists.md).

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
- Compliance automation layers
- TLS interception or invasive traffic capture
- Design-partner-specific orchestration

## Principles

- **One runtime.** Listener and enforcement are modes of the same binary, not separate products.
- **Safe by default.** Kubernetes installation is opt-in per namespace, webhook failure is non-blocking, base-URL injection never clobbers existing env vars.
- **Transparent behavior.** Every decision is observable. Shadow mode produces the same signals enforcement would, without affecting traffic.
- **Portable observability.** Structured events and Prometheus metrics work with any log aggregator or monitoring stack. No ecosystem lock-in required.
- **Additive evolution.** New capabilities are added without hiding or replacing core mechanics. The runtime's accounting model, enforcement pipeline, and event schema are stable public surfaces.

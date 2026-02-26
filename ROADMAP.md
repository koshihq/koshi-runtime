# Koshi Roadmap

## Version Definitions

- **v0.1** — Staging-ready, single-replica sidecar. Validates enforcement correctness in a controlled environment. Not production-grade.
- **v1 GA** — Production-grade, single-replica deployment. Observability, provider coverage, operational hardening. No cross-instance coordination. Each replica enforces independently.
- **Post-v1** — Multi-replica coordination, persistence, extended providers. Explicitly deferred — no timeline, no commitment.

## Correctness Model

These invariants hold across all versions:

- **Fail-closed identity:** unknown workloads receive 403 unless `default_policy` is explicitly configured
- **Reservation-first accounting:** budget pre-deducted before proxy, reconciled with actuals after response
- **Deterministic enforcement:** same policy + same budget state produces the same decision, every time
- **No silent pass-through:** requests are either enforced, explicitly defaulted, or in degraded mode — all three are observable via emit events
- **Budget floor at zero:** budget can never go negative; late reconciliation clamps to zero
- **Reserved tokens eventually released:** every reservation is either reconciled (actual usage recorded) or released (upstream 5xx returns reservation). Exception: pod restart loses in-flight reservations

For the complete formal specification of these invariants, see [`docs/design/koshi-v1-deterministic-accounting-invariants.md`](docs/design/koshi-v1-deterministic-accounting-invariants.md).

## v0.1 — Staging-Ready [SHIPPED]

- [x] Non-streaming enforcement pipeline (identity → policy → guard → reserve → decide → proxy → record)
- [x] OpenAI SSE extraction (`stream_options.include_usage` injection, `tokenExtractingReader`)
- [x] Shared `*http.Transport` (connection pooling, TLS reuse)
- [x] Degraded mode with `/healthz` and `/readyz` returning 503
- [x] Panic recovery with `debug.Stack()` capture
- [x] Enriched enforcement events (native int64 JSON via `map[string]any`)
- [x] Per-request guards (`max_tokens_per_request`)
- [x] Rolling window budget with burst (circular buffer, CAS burst consumption)
- [x] Reserve/record reconciliation (negative delta returns tokens)
- [x] Helm chart with liveness/readiness probes
- [x] Structured JSON logging (`slog.JSONHandler`)
- [x] Graceful shutdown (SIGTERM/SIGINT, 30s drain)

## v1 GA — Definition of Done

All must pass. Binary, not subjective.

- [x] All v1 phase checkboxes (Phases 2–5) checked
- [x] `make test-race` passes with zero failures
- [x] No B-grade or higher risks in risk register
- [x] Sustained load validation (>=30 min under budget pressure): `scripts/sustained-load-test.sh`
- [x] Helm chart passes `helm lint` and `helm template` clean
- [x] All known limitations either resolved or documented as accepted
- [x] Operator commitments verified against code

## v1 Phases

### Phase 2: Multi-Workload Correctness [SHIPPED]

*Per-workload budget registration. Each workload registered with its resolved policy's budget params at startup.*

- [x] Per-policy budget parameters (each workload's policy drives its own window/limit/burst)
- [x] Identity key validation (all workloads must share same key in v1; fail-fast at config load)
- [x] Log warning on silently dropped `policy_refs[1:]`
- [x] Integration test: 2 workloads with different policies, numerical token isolation verified

### Phase 3: Observability [SHIPPED]

*`/status` endpoint, `budget_reconciled` events, `phase` attribute, dropped events, Prometheus `/metrics`.*

- [x] `/status` endpoint: per-workload budget state (tokens_used, tokens_limit, burst_remaining)
- [x] `budget_reconciled` event on every reserve-to-record delta (non-streaming, SSE streaming, 5xx refund, zero-delta silence)
- [x] `phase` attribute on `request_allowed`: `"reservation"` (renamed from `accounting_mode`)
- [x] Surface `emit.Dropped()` count in `/status` as `dropped_events`
- [x] Prometheus `/metrics` endpoint (`koshi_requests_total`, `koshi_tokens_used_total`, `koshi_enforcement_decisions_total`, `koshi_enforcement_latency_seconds`, `koshi_emitter_dropped_total`)

### Phase 4: Provider & Transport Hardening [CLOSED]

*Anthropic SSE extraction, transport timeouts, context propagation fix, Google provider rejection.*

- [x] Anthropic SSE extraction — `ParseAnthropicSSEUsage` + `anthropicSSEAccumulator`; generalized `tokenExtractingReader` with pluggable `parseFunc`
- [x] `ResponseHeaderTimeout` on shared Transport — 30s default, configurable via `response_header_timeout` YAML
- [x] Fix `context.Background()` in `handleNonStreamingResponse` — use request context
- [x] Google parser: config-time rejection — `upstreams.google` fails validation at startup

### Phase 5: Operational Hardening [SHIPPED]

- [x] Helm: `securityContext` (readOnlyRootFilesystem, drop ALL, non-root)
- [x] Helm: PodDisruptionBudget template
- [x] Fix Dockerfile Go version mismatch (1.23 → 1.25)
- [x] Fix `os.Exit(1)` in server goroutine — clean shutdown propagation
- [x] `make cover` target
- [x] Health endpoint integration tests (`/healthz`, `/readyz` on mux)

### Operator Commitment Verification

| Commitment | Verification |
|---|---|
| Structured JSON log schema stable | Code review: `slog.JSONHandler` with stable field names |
| 429/503 bodies with `tokens_used`/`tokens_limit` | `TestEnforcementResponse_429_IncludesTokenFields`, `TestEnforcementResponse_503_IncludesTokenFields` |
| `/healthz` `/readyz` 503 when degraded | `TestMux_HealthEndpoints` |
| Budget reconciliation all paths | `TestProxy_BudgetReconciled_NonStreaming`, `TestProxy_Concurrent_StreamingSSE`, `TestTokenExtractingReader_AnthropicSSE` |
| Graceful shutdown 30s drain | `TestGracefulShutdown_InFlightCompletes` |
| Config validation fails fast | 11 `TestValidate_*` functions in `config_test.go` |

## Operator Commitments (v1 GA)

What operators can rely on. Stable across v1.x.

- Structured JSON log schema stable across v1.x (no field renames/removals without version bump)
- 429/503 response bodies include `tokens_used` and `tokens_limit` as native JSON numbers
- `/healthz` and `/readyz` return 503 when degraded
- Budget reconciliation for non-streaming, OpenAI streaming, and Anthropic streaming (SSE extraction enabled)
- Graceful shutdown: in-flight requests complete within 30s on SIGTERM
- Config validation fails fast at startup (no silent misconfig)

For the full operator contract specification, see [`docs/design/koshi-v1-operator-trust-guarantees.md`](docs/design/koshi-v1-operator-trust-guarantees.md).

## Design Artifacts

Core architectural framing documents:

- [`docs/design/koshi-v1-deterministic-accounting-invariants.md`](docs/design/koshi-v1-deterministic-accounting-invariants.md) — formal runtime invariants
- [`docs/design/koshi-v1-enforcement-boundary.md`](docs/design/koshi-v1-enforcement-boundary.md) — enforcement layer model and state ownership
- [`docs/design/koshi-v1-operator-trust-guarantees.md`](docs/design/koshi-v1-operator-trust-guarantees.md) — operator contract surfaces
- [`docs/design/koshi-v1-why-koshi-exists.md`](docs/design/koshi-v1-why-koshi-exists.md) — problem statement and architectural rationale

## Risk Register

| Risk | Sev | Current Mitigation | Resolved By |
|---|---|---|---|
| ~~Anthropic streaming overcount (up to 8x)~~ | ~~B~~ | Anthropic SSE extraction shipped: `ParseAnthropicSSEUsage` + accumulator | **Resolved** |
| In-memory budget state loss on restart | C | Bounded by window duration; fresh budget is safe. Accepted non-goal (v1). | N/A — accepted |
| ~~No metrics endpoint~~ | ~~B~~ | Prometheus `/metrics` endpoint shipped | **Resolved** |
| ~~Single global budget config (all workloads share `Policies[0]`)~~ | ~~B~~ | Per-workload registration with policy-driven BudgetParams | **Resolved** |
| ~~No transport response timeout~~ | ~~B~~ | `ResponseHeaderTimeout` 30s default on shared Transport | **Resolved** |
| ~~Dockerfile Go version mismatch~~ | ~~B~~ | Fixed | **Resolved** |

## Non-Goals (v1)

These are explicitly out of scope. Not deferred — excluded by design.

- Multi-replica budget coordination (no shared state across pods)
- Persistent budget state (no Redis, no database, no checkpointing)
- Hot config reload (pod restart required)
- JWT/mTLS identity resolution (header-only)
- Multiple policy refs per workload (first ref only)
- Model-level routing or per-model enforcement
- Tier 2 sustained-overuse auto-escalation
- Fanout/correlation tracking (`NoOpTracker` stays)

## Post-v1

No dates. No commitments.

- Cross-replica budget coordination (shared state backend)
- Persistent audit trail (budget events to durable store)
- SIGHUP config reload (no restart required)
- Tier 2 escalation (sustained-overuse detection and auto-response)
- Multi-policy composition (multiple `policy_refs` per workload)

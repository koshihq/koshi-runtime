# Koshi and GenOps: Relationship

## One sentence each

- **Koshi Runtime** is the product operators deploy — a Kubernetes sidecar governance plane for AI workloads.
- **GenOps** is the open governance specification that defines the event semantics, required attributes, and interoperability surfaces Koshi implements.

## Why GenOps exists

GenOps provides a shared vocabulary for AI workload governance. When multiple runtimes, observability tools, and compliance systems agree on the same attribute names and event lifecycle, governance data becomes portable — operators can swap tools without rewriting dashboards, alerts, or compliance queries.

Koshi is the reference implementation of the GenOps spec. The spec is developed alongside the runtime but is intended to be implementable by other runtimes independently.

## What Koshi currently implements

Spec version: `0.1.0`

### Structured event attributes

Every governance event emitted by Koshi includes the `genops.spec.version` attribute and uses GenOps-defined attribute names for accounting, policy, and workload metadata:

| Attribute | Example |
|-----------|---------|
| `genops.spec.version` | `"0.1.0"` |
| `genops.accounting.reserved` | `4096` |
| `genops.accounting.actual` | `350` |
| `genops.accounting.unit` | `"tokens"` |
| `genops.policy.result` | `"allow"`, `"throttle"`, `"kill"` |
| `genops.policy.reason_code` | `"guard_max_tokens"`, `"budget_exhausted_throttle"` |
| `genops.operation.name` | `"chat_completion"` |
| `genops.operation.type` | `"inference"` |
| `genops.project` | namespace |
| `genops.team` | owner team |
| `genops.environment` | environment label |

### Required lifecycle events

| Event | When emitted |
|-------|-------------|
| `genops.budget.reservation` | Before proxying — tokens pre-deducted |
| `genops.budget.reconciliation` | After response — actual usage recorded |
| `genops.policy.evaluated` | On every enforcement pipeline evaluation |

### Status endpoint

`GET /status` returns `genops_spec_version: "0.1.0"` alongside runtime diagnostics and budget state.

### Standalone header naming

The default standalone identity header (`x-genops-workload-id`) follows GenOps naming conventions. This is a default — operators can configure a different header key.

## What operators need to know

**Day one:** nothing. Koshi handles GenOps compliance internally. Install the sidecar, collect events, observe shadow decisions — the GenOps layer is invisible.

**When integrating governance telemetry:** the `genops.*` attribute names are stable, spec-defined, and designed for cross-tool portability. Use them as the canonical field names in dashboards, alerts, and compliance queries rather than inventing custom field mappings.

**When evaluating interoperability:** the GenOps spec version on events and `/status` tells you which semantic contract the runtime is honoring. This matters when multiple governance tools or runtimes need to agree on event structure.

## Source of truth

- Spec version and required attributes: [`internal/genops/spec.go`](../../internal/genops/spec.go)
- Spec repository: [github.com/koshihq/genops-spec](https://github.com/koshihq/genops-spec)

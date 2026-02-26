// Package genops declares the GenOps Governance Specification contract
// implemented by this runtime. It is the single source of truth for the
// spec version, required attributes, and required events.
package genops

// SpecVersion is the GenOps specification version implemented by this
// runtime, formatted as SemVer MAJOR.MINOR.PATCH (Section 11.3).
const SpecVersion = "0.1.0"

// RequiredAttributes lists the attribute names that a GenOps v0.1.0
// compliant runtime MUST emit on every AWU (Section 7.1).
// Sorted alphabetically for stable diffs.
var RequiredAttributes = [...]string{
	"genops.accounting.actual",
	"genops.accounting.reserved",
	"genops.accounting.unit",
	"genops.environment",
	"genops.operation.name",
	"genops.operation.type",
	"genops.policy.reason_code",
	"genops.policy.result",
	"genops.project",
	"genops.spec.version",
	"genops.team",
}

// RequiredEvents lists the event names that a GenOps v0.1.0 compliant
// runtime MUST emit at the specified lifecycle points (Section 7.2).
// Sorted alphabetically for stable diffs.
var RequiredEvents = [...]string{
	"genops.budget.reconciliation",
	"genops.budget.reservation",
	"genops.policy.evaluated",
}

package enforce

import "time"

// Action represents an enforcement action.
type Action int

const (
	ActionAllow    Action = iota // Allow the request
	ActionThrottle               // Tier 1: return 429
	ActionKill                   // Tier 3: return 503
)

// Stable reason codes for policy outcomes. These codes appear in enforcement
// events, shadow listener events, and JSON error bodies.
const (
	ReasonIdentityMissing         = "identity_missing"
	ReasonPolicyNotFound          = "policy_not_found"
	ReasonGuardMaxTokens          = "guard_max_tokens"
	ReasonBudgetExhaustedThrottle = "budget_exhausted_throttle"
	ReasonBudgetExhaustedKill     = "budget_exhausted_kill"
	ReasonUpstreamNotConfigured   = "upstream_not_configured"
	ReasonUpstreamTimeout         = "upstream_timeout"
	ReasonSystemDegraded          = "system_degraded"
	ReasonBudgetConfigError       = "budget_config_error"
)

// Decision represents the result of enforcement evaluation.
type Decision struct {
	Action      Action
	Tier        int
	Reason      string
	ReasonCode  string
	RetryAfter  time.Duration
	TokensUsed  int64
	TokensLimit int64
}

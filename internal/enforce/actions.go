package enforce

import "time"

// Action represents an enforcement action.
type Action int

const (
	ActionAllow    Action = iota // Allow the request
	ActionThrottle               // Tier 1: return 429
	ActionKill                   // Tier 3: return 503
)

// Decision represents the result of enforcement evaluation.
type Decision struct {
	Action      Action
	Tier        int
	Reason      string
	RetryAfter  time.Duration
	TokensUsed  int64
	TokensLimit int64
}

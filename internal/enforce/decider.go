package enforce

import (
	"time"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/fanout"
)

// Decider evaluates enforcement decisions based on policy, budget, and fanout status.
type Decider interface {
	Evaluate(policy *config.Policy, budgetStatus budget.BudgetStatus, fanoutStatus *fanout.FanoutStatus) Decision
}

// TierDecider implements simplified tier logic for v1.
type TierDecider struct{}

// NewTierDecider creates a new TierDecider.
func NewTierDecider() *TierDecider {
	return &TierDecider{}
}

// Evaluate applies tier logic:
// - Within limit → Allow
// - At limit, burst remaining → Allow (burst consumed by tracker)
// - At limit, no burst → Throttle (429)
// - Kill only if explicitly configured in tier3_platform
func (d *TierDecider) Evaluate(policy *config.Policy, bs budget.BudgetStatus, _ *fanout.FanoutStatus) Decision {
	// Per-request guard is checked separately before calling Evaluate.
	// Here we only handle budget-level decisions.

	if bs.WindowTokensUsed <= bs.WindowTokensLimit {
		return Decision{Action: ActionAllow}
	}

	// Over limit — check if burst was consumed (tracker handles this).
	// If BurstRemaining > 0, the tracker already consumed burst and allowed the reservation.
	// This path is hit when the tracker denied the reservation.

	// Check for explicit kill configuration.
	if policy.DecisionTiers.Tier3Platform.Action == "kill_workload" {
		return Decision{
			Action: ActionKill,
			Tier:   3,
			Reason: "budget exceeded, kill_workload configured",
		}
	}

	retryAfter := time.Duration(policy.Budgets.RollingTokens.WindowSeconds/2) * time.Second

	return Decision{
		Action:     ActionThrottle,
		Tier:       1,
		Reason:     "budget exceeded, no burst remaining",
		RetryAfter: retryAfter,
	}
}

// CheckPerRequestGuard checks if a request exceeds the per-request token limit.
// Returns a Throttle decision if exceeded, nil if within guard.
func CheckPerRequestGuard(policy *config.Policy, requestMaxTokens int64) *Decision {
	if policy.Guards.MaxTokensPerRequest <= 0 {
		return nil // Guard not configured
	}
	if requestMaxTokens <= policy.Guards.MaxTokensPerRequest {
		return nil
	}
	return &Decision{
		Action: ActionThrottle,
		Tier:   1,
		Reason: "max_tokens exceeds per-request guard",
	}
}

package enforce

import (
	"testing"
	"time"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/config"
)

func makePolicy(windowSeconds int, killConfigured bool) *config.Policy {
	p := &config.Policy{
		ID: "test",
		Budgets: config.Budgets{
			RollingTokens: config.RollingTokenBudget{
				WindowSeconds: windowSeconds,
				LimitTokens:   10000,
				BurstTokens:   1000,
			},
		},
		Guards: config.Guards{
			MaxTokensPerRequest: 4096,
		},
		DecisionTiers: config.DecisionTiers{
			Tier1Auto: config.TierAction{Action: "throttle"},
		},
	}
	if killConfigured {
		p.DecisionTiers.Tier3Platform = config.TierAction{Action: "kill_workload"}
	}
	return p
}

func TestDecider_AllowBelowLimit(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  5000,
		WindowTokensLimit: 10000,
		BurstRemaining:    1000,
	}
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionAllow {
		t.Errorf("expected Allow, got %d", dec.Action)
	}
}

func TestDecider_AllowAtExactLimit(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  10000,
		WindowTokensLimit: 10000,
		BurstRemaining:    1000,
	}
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionAllow {
		t.Errorf("expected Allow at exact limit, got %d", dec.Action)
	}
}

func TestDecider_ThrottleOverLimit_NoBurst(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  10001,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionThrottle {
		t.Errorf("expected Throttle, got %d", dec.Action)
	}
	if dec.Tier != 1 {
		t.Errorf("expected Tier 1, got %d", dec.Tier)
	}
	expectedRetry := 150 * time.Second // 300/2
	if dec.RetryAfter != expectedRetry {
		t.Errorf("expected RetryAfter %v, got %v", expectedRetry, dec.RetryAfter)
	}
}

func TestDecider_KillOnlyIfConfigured(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  20000,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}

	// Without kill configured → throttle
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionThrottle {
		t.Errorf("expected Throttle without kill config, got %d", dec.Action)
	}

	// With kill configured → kill
	dec = d.Evaluate(makePolicy(300, true), bs, nil)
	if dec.Action != ActionKill {
		t.Errorf("expected Kill with kill config, got %d", dec.Action)
	}
	if dec.Tier != 3 {
		t.Errorf("expected Tier 3, got %d", dec.Tier)
	}
}

func TestDecider_RetryAfterValue(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  15000,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}

	// window_seconds=60 → Retry-After=30s
	dec := d.Evaluate(makePolicy(60, false), bs, nil)
	if dec.RetryAfter != 30*time.Second {
		t.Errorf("expected 30s, got %v", dec.RetryAfter)
	}

	// window_seconds=300 → Retry-After=150s
	dec = d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.RetryAfter != 150*time.Second {
		t.Errorf("expected 150s, got %v", dec.RetryAfter)
	}
}

func TestDecider_Determinism(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  15000,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}
	p := makePolicy(300, false)

	var first Decision
	for i := 0; i < 1000; i++ {
		dec := d.Evaluate(p, bs, nil)
		if i == 0 {
			first = dec
			continue
		}
		if dec.Action != first.Action || dec.Tier != first.Tier || dec.RetryAfter != first.RetryAfter {
			t.Fatalf("non-deterministic decision at iteration %d: %+v vs %+v", i, dec, first)
		}
	}
}

func TestDecider_BoundaryPrecision(t *testing.T) {
	d := NewTierDecider()

	// One below limit → Allow
	bs := budget.BudgetStatus{
		WindowTokensUsed:  9999,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionAllow {
		t.Errorf("expected Allow at limit-1, got %d", dec.Action)
	}

	// Exactly at limit → Allow
	bs.WindowTokensUsed = 10000
	dec = d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionAllow {
		t.Errorf("expected Allow at exact limit, got %d", dec.Action)
	}

	// One over limit → Throttle
	bs.WindowTokensUsed = 10001
	dec = d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.Action != ActionThrottle {
		t.Errorf("expected Throttle at limit+1, got %d", dec.Action)
	}
}

func TestCheckPerRequestGuard_WithinGuard(t *testing.T) {
	p := makePolicy(300, false)
	dec := CheckPerRequestGuard(p, 4096)
	if dec != nil {
		t.Errorf("expected nil for request within guard, got %+v", dec)
	}
}

func TestCheckPerRequestGuard_ExceedsGuard(t *testing.T) {
	p := makePolicy(300, false)
	dec := CheckPerRequestGuard(p, 4097)
	if dec == nil {
		t.Fatal("expected decision for request exceeding guard")
	}
	if dec.Action != ActionThrottle {
		t.Errorf("expected Throttle, got %d", dec.Action)
	}
}

func TestCheckPerRequestGuard_GuardNotConfigured(t *testing.T) {
	p := makePolicy(300, false)
	p.Guards.MaxTokensPerRequest = 0
	dec := CheckPerRequestGuard(p, 100000)
	if dec != nil {
		t.Errorf("expected nil when guard not configured, got %+v", dec)
	}
}

// --- ReasonCode tests ---

func TestDecider_ReasonCode_Throttle(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  15000,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.ReasonCode != ReasonBudgetExhaustedThrottle {
		t.Errorf("expected reason_code %q, got %q", ReasonBudgetExhaustedThrottle, dec.ReasonCode)
	}
}

func TestDecider_ReasonCode_Kill(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  15000,
		WindowTokensLimit: 10000,
		BurstRemaining:    0,
	}
	dec := d.Evaluate(makePolicy(300, true), bs, nil)
	if dec.ReasonCode != ReasonBudgetExhaustedKill {
		t.Errorf("expected reason_code %q, got %q", ReasonBudgetExhaustedKill, dec.ReasonCode)
	}
}

func TestDecider_ReasonCode_Allow_Empty(t *testing.T) {
	d := NewTierDecider()
	bs := budget.BudgetStatus{
		WindowTokensUsed:  5000,
		WindowTokensLimit: 10000,
		BurstRemaining:    1000,
	}
	dec := d.Evaluate(makePolicy(300, false), bs, nil)
	if dec.ReasonCode != "" {
		t.Errorf("expected empty reason_code for Allow, got %q", dec.ReasonCode)
	}
}

func TestCheckPerRequestGuard_ReasonCode(t *testing.T) {
	p := makePolicy(300, false)
	dec := CheckPerRequestGuard(p, 5000)
	if dec == nil {
		t.Fatal("expected decision for exceeding guard")
	}
	if dec.ReasonCode != ReasonGuardMaxTokens {
		t.Errorf("expected reason_code %q, got %q", ReasonGuardMaxTokens, dec.ReasonCode)
	}
}

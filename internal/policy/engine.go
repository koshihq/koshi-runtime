package policy

import (
	"log/slog"

	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/identity"
)

// Engine resolves a policy for a given workload identity.
type Engine interface {
	Lookup(id identity.WorkloadIdentity) (*config.Policy, bool)
}

// MapEngine is an in-memory policy engine built from config at startup.
type MapEngine struct {
	policies      map[string]*config.Policy // workload ID → resolved policy
	defaultPolicy *config.Policy
	strictMode    bool
}

// NewMapEngine builds a MapEngine from the provided config.
// It resolves policy_refs for each workload into a single effective policy
// (v1: uses the first policy ref only).
func NewMapEngine(cfg *config.Config, logger *slog.Logger) *MapEngine {
	// Build policy lookup by ID.
	policyByID := make(map[string]*config.Policy, len(cfg.Policies))
	for i := range cfg.Policies {
		policyByID[cfg.Policies[i].ID] = &cfg.Policies[i]
	}

	// Resolve workload → policy mapping.
	policies := make(map[string]*config.Policy, len(cfg.Workloads))
	for _, w := range cfg.Workloads {
		if len(w.PolicyRefs) > 0 {
			if p, ok := policyByID[w.PolicyRefs[0]]; ok {
				policies[w.ID] = p
			}
		}
		if len(w.PolicyRefs) > 1 {
			logger.Warn("workload has multiple policy_refs, only first is used",
				"workload_id", w.ID,
				"first_policy", w.PolicyRefs[0],
				"dropped_refs", w.PolicyRefs[1:],
				"ref_count", len(w.PolicyRefs),
			)
		}
	}

	return &MapEngine{
		policies:      policies,
		defaultPolicy: cfg.DefaultPolicy,
		strictMode:    cfg.StrictMode,
	}
}

// Lookup returns the policy for the given workload identity.
// If the workload is unknown, it falls back to the default policy unless
// strict mode is enabled.
func (e *MapEngine) Lookup(id identity.WorkloadIdentity) (*config.Policy, bool) {
	if p, ok := e.policies[id.WorkloadID]; ok {
		return p, true
	}

	if e.strictMode {
		return nil, false
	}

	if e.defaultPolicy != nil {
		return e.defaultPolicy, true
	}

	return nil, false
}

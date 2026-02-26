package policy

import (
	"io"
	"log/slog"
	"testing"

	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/identity"
)

var discardLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

func makeConfig(defaultPolicy *config.Policy, strictMode bool) *config.Config {
	return &config.Config{
		Upstreams: map[string]string{"openai": "https://api.openai.com"},
		Workloads: []config.Workload{
			{
				ID:         "svc-a",
				Identity:   config.Identity{Mode: "header", Key: "x-genops-workload-id"},
				PolicyRefs: []string{"standard"},
			},
		},
		Policies: []config.Policy{
			{
				ID: "standard",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 300,
						LimitTokens:   100000,
						BurstTokens:   5000,
					},
				},
			},
		},
		DefaultPolicy: defaultPolicy,
		StrictMode:    strictMode,
	}
}

func TestMapEngine_KnownWorkload(t *testing.T) {
	e := NewMapEngine(makeConfig(nil, false), discardLogger)
	p, ok := e.Lookup(identity.WorkloadIdentity{WorkloadID: "svc-a"})
	if !ok {
		t.Fatal("expected policy found for known workload")
	}
	if p.ID != "standard" {
		t.Errorf("expected policy ID standard, got %s", p.ID)
	}
}

func TestMapEngine_UnknownWorkload_NoDefault(t *testing.T) {
	e := NewMapEngine(makeConfig(nil, false), discardLogger)
	_, ok := e.Lookup(identity.WorkloadIdentity{WorkloadID: "unknown"})
	if ok {
		t.Fatal("expected no policy for unknown workload without default")
	}
}

func TestMapEngine_UnknownWorkload_WithDefault(t *testing.T) {
	dp := &config.Policy{
		ID: "_default",
		Budgets: config.Budgets{
			RollingTokens: config.RollingTokenBudget{
				WindowSeconds: 60,
				LimitTokens:   1000,
				BurstTokens:   0,
			},
		},
	}
	e := NewMapEngine(makeConfig(dp, false), discardLogger)
	p, ok := e.Lookup(identity.WorkloadIdentity{WorkloadID: "unknown"})
	if !ok {
		t.Fatal("expected default policy for unknown workload")
	}
	if p.ID != "_default" {
		t.Errorf("expected _default policy, got %s", p.ID)
	}
}

func TestMapEngine_UnknownWorkload_StrictMode(t *testing.T) {
	dp := &config.Policy{
		ID: "_default",
		Budgets: config.Budgets{
			RollingTokens: config.RollingTokenBudget{
				WindowSeconds: 60,
				LimitTokens:   1000,
				BurstTokens:   0,
			},
		},
	}
	e := NewMapEngine(makeConfig(dp, true), discardLogger)
	_, ok := e.Lookup(identity.WorkloadIdentity{WorkloadID: "unknown"})
	if ok {
		t.Fatal("expected no policy in strict mode even with default")
	}
}

func TestMapEngine_KnownWorkload_StrictMode(t *testing.T) {
	e := NewMapEngine(makeConfig(nil, true), discardLogger)
	p, ok := e.Lookup(identity.WorkloadIdentity{WorkloadID: "svc-a"})
	if !ok {
		t.Fatal("strict mode should not affect known workloads")
	}
	if p.ID != "standard" {
		t.Errorf("expected standard, got %s", p.ID)
	}
}

func TestMapEngine_MultipleRefs_UsesFirst(t *testing.T) {
	cfg := &config.Config{
		Upstreams: map[string]string{"openai": "https://api.openai.com"},
		Workloads: []config.Workload{
			{
				ID:         "svc-multi",
				Identity:   config.Identity{Mode: "header", Key: "x-genops-workload-id"},
				PolicyRefs: []string{"standard", "premium"},
			},
		},
		Policies: []config.Policy{
			{
				ID: "standard",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 60,
						LimitTokens:   10000,
						BurstTokens:   0,
					},
				},
			},
			{
				ID: "premium",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 60,
						LimitTokens:   500000,
						BurstTokens:   10000,
					},
				},
			},
		},
	}

	e := NewMapEngine(cfg, discardLogger)
	p, ok := e.Lookup(identity.WorkloadIdentity{WorkloadID: "svc-multi"})
	if !ok {
		t.Fatal("expected policy for multi-ref workload")
	}
	if p.ID != "standard" {
		t.Errorf("expected first ref (standard), got %s", p.ID)
	}
}

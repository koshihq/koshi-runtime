package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level Koshi configuration.
type Config struct {
	Upstreams      map[string]string `yaml:"upstreams"`
	Workloads      []Workload        `yaml:"workloads"`
	Policies       []Policy          `yaml:"policies"`
	DefaultPolicy  *Policy           `yaml:"default_policy,omitempty"`
	StrictMode     bool              `yaml:"strict_mode"`
	SSEExtraction          *bool             `yaml:"sse_extraction,omitempty"`
	ResponseHeaderTimeout  int               `yaml:"response_header_timeout,omitempty"`
	ListenAddr             string            `yaml:"listen_addr,omitempty"`
}

type Workload struct {
	ID           string        `yaml:"id"`
	Type         string        `yaml:"type"`
	OwnerTeam    string        `yaml:"owner_team"`
	Environment  string        `yaml:"environment"`
	Identity     Identity      `yaml:"identity"`
	ModelTargets []ModelTarget `yaml:"model_targets"`
	PolicyRefs   []string      `yaml:"policy_refs"`
}

type Identity struct {
	Mode string `yaml:"mode"` // "header"
	Key  string `yaml:"key"`  // e.g. "x-genops-workload-id"
}

type ModelTarget struct {
	Provider string `yaml:"provider"` // "openai", "anthropic", "google"
	Model    string `yaml:"model"`
}

type Policy struct {
	ID            string        `yaml:"id"`
	Budgets       Budgets       `yaml:"budgets"`
	Guards        Guards        `yaml:"guards"`
	DecisionTiers DecisionTiers `yaml:"decision_tiers"`
}

type Budgets struct {
	RollingTokens RollingTokenBudget `yaml:"rolling_tokens"`
}

type RollingTokenBudget struct {
	WindowSeconds int   `yaml:"window_seconds"`
	LimitTokens   int64 `yaml:"limit_tokens"`
	BurstTokens   int64 `yaml:"burst_tokens"`
}

type Guards struct {
	MaxTokensPerRequest int64 `yaml:"max_tokens_per_request"`
}

type DecisionTiers struct {
	Tier1Auto     TierAction `yaml:"tier1_auto"`
	Tier3Platform TierAction `yaml:"tier3_platform"`
}

type TierAction struct {
	Action string `yaml:"action"` // "throttle", "kill_workload"
}

// Load reads and parses a YAML config file, then validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate: %w", err)
	}

	return &cfg, nil
}

// Validate checks all configuration constraints. Returns an error describing
// the first invalid field found.
func (c *Config) Validate() error {
	if len(c.Upstreams) == 0 {
		return errors.New("upstreams: at least one upstream must be configured")
	}

	// Validate upstreams have non-empty URLs and reject unsupported providers.
	for name, url := range c.Upstreams {
		if url == "" {
			return fmt.Errorf("upstreams.%s: URL must not be empty", name)
		}
		if name == "google" {
			return fmt.Errorf("upstreams.google: Google provider is not supported in v1; remove this upstream")
		}
	}

	// Build policy ID set for ref validation.
	policyIDs := make(map[string]struct{}, len(c.Policies))
	for _, p := range c.Policies {
		if p.ID == "" {
			return errors.New("policies: policy ID must not be empty")
		}
		if _, dup := policyIDs[p.ID]; dup {
			return fmt.Errorf("policies: duplicate policy ID %q", p.ID)
		}
		policyIDs[p.ID] = struct{}{}

		if err := validatePolicy(&p); err != nil {
			return fmt.Errorf("policies.%s: %w", p.ID, err)
		}
	}

	// Validate default policy if present.
	if c.DefaultPolicy != nil {
		if err := validatePolicy(c.DefaultPolicy); err != nil {
			return fmt.Errorf("default_policy: %w", err)
		}
	}

	// Validate workloads.
	workloadIDs := make(map[string]struct{}, len(c.Workloads))
	for _, w := range c.Workloads {
		if w.ID == "" {
			return errors.New("workloads: workload ID must not be empty")
		}
		if _, dup := workloadIDs[w.ID]; dup {
			return fmt.Errorf("workloads: duplicate workload ID %q", w.ID)
		}
		workloadIDs[w.ID] = struct{}{}

		if w.Identity.Mode != "header" {
			return fmt.Errorf("workloads.%s.identity.mode: must be \"header\", got %q", w.ID, w.Identity.Mode)
		}
		if w.Identity.Key == "" {
			return fmt.Errorf("workloads.%s.identity.key: must not be empty", w.ID)
		}

		for _, ref := range w.PolicyRefs {
			if _, ok := policyIDs[ref]; !ok {
				return fmt.Errorf("workloads.%s.policy_refs: unknown policy %q", w.ID, ref)
			}
		}
		if len(w.PolicyRefs) == 0 {
			return fmt.Errorf("workloads.%s.policy_refs: at least one policy ref required", w.ID)
		}
	}

	// v1: all workloads must share the same identity key.
	if len(c.Workloads) > 1 {
		firstKey := c.Workloads[0].Identity.Key
		for _, w := range c.Workloads[1:] {
			if w.Identity.Key != firstKey {
				return fmt.Errorf(
					"workloads: all identity keys must match in v1, "+
						"workload %q uses %q but %q uses %q",
					c.Workloads[0].ID, firstKey, w.ID, w.Identity.Key,
				)
			}
		}
	}

	return nil
}

func validatePolicy(p *Policy) error {
	b := p.Budgets.RollingTokens
	if b.WindowSeconds <= 0 {
		return fmt.Errorf("budgets.rolling_tokens.window_seconds: must be positive, got %d", b.WindowSeconds)
	}
	if b.LimitTokens <= 0 {
		return fmt.Errorf("budgets.rolling_tokens.limit_tokens: must be positive, got %d", b.LimitTokens)
	}
	if b.BurstTokens < 0 {
		return fmt.Errorf("budgets.rolling_tokens.burst_tokens: must be non-negative, got %d", b.BurstTokens)
	}
	if p.Guards.MaxTokensPerRequest < 0 {
		return fmt.Errorf("guards.max_tokens_per_request: must be non-negative, got %d", p.Guards.MaxTokensPerRequest)
	}
	return nil
}

// SSEExtractionEnabled returns whether SSE extraction is enabled. Defaults to true.
func (c *Config) SSEExtractionEnabled() bool {
	if c.SSEExtraction == nil {
		return true
	}
	return *c.SSEExtraction
}

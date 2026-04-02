package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Mode configures the runtime operating mode.
type Mode struct {
	Type string `yaml:"type"` // "listener" or "enforcement"; empty defaults to "enforcement"
}

// Config is the top-level Koshi configuration.
type Config struct {
	Mode           Mode              `yaml:"mode,omitempty"`
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
	Mode string `yaml:"mode"` // "header" or "pod"
	Key  string `yaml:"key"`  // e.g. "x-genops-workload-id" (required for header mode)
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

// Parse reads and unmarshals a YAML config file without validation.
// Use Load() for standalone configs (parse + Validate) or call
// ValidateSidecarConfig() for sidecar custom configs.
func Parse(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}

	return &cfg, nil
}

// Load reads and parses a YAML config file, then validates it for standalone use.
func Load(path string) (*Config, error) {
	cfg, err := Parse(path)
	if err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate: %w", err)
	}

	return cfg, nil
}

// Validate checks all configuration constraints. Returns an error describing
// the first invalid field found.
func (c *Config) Validate() error {
	// Normalize mode: default to "enforcement" for backward compatibility.
	if c.Mode.Type == "" {
		c.Mode.Type = "enforcement"
	}
	if c.Mode.Type != "listener" && c.Mode.Type != "enforcement" {
		return fmt.Errorf("mode.type: must be \"listener\" or \"enforcement\", got %q", c.Mode.Type)
	}

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

	// In listener mode, workloads are optional if default_policy is set.
	if c.Mode.Type == "listener" && len(c.Workloads) == 0 {
		if c.DefaultPolicy == nil {
			return errors.New("listener mode requires a default_policy when no workloads are defined")
		}
		// Default listen address for listener mode.
		if c.ListenAddr == "" {
			c.ListenAddr = ":15080"
		}
		return nil
	}

	// Validate workloads.
	workloadIDs := make(map[string]struct{}, len(c.Workloads))
	var headerWorkloads []Workload
	for _, w := range c.Workloads {
		if w.ID == "" {
			return errors.New("workloads: workload ID must not be empty")
		}
		if _, dup := workloadIDs[w.ID]; dup {
			return fmt.Errorf("workloads: duplicate workload ID %q", w.ID)
		}
		workloadIDs[w.ID] = struct{}{}

		if w.Identity.Mode != "header" && w.Identity.Mode != "pod" {
			return fmt.Errorf("workloads.%s.identity.mode: must be \"header\" or \"pod\", got %q", w.ID, w.Identity.Mode)
		}
		if w.Identity.Mode == "header" {
			if w.Identity.Key == "" {
				return fmt.Errorf("workloads.%s.identity.key: must not be empty for header mode", w.ID)
			}
			headerWorkloads = append(headerWorkloads, w)
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

	// v1: all header-mode workloads must share the same identity key.
	if len(headerWorkloads) > 1 {
		firstKey := headerWorkloads[0].Identity.Key
		for _, w := range headerWorkloads[1:] {
			if w.Identity.Key != firstKey {
				return fmt.Errorf(
					"workloads: all identity keys must match in v1, "+
						"workload %q uses %q but %q uses %q",
					headerWorkloads[0].ID, firstKey, w.ID, w.Identity.Key,
				)
			}
		}
	}

	// Default listen address for listener mode.
	if c.Mode.Type == "listener" && c.ListenAddr == "" {
		c.ListenAddr = ":15080"
	}

	return nil
}

// ValidateSidecarConfig checks configuration constraints specific to sidecar
// custom configs delivered via namespace-local ConfigMap. Sidecar configs provide
// named policies only; the runtime selects one via KOSHI_POLICY_OVERRIDE and
// synthesizes a workload at startup. No default_policy or workloads needed.
func (c *Config) ValidateSidecarConfig() error {
	if c.Mode.Type != "" && c.Mode.Type != "listener" && c.Mode.Type != "enforcement" {
		return fmt.Errorf("mode.type: must be \"listener\" or \"enforcement\", got %q", c.Mode.Type)
	}

	if len(c.Upstreams) == 0 {
		return errors.New("upstreams: at least one upstream must be configured")
	}
	for name, url := range c.Upstreams {
		if url == "" {
			return fmt.Errorf("upstreams.%s: URL must not be empty", name)
		}
		if name == "google" {
			return fmt.Errorf("upstreams.google: Google provider is not supported in v1; remove this upstream")
		}
	}

	if len(c.Policies) == 0 {
		return errors.New("policies: at least one policy must be defined")
	}

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

	if len(c.Workloads) > 0 {
		return errors.New("workloads must not be defined in sidecar config — workloads are synthesized at startup from pod identity")
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

// SidecarPolicyCatalog returns the built-in sidecar policy catalog. These
// policies are available for selection via the runtime.getkoshi.ai/policy
// pod annotation in both listener and enforcement sidecar modes.
func SidecarPolicyCatalog() []Policy {
	return []Policy{
		{
			ID: "sidecar-baseline",
			Budgets: Budgets{
				RollingTokens: RollingTokenBudget{
					WindowSeconds: 3600,
					LimitTokens:   100000,
					BurstTokens:   10000,
				},
			},
			Guards: Guards{
				MaxTokensPerRequest: 4096,
			},
			DecisionTiers: DecisionTiers{
				Tier1Auto:     TierAction{Action: "throttle"},
				Tier3Platform: TierAction{Action: "kill_workload"},
			},
		},
		{
			ID: "sidecar-strict",
			Budgets: Budgets{
				RollingTokens: RollingTokenBudget{
					WindowSeconds: 3600,
					LimitTokens:   25000,
					BurstTokens:   2500,
				},
			},
			Guards: Guards{
				MaxTokensPerRequest: 2048,
			},
			DecisionTiers: DecisionTiers{
				Tier1Auto:     TierAction{Action: "throttle"},
				Tier3Platform: TierAction{Action: "kill_workload"},
			},
		},
		{
			ID: "sidecar-high-throughput",
			Budgets: Budgets{
				RollingTokens: RollingTokenBudget{
					WindowSeconds: 3600,
					LimitTokens:   500000,
					BurstTokens:   50000,
				},
			},
			Guards: Guards{
				MaxTokensPerRequest: 32768,
			},
			DecisionTiers: DecisionTiers{
				Tier1Auto: TierAction{Action: "throttle"},
			},
		},
	}
}

// DefaultSidecarPolicyID is the policy used when no runtime.getkoshi.ai/policy
// annotation is set on the pod.
const DefaultSidecarPolicyID = "sidecar-baseline"

// DefaultListenerConfig returns the standard in-memory listener config used by
// injected sidecars that have no KOSHI_CONFIG_PATH set. Includes the built-in
// sidecar policy catalog so KOSHI_POLICY_OVERRIDE can select a named policy.
func DefaultListenerConfig() *Config {
	return &Config{
		Mode:       Mode{Type: "listener"},
		ListenAddr: ":15080",
		Upstreams: map[string]string{
			"openai":    "https://api.openai.com",
			"anthropic": "https://api.anthropic.com",
		},
		Policies: SidecarPolicyCatalog(),
		DefaultPolicy: &Policy{
			ID: "_listener_default",
			Budgets: Budgets{
				RollingTokens: RollingTokenBudget{
					WindowSeconds: 3600,
					LimitTokens:   1000000,
					BurstTokens:   0,
				},
			},
			Guards: Guards{
				MaxTokensPerRequest: 32768,
			},
			DecisionTiers: DecisionTiers{
				Tier1Auto: TierAction{Action: "throttle"},
			},
		},
	}
}

// DefaultEnforcementSidecarConfig returns the built-in enforcement config used
// by injected sidecars running in enforcement mode (KOSHI_MODE=enforcement).
// It includes the full sidecar policy catalog; the single workload entry is
// synthesized at startup from pod identity env vars and attached to a selected
// (or default) policy before building the policy engine.
func DefaultEnforcementSidecarConfig() *Config {
	return &Config{
		Mode:       Mode{Type: "enforcement"},
		ListenAddr: ":15080",
		Upstreams: map[string]string{
			"openai":    "https://api.openai.com",
			"anthropic": "https://api.anthropic.com",
		},
		Policies: SidecarPolicyCatalog(),
	}
}

// SSEExtractionEnabled returns whether SSE extraction is enabled. Defaults to true.
func (c *Config) SSEExtractionEnabled() bool {
	if c.SSEExtraction == nil {
		return true
	}
	return *c.SSEExtraction
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

const validConfig = `
upstreams:
  openai: "https://api.openai.com"
  anthropic: "https://api.anthropic.com"

workloads:
  - id: "svc-a"
    type: "agent"
    owner_team: "ml-team"
    environment: "production"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    model_targets:
      - provider: "openai"
        model: "gpt-4"
    policy_refs:
      - "standard"

policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 300
        limit_tokens: 100000
        burst_tokens: 5000
    guards:
      max_tokens_per_request: 4096
    decision_tiers:
      tier1_auto:
        action: "throttle"
      tier3_platform:
        action: "kill_workload"
`

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTemp(t, validConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(cfg.Upstreams) != 2 {
		t.Errorf("expected 2 upstreams, got %d", len(cfg.Upstreams))
	}
	if cfg.Upstreams["openai"] != "https://api.openai.com" {
		t.Errorf("unexpected openai upstream: %s", cfg.Upstreams["openai"])
	}
	if len(cfg.Workloads) != 1 {
		t.Errorf("expected 1 workload, got %d", len(cfg.Workloads))
	}
	if cfg.Workloads[0].ID != "svc-a" {
		t.Errorf("expected workload ID svc-a, got %s", cfg.Workloads[0].ID)
	}
	if len(cfg.Policies) != 1 {
		t.Errorf("expected 1 policy, got %d", len(cfg.Policies))
	}
	if cfg.Policies[0].Budgets.RollingTokens.LimitTokens != 100000 {
		t.Errorf("expected limit 100000, got %d", cfg.Policies[0].Budgets.RollingTokens.LimitTokens)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTemp(t, "{{{{not yaml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestValidate_MissingUpstreams(t *testing.T) {
	path := writeTemp(t, `
workloads: []
policies: []
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing upstreams")
	}
}

func TestValidate_EmptyUpstreamURL(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: ""
workloads: []
policies: []
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty upstream URL")
	}
}

func TestValidate_ZeroWindowSeconds(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads: []
policies:
  - id: "bad"
    budgets:
      rolling_tokens:
        window_seconds: 0
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for zero window_seconds")
	}
}

func TestValidate_NegativeLimitTokens(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads: []
policies:
  - id: "bad"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: -100
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative limit_tokens")
	}
}

func TestValidate_NegativeBurstTokens(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads: []
policies:
  - id: "bad"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: -5
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative burst_tokens")
	}
}

func TestValidate_DanglingPolicyRef(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "nonexistent"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for dangling policy ref")
	}
}

func TestValidate_InvalidIdentityMode(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "magic"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid identity mode")
	}
}

func TestValidate_DuplicateWorkloadID(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate workload ID")
	}
}

func TestValidate_DuplicatePolicyID(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads: []
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 2000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate policy ID")
	}
}

func TestValidate_MissingPolicyRefs(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs: []
policies: []
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty policy_refs")
	}
}

func TestValidate_DefaultPolicy(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads: []
policies: []
default_policy:
  id: "_default"
  budgets:
    rolling_tokens:
      window_seconds: 60
      limit_tokens: 1000
      burst_tokens: 0
  guards:
    max_tokens_per_request: 1024
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.DefaultPolicy == nil {
		t.Fatal("expected default_policy to be set")
	}
	if cfg.DefaultPolicy.Budgets.RollingTokens.LimitTokens != 1000 {
		t.Errorf("expected default policy limit 1000, got %d", cfg.DefaultPolicy.Budgets.RollingTokens.LimitTokens)
	}
}

func TestValidate_DefaultPolicy_Invalid(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads: []
policies: []
default_policy:
  id: "_default"
  budgets:
    rolling_tokens:
      window_seconds: -1
      limit_tokens: 1000
      burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid default_policy")
	}
}

func TestValidate_MismatchedIdentityKeys(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
  - id: "svc-b"
    identity:
      mode: "header"
      key: "x-other-header"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for mismatched identity keys")
	}
}

func TestValidate_MatchingIdentityKeys(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
  - id: "svc-b"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("expected no error for matching identity keys, got: %v", err)
	}
}

func TestValidate_GoogleUpstreamRejected(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
  google: "https://generativelanguage.googleapis.com"
workloads: []
policies: []
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for google upstream")
	}
	if got := err.Error(); !strings.Contains(got, "not supported in v1") {
		t.Errorf("expected error to mention 'not supported in v1', got: %s", got)
	}
}

// --- Mode tests ---

func TestValidate_ModeDefaultsToEnforcement(t *testing.T) {
	path := writeTemp(t, validConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode.Type != "enforcement" {
		t.Errorf("expected mode enforcement, got %q", cfg.Mode.Type)
	}
}

func TestValidate_ModeExplicitEnforcement(t *testing.T) {
	path := writeTemp(t, `
mode:
  type: "enforcement"
`+validConfig[1:]) // prepend mode to validConfig (skip leading newline)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode.Type != "enforcement" {
		t.Errorf("expected mode enforcement, got %q", cfg.Mode.Type)
	}
}

func TestValidate_ModeListener(t *testing.T) {
	path := writeTemp(t, `
mode:
  type: "listener"
upstreams:
  openai: "https://api.openai.com"
default_policy:
  id: "_listener_default"
  budgets:
    rolling_tokens:
      window_seconds: 3600
      limit_tokens: 1000000
      burst_tokens: 0
  guards:
    max_tokens_per_request: 32768
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode.Type != "listener" {
		t.Errorf("expected mode listener, got %q", cfg.Mode.Type)
	}
	if cfg.ListenAddr != ":15080" {
		t.Errorf("expected listener default port :15080, got %q", cfg.ListenAddr)
	}
}

func TestValidate_ModeListenerNoDefaultPolicy(t *testing.T) {
	path := writeTemp(t, `
mode:
  type: "listener"
upstreams:
  openai: "https://api.openai.com"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for listener mode without default_policy")
	}
	if !strings.Contains(err.Error(), "default_policy") {
		t.Errorf("expected error about default_policy, got: %v", err)
	}
}

func TestValidate_ModeInvalid(t *testing.T) {
	path := writeTemp(t, `
mode:
  type: "passthrough"
upstreams:
  openai: "https://api.openai.com"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid mode type")
	}
	if !strings.Contains(err.Error(), "mode.type") {
		t.Errorf("expected error about mode.type, got: %v", err)
	}
}

func TestValidate_ModeListenerCustomListenAddr(t *testing.T) {
	path := writeTemp(t, `
mode:
  type: "listener"
listen_addr: ":9090"
upstreams:
  openai: "https://api.openai.com"
default_policy:
  id: "_default"
  budgets:
    rolling_tokens:
      window_seconds: 60
      limit_tokens: 1000
      burst_tokens: 0
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("expected custom listen addr :9090, got %q", cfg.ListenAddr)
	}
}

// --- Pod identity mode tests ---

func TestValidate_PodIdentityMode(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "pod"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("expected pod identity mode to be accepted, got: %v", err)
	}
}

func TestValidate_PodIdentityKeyNotRequired(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "pod"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("pod identity should not require key, got: %v", err)
	}
}

func TestValidate_MixedHeaderAndPodIdentity(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: "x-genops-workload-id"
    policy_refs:
      - "standard"
  - id: "svc-b"
    identity:
      mode: "pod"
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("mixed header and pod identity should be valid, got: %v", err)
	}
}

func TestValidate_HeaderModeEmptyKey(t *testing.T) {
	path := writeTemp(t, `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "svc-a"
    identity:
      mode: "header"
      key: ""
    policy_refs:
      - "standard"
policies:
  - id: "standard"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 1000
        burst_tokens: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for header mode with empty key")
	}
}

func TestSSEExtractionEnabled_Default(t *testing.T) {
	cfg := &Config{}
	if !cfg.SSEExtractionEnabled() {
		t.Error("expected SSE extraction enabled by default")
	}
}

func TestSSEExtractionEnabled_Explicit(t *testing.T) {
	f := false
	cfg := &Config{SSEExtraction: &f}
	if cfg.SSEExtractionEnabled() {
		t.Error("expected SSE extraction disabled when explicitly false")
	}

	tr := true
	cfg.SSEExtraction = &tr
	if !cfg.SSEExtractionEnabled() {
		t.Error("expected SSE extraction enabled when explicitly true")
	}
}

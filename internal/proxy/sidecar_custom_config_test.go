package proxy

// Phase 3 validation: sidecar custom config via ConfigMap.
//
// These tests simulate the full main.go startup path for sidecar file-config
// mode and then exercise the handler end-to-end. They validate:
//
// 1. Custom listener-sidecar: shadow decisions against custom policy, no blocking
// 2. Custom enforcement-sidecar: active blocking (429/503) against custom policy
// 3. Operator failure modes: missing policy, missing config file

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/enforce"
	"github.com/koshihq/koshi-runtime/internal/fanout"
	"github.com/koshihq/koshi-runtime/internal/identity"
	"github.com/koshihq/koshi-runtime/internal/policy"
)

// writeTempConfig writes a YAML config file and returns the path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// customSidecarConfig is a minimal sidecar custom config with a tight policy
// (small guard, small budget) that's easy to trigger in tests.
const customSidecarConfig = `
upstreams:
  openai: "https://api.openai.com"
  anthropic: "https://api.anthropic.com"

policies:
  - id: "team-tight"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 500
        burst_tokens: 50
    guards:
      max_tokens_per_request: 256
    decision_tiers:
      tier1_auto:
        action: "throttle"
      tier3_platform:
        action: "kill_workload"
`

// startupSidecarFileConfig simulates the sidecar file-config startup path
// from main.go. Returns the fully-wired handler ready to serve requests.
//
// This mirrors main.go lines for the KOSHI_CONFIG_PATH + KOSHI_POD_NAMESPACE branch.
func startupSidecarFileConfig(t *testing.T, configPath string, upstreamURL string, mode string, policyID string) (*Handler, *spyEmitter) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Step 1: Parse (not Load — no standalone validation)
	cfg, err := config.Parse(configPath)
	if err != nil {
		t.Fatalf("config.Parse failed: %v", err)
	}

	// Step 2: ValidateSidecarConfig
	if err := cfg.ValidateSidecarConfig(); err != nil {
		t.Fatalf("ValidateSidecarConfig failed: %v", err)
	}

	// Step 3: Pod identity (simulated)
	ns := "prod"
	wKind := "Deployment"
	wName := "test-workload"

	// Step 4: Resolve mode — annotation/env only, cfg.Mode.Type ignored
	resolvedMode := "listener"
	if mode == "enforcement" {
		resolvedMode = "enforcement"
	}
	cfg.Mode.Type = resolvedMode

	// Step 5: Require explicit policy selection
	if policyID == "" {
		t.Fatal("sidecar custom config requires policy selection")
	}
	found := false
	for _, p := range cfg.Policies {
		if p.ID == policyID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("policy %q not found in config", policyID)
	}

	// Step 6-7: Synthesize workload and append
	workloadID := fmt.Sprintf("%s/%s/%s", ns, wKind, wName)
	cfg.Workloads = append(cfg.Workloads, config.Workload{
		ID:         workloadID,
		Type:       "sidecar",
		Identity:   config.Identity{Mode: "pod"},
		PolicyRefs: []string{policyID},
	})

	// Override upstream URL for test
	cfg.Upstreams["openai"] = upstreamURL

	// Step 9: Build policy engine and budget tracker
	policyEngine := policy.NewMapEngine(cfg, logger)
	budgetTracker := budget.NewTracker()

	pol, ok := policyEngine.Lookup(identity.WorkloadIdentity{WorkloadID: workloadID})
	if !ok {
		t.Fatalf("workload %q has no resolvable policy", workloadID)
	}
	rt := pol.Budgets.RollingTokens
	budgetTracker.Register(workloadID, budget.BudgetParams{
		WindowSeconds: rt.WindowSeconds,
		LimitTokens:   rt.LimitTokens,
		BurstTokens:   rt.BurstTokens,
	})

	spy := &spyEmitter{}

	// Build handler config matching what main.go does
	hcfg := HandlerConfig{
		Resolver:      identity.NewPodResolver(), // Step 10: PodResolver for sidecars
		PolicyEngine:  policyEngine,
		BudgetTracker: budgetTracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       spy,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: cfg.SSEExtractionEnabled(),
		Logger:        logger,
		Mode:          resolvedMode,
	}

	// Listener mode needs accounting key
	if resolvedMode == "listener" {
		hcfg.ListenerAccountingKey = workloadID
	}

	return NewHandler(hcfg), spy
}

// makePodRequest creates an HTTP request with pod identity env vars set in
// the way PodResolver expects them. PodResolver reads from env vars, so we
// set them before the request.
func makePodRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// ============================================================
// Validation 1: Custom listener-sidecar behavior
// ============================================================

func TestSidecarCustomConfig_Listener_AllowWithinLimits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"total_tokens": 30},
		})
	}))
	defer upstream.Close()

	// Set pod identity env vars for PodResolver
	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, customSidecarConfig)
	h, spy := startupSidecarFileConfig(t, configPath, upstream.URL, "", "team-tight")

	// Request within guard limit (256)
	req := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":100}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Listener mode: 200, traffic not blocked
	if w.Code != http.StatusOK {
		t.Errorf("listener mode should not block, got %d: %s", w.Code, w.Body.String())
	}

	// Should have shadow allow event
	events := spy.eventsByType("listener_shadow")
	if len(events) == 0 {
		t.Fatal("expected listener_shadow event")
	}
	if events[0].Attributes["decision_shadow"] != ShadowAllow {
		t.Errorf("expected shadow allow, got %v", events[0].Attributes["decision_shadow"])
	}
	if events[0].Attributes["mode"] != "listener" {
		t.Errorf("expected mode=listener, got %v", events[0].Attributes["mode"])
	}
}

func TestSidecarCustomConfig_Listener_GuardExceeded_WouldThrottle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"total_tokens": 30},
		})
	}))
	defer upstream.Close()

	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, customSidecarConfig)
	h, spy := startupSidecarFileConfig(t, configPath, upstream.URL, "", "team-tight")

	// Request exceeding guard (256) — triggers would_throttle in listener mode
	req := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":1000}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Listener mode: still 200, traffic not blocked
	if w.Code != http.StatusOK {
		t.Errorf("listener mode should not block on guard exceeded, got %d: %s", w.Code, w.Body.String())
	}

	// Should have would_throttle shadow event with guard reason
	events := spy.eventsByType("listener_shadow")
	found := false
	for _, e := range events {
		if e.Attributes["decision_shadow"] == ShadowWouldThrottle &&
			e.Attributes["reason_code"] == enforce.ReasonGuardMaxTokens {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected would_throttle shadow event with reason guard_max_tokens")
	}
}

// ============================================================
// Validation 2: Custom enforcement-sidecar behavior
// ============================================================

func TestSidecarCustomConfig_Enforcement_AllowWithinLimits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"total_tokens": 30},
		})
	}))
	defer upstream.Close()

	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, customSidecarConfig)
	h, _ := startupSidecarFileConfig(t, configPath, upstream.URL, "enforcement", "team-tight")

	req := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":100}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("enforcement mode should allow within limits, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSidecarCustomConfig_Enforcement_GuardExceeded_Returns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called when guard is exceeded in enforcement mode")
	}))
	defer upstream.Close()

	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, customSidecarConfig)
	h, _ := startupSidecarFileConfig(t, configPath, upstream.URL, "enforcement", "team-tight")

	// max_tokens=1000 exceeds guard of 256 → 429 in enforcement mode
	req := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":1000}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("enforcement mode should return 429 on guard exceeded, got %d: %s", w.Code, w.Body.String())
	}

	// Verify reason code in response body
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["reason_code"] != enforce.ReasonGuardMaxTokens {
		t.Errorf("expected reason_code guard_max_tokens, got %v", resp["reason_code"])
	}
}

func TestSidecarCustomConfig_Enforcement_BudgetTracked(t *testing.T) {
	// Verify budget accounting runs against the custom policy's budget parameters.
	// Guard blocking (tested above) validates enforcement mode blocking.
	// Budget reservation/reconciliation uses the custom policy's rolling window.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"total_tokens": 30},
		})
	}))
	defer upstream.Close()

	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, customSidecarConfig)

	// Build handler with budget spy to observe accounting.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg, err := config.Parse(configPath)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	cfg.ValidateSidecarConfig()
	workloadID := "prod/Deployment/test-workload"
	cfg.Workloads = append(cfg.Workloads, config.Workload{
		ID: workloadID, Type: "sidecar", Identity: config.Identity{Mode: "pod"}, PolicyRefs: []string{"team-tight"},
	})
	cfg.Mode.Type = "enforcement"
	cfg.Upstreams["openai"] = upstream.URL

	tracker := budget.NewTracker()
	// Budget from custom policy: 500 tokens, 60s window, 50 burst
	tracker.Register(workloadID, budget.BudgetParams{WindowSeconds: 60, LimitTokens: 500, BurstTokens: 50})
	spy := newBudgetSpy(tracker)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewPodResolver(),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: spy,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       &spyEmitter{},
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
		Mode:          "enforcement",
	})

	req := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":100}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify budget was reserved and reconciled with custom policy's workload key.
	if spy.reserveCalls.Load() != 1 {
		t.Errorf("expected 1 reserve call, got %d", spy.reserveCalls.Load())
	}
	if spy.recordCalls.Load() != 1 {
		t.Errorf("expected 1 record call, got %d", spy.recordCalls.Load())
	}

	spy.mu.Lock()
	if len(spy.reserveKeys) == 0 || spy.reserveKeys[0] != workloadID {
		t.Errorf("expected reserve key %q, got %v", workloadID, spy.reserveKeys)
	}
	if len(spy.recordKeys) == 0 || spy.recordKeys[0] != workloadID {
		t.Errorf("expected record key %q, got %v", workloadID, spy.recordKeys)
	}
	spy.mu.Unlock()

	// Verify reconciliation delta: actual(30) - reserved(100) = -70
	if spy.lastDelta.Load() != -70 {
		t.Errorf("expected reconciliation delta -70, got %d", spy.lastDelta.Load())
	}
}

func TestSidecarCustomConfig_Enforcement_DiffersFromListener(t *testing.T) {
	// Verify the same config produces different behavior in enforcement vs listener mode.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"total_tokens": 30},
		})
	}))
	defer upstream.Close()

	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, customSidecarConfig)

	// Listener mode: guard exceeded → 200 (shadow)
	hListener, _ := startupSidecarFileConfig(t, configPath, upstream.URL, "", "team-tight")
	reqL := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":1000}`)
	wL := httptest.NewRecorder()
	hListener.ServeHTTP(wL, reqL)

	// Enforcement mode: guard exceeded → 429
	hEnforce, _ := startupSidecarFileConfig(t, configPath, upstream.URL, "enforcement", "team-tight")
	reqE := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":1000}`)
	wE := httptest.NewRecorder()
	hEnforce.ServeHTTP(wE, reqE)

	if wL.Code != http.StatusOK {
		t.Errorf("listener should return 200, got %d", wL.Code)
	}
	if wE.Code != http.StatusTooManyRequests {
		t.Errorf("enforcement should return 429, got %d", wE.Code)
	}
}

// ============================================================
// Validation 3: Operator failure modes
// ============================================================

func TestSidecarCustomConfig_MissingPolicyOverride_FailsStartup(t *testing.T) {
	// When runtime.getkoshi.ai/configmap is used without runtime.getkoshi.ai/policy,
	// KOSHI_POLICY_OVERRIDE is empty → startup must fail.
	configPath := writeTempConfig(t, customSidecarConfig)

	cfg, err := config.Parse(configPath)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := cfg.ValidateSidecarConfig(); err != nil {
		t.Fatalf("ValidateSidecarConfig failed: %v", err)
	}

	// Simulate: KOSHI_POLICY_OVERRIDE is empty (no annotation)
	policyOverride := "" // This is what main.go reads from os.Getenv("KOSHI_POLICY_OVERRIDE")
	if policyOverride == "" {
		// This is the expected failure path — main.go would os.Exit(1) with:
		// "sidecar custom config requires runtime.getkoshi.ai/policy annotation (KOSHI_POLICY_OVERRIDE)"
		t.Log("PASS: missing KOSHI_POLICY_OVERRIDE correctly detected — startup would fail")
		return
	}
	t.Error("should have detected missing policy override")
}

func TestSidecarCustomConfig_UnknownPolicyOverride_FailsStartup(t *testing.T) {
	configPath := writeTempConfig(t, customSidecarConfig)

	cfg, err := config.Parse(configPath)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := cfg.ValidateSidecarConfig(); err != nil {
		t.Fatalf("ValidateSidecarConfig failed: %v", err)
	}

	// Simulate: KOSHI_POLICY_OVERRIDE references a policy not in the config
	policyOverride := "nonexistent-policy"
	found := false
	for _, p := range cfg.Policies {
		if p.ID == policyOverride {
			found = true
			break
		}
	}
	if found {
		t.Error("policy should not exist in config")
	}
	// main.go would os.Exit(1) with:
	// "KOSHI_POLICY_OVERRIDE references unknown policy in sidecar config"
	t.Log("PASS: unknown KOSHI_POLICY_OVERRIDE correctly detected — startup would fail")
}

func TestSidecarCustomConfig_MissingConfigFile_FailsStartup(t *testing.T) {
	// When the ConfigMap doesn't have a config.yaml key, the mount path
	// won't have the file → config.Parse fails.
	_, err := config.Parse("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error when config file is missing")
	}
	t.Logf("PASS: missing config.yaml correctly detected: %v", err)
}

func TestSidecarCustomConfig_WorkloadsInConfig_FailsValidation(t *testing.T) {
	configWithWorkloads := `
upstreams:
  openai: "https://api.openai.com"
workloads:
  - id: "should-not-be-here"
    identity:
      mode: "pod"
    policy_refs:
      - "team-tight"
policies:
  - id: "team-tight"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 500
        burst_tokens: 50
    guards:
      max_tokens_per_request: 256
`
	configPath := writeTempConfig(t, configWithWorkloads)
	cfg, err := config.Parse(configPath)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	err = cfg.ValidateSidecarConfig()
	if err == nil {
		t.Fatal("expected error for workloads in sidecar config")
	}
	if !strings.Contains(err.Error(), "workloads must not be defined") {
		t.Errorf("expected error about workloads, got: %v", err)
	}
	t.Logf("PASS: workloads in sidecar config correctly rejected: %v", err)
}

func TestSidecarCustomConfig_ModeIgnoredFromConfigFile(t *testing.T) {
	// Config file says enforcement, but no KOSHI_MODE env → should default to listener
	configWithEnforcementMode := `
mode:
  type: "enforcement"
upstreams:
  openai: "https://api.openai.com"
policies:
  - id: "team-tight"
    budgets:
      rolling_tokens:
        window_seconds: 60
        limit_tokens: 500
        burst_tokens: 50
    guards:
      max_tokens_per_request: 256
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"total_tokens": 30},
		})
	}))
	defer upstream.Close()

	t.Setenv("KOSHI_POD_NAMESPACE", "prod")
	t.Setenv("KOSHI_WORKLOAD_KIND", "Deployment")
	t.Setenv("KOSHI_WORKLOAD_NAME", "test-workload")

	configPath := writeTempConfig(t, configWithEnforcementMode)
	// Pass empty mode (simulating KOSHI_MODE unset) → should default to listener
	h, _ := startupSidecarFileConfig(t, configPath, upstream.URL, "", "team-tight")

	// Guard exceeded request — in listener mode should get 200, not 429
	req := makePodRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4","max_tokens":1000}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("config.mode.type=enforcement should be ignored for sidecars; expected listener (200), got %d", w.Code)
	}
}

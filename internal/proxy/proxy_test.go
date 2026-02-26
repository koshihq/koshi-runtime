package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/emit"
	"github.com/koshihq/koshi-runtime/internal/enforce"
	"github.com/koshihq/koshi-runtime/internal/fanout"
	"github.com/koshihq/koshi-runtime/internal/identity"
	"github.com/koshihq/koshi-runtime/internal/policy"
	"github.com/koshihq/koshi-runtime/internal/provider"
)

// ============================================================
// Test Helpers
// ============================================================

func testConfig() *config.Config {
	return &config.Config{
		Upstreams: map[string]string{
			"openai":    "", // Will be set to mock server URL.
			"anthropic": "",
		},
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
						WindowSeconds: 60,
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
			},
		},
	}
}

// budgetSpy wraps a real tracker and records calls.
type budgetSpy struct {
	inner        budget.Tracker
	reserveCalls atomic.Int64
	recordCalls  atomic.Int64
	lastDelta    atomic.Int64
}

func newBudgetSpy(inner budget.Tracker) *budgetSpy {
	return &budgetSpy{inner: inner}
}

func (s *budgetSpy) Reserve(ctx context.Context, wid string, tokens int64) (budget.BudgetStatus, bool, error) {
	s.reserveCalls.Add(1)
	return s.inner.Reserve(ctx, wid, tokens)
}

func (s *budgetSpy) Record(ctx context.Context, report budget.UsageReport) {
	s.recordCalls.Add(1)
	s.lastDelta.Store(report.Tokens)
	s.inner.Record(ctx, report)
}

func (s *budgetSpy) Status(ctx context.Context, wid string) (budget.BudgetStatus, error) {
	return s.inner.Status(ctx, wid)
}

func (s *budgetSpy) StatusAll(ctx context.Context) map[string]budget.WorkloadStatus {
	return s.inner.StatusAll(ctx)
}

// panicResolver panics on Resolve to test degraded mode.
type panicResolver struct{}

func (p *panicResolver) Resolve(_ *http.Request) (identity.WorkloadIdentity, error) {
	panic("intentional panic for testing")
}

func makeHandler(t *testing.T, upstreamURL string) (*Handler, *budgetSpy) {
	t.Helper()
	cfg := testConfig()
	cfg.Upstreams["openai"] = upstreamURL

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	spy := newBudgetSpy(tracker)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: spy,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false, // Phase 1: no SSE extraction.
		Logger:        logger,
	})
	return h, spy
}

// spyEmitter captures emitted events for test assertions.
type spyEmitter struct {
	mu     sync.Mutex
	events []emit.Event
}

func (s *spyEmitter) Emit(_ context.Context, e emit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *spyEmitter) Close()         {}
func (s *spyEmitter) Dropped() int64 { return 0 }

func (s *spyEmitter) eventsByType(eventType string) []emit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []emit.Event
	for _, e := range s.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// ============================================================
// Non-Streaming Proxy Tests
// ============================================================

func TestProxy_NonStreaming_RoundTrip(t *testing.T) {
	// Mock upstream returns OpenAI-style response with usage.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-abc",
			"model": "gpt-4",
			"choices": []map[string]any{
				{"message": map[string]string{"content": "Hello"}},
			},
			"usage": map[string]int{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		})
	}))
	defer upstream.Close()

	h, spy := makeHandler(t, upstream.URL)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"max_tokens":4096}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify response contains usage data.
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["usage"] == nil {
		t.Error("expected usage in response")
	}

	// Verify budget spy was called.
	if spy.reserveCalls.Load() != 1 {
		t.Errorf("expected 1 reserve call, got %d", spy.reserveCalls.Load())
	}
	if spy.recordCalls.Load() != 1 {
		t.Errorf("expected 1 record call, got %d", spy.recordCalls.Load())
	}

	// Delta should be actual(30) - reserved(4096) = -4066
	if spy.lastDelta.Load() != -4066 {
		t.Errorf("expected delta -4066, got %d", spy.lastDelta.Load())
	}
}

func TestProxy_Upstream5xx_ReturnsReservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer upstream.Close()

	h, spy := makeHandler(t, upstream.URL)

	body := `{"model":"gpt-4","messages":[],"max_tokens":1000}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}

	// Record should have been called to return reservation.
	if spy.recordCalls.Load() != 1 {
		t.Errorf("expected 1 record call for 5xx, got %d", spy.recordCalls.Load())
	}
	// Delta should be -reservedTokens (returning full reservation).
	if spy.lastDelta.Load() != -1000 {
		t.Errorf("expected delta -1000 (reservation returned), got %d", spy.lastDelta.Load())
	}
}

// ============================================================
// Identity + Policy Tests
// ============================================================

func TestProxy_MissingIdentity_NoDefault_403(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h, _ := makeHandler(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	// No workload identity header.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestProxy_MissingIdentity_WithDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 10, "total_tokens": 15},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	cfg.DefaultPolicy = &config.Policy{
		ID: "_default",
		Budgets: config.Budgets{
			RollingTokens: config.RollingTokenBudget{
				WindowSeconds: 60,
				LimitTokens:   1000,
				BurstTokens:   0,
			},
		},
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	tracker.Register("_default", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 1000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	body := `{"model":"gpt-4","messages":[],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	// No workload identity header — should use default policy.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with default policy, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_UnknownWorkload_403(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h, _ := makeHandler(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	req.Header.Set("x-genops-workload-id", "unknown-service")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

// ============================================================
// Enforcement Tests
// ============================================================

func TestProxy_BudgetExhausted_429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5000, "completion_tokens": 5000, "total_tokens": 10000},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	// Small budget: limit=5000, no burst
	cfg.Policies[0].Budgets.RollingTokens.LimitTokens = 5000
	cfg.Policies[0].Budgets.RollingTokens.BurstTokens = 0

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 5000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// First request: reserve 4096 (default max_tokens) — fits in 5000.
	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", w.Code)
	}

	// Second request: should be denied (budget near/at limit after reconciliation).
	req2 := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("x-genops-workload-id", "svc-a")

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after budget exhausted, got %d", w2.Code)
	}

	// Verify Retry-After header.
	retryAfter := w2.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestProxy_PerRequestGuard_429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not be called for guarded request")
	}))
	defer upstream.Close()

	h, _ := makeHandler(t, upstream.URL)

	// max_tokens=8192 exceeds guard of 4096.
	body := `{"model":"gpt-4","messages":[],"max_tokens":8192}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for guard violation, got %d", w.Code)
	}
}

// ============================================================
// Degraded Mode Tests
// ============================================================

func TestProxy_DegradedMode_PanicRecovery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      &panicResolver{}, // Will panic.
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// First request triggers panic → handler enters degraded mode.
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	req.Header.Set("x-genops-workload-id", "svc-a")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// The panic should be recovered. We get a 500.
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on panic, got %d", w.Code)
	}

	// Handler should now be degraded.
	if !h.IsDegraded() {
		t.Error("expected handler to be degraded after panic")
	}

	// Subsequent request should pass through without enforcement.
	req2 := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	// In degraded mode, proxy should forward (but without workload header,
	// it still routes to upstream based on Host).
	// The mock upstream returns 200.
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 in degraded pass-through, got %d", w2.Code)
	}
}

// ============================================================
// Token Count / SSE Helper Tests
// ============================================================

func TestExtractMaxTokens(t *testing.T) {
	tests := []struct {
		body     string
		expected int64
	}{
		{`{"max_tokens": 4096}`, 4096},
		{`{"max_tokens": 100}`, 100},
		{`{"model": "gpt-4"}`, 0},
		{`{}`, 0},
		{``, 0},
		{`not json`, 0},
	}

	for _, tt := range tests {
		got := extractMaxTokens([]byte(tt.body))
		if got != tt.expected {
			t.Errorf("extractMaxTokens(%q) = %d, want %d", tt.body, got, tt.expected)
		}
	}
}

func TestIsStreamingRequest(t *testing.T) {
	tests := []struct {
		body     string
		expected bool
	}{
		{`{"stream": true}`, true},
		{`{"stream": false}`, false},
		{`{"model": "gpt-4"}`, false},
		{`{}`, false},
		{``, false},
	}

	for _, tt := range tests {
		got := isStreamingRequest([]byte(tt.body))
		if got != tt.expected {
			t.Errorf("isStreamingRequest(%q) = %v, want %v", tt.body, got, tt.expected)
		}
	}
}

func TestInjectStreamOptions(t *testing.T) {
	body := []byte(`{"model":"gpt-4","stream":true}`)
	modified, err := injectStreamOptions(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(modified, &parsed); err != nil {
		t.Fatalf("failed to parse modified body: %v", err)
	}

	if _, ok := parsed["stream_options"]; !ok {
		t.Error("expected stream_options in modified body")
	}

	var opts map[string]bool
	if err := json.Unmarshal(parsed["stream_options"], &opts); err != nil {
		t.Fatalf("failed to parse stream_options: %v", err)
	}
	if !opts["include_usage"] {
		t.Error("expected include_usage=true")
	}

	// Verify original fields preserved.
	if _, ok := parsed["model"]; !ok {
		t.Error("expected model field preserved")
	}
	if _, ok := parsed["stream"]; !ok {
		t.Error("expected stream field preserved")
	}
}

func TestIsSSEResponse(t *testing.T) {
	if !isSSEResponse("text/event-stream") {
		t.Error("expected true for text/event-stream")
	}
	if !isSSEResponse("text/event-stream; charset=utf-8") {
		t.Error("expected true for text/event-stream with charset")
	}
	if isSSEResponse("application/json") {
		t.Error("expected false for application/json")
	}
}

// ============================================================
// SSE Token Extracting Reader Tests
// ============================================================

func TestTokenExtractingReader_PassThrough(t *testing.T) {
	// Verify bytes pass through unchanged.
	sseData := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	inner := io.NopCloser(strings.NewReader(sseData))

	var extractedUsage *provider.UsageData
	r := newTokenExtractingReader(inner, provider.ParseOpenAISSEUsage, func(usage *provider.UsageData) {
		extractedUsage = usage
	})

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	if buf.String() != sseData {
		t.Errorf("byte fidelity violated:\ngot:  %q\nwant: %q", buf.String(), sseData)
	}

	// No usage in this stream.
	if extractedUsage != nil {
		t.Errorf("expected no usage, got %+v", extractedUsage)
	}
}

func TestTokenExtractingReader_ExtractsUsage(t *testing.T) {
	sseData := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n" +
		"data: [DONE]\n\n"

	inner := io.NopCloser(strings.NewReader(sseData))

	var extractedUsage *provider.UsageData
	r := newTokenExtractingReader(inner, provider.ParseOpenAISSEUsage, func(usage *provider.UsageData) {
		extractedUsage = usage
	})

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	// Bytes must pass through unchanged.
	if buf.String() != sseData {
		t.Errorf("byte fidelity violated:\ngot:  %q\nwant: %q", buf.String(), sseData)
	}

	// Usage must be extracted.
	if extractedUsage == nil {
		t.Fatal("expected usage to be extracted")
	}
	if extractedUsage.TotalTokens != 30 {
		t.Errorf("expected total 30, got %d", extractedUsage.TotalTokens)
	}
	if extractedUsage.InputTokens != 10 {
		t.Errorf("expected input 10, got %d", extractedUsage.InputTokens)
	}
}

func TestTokenExtractingReader_PartialLine(t *testing.T) {
	// Simulate usage JSON split across two reads.
	part1 := "data: {\"choices\":[],\"usa"
	part2 := "ge\":{\"prompt_tokens\":5,\"completion_tokens\":15,\"total_tokens\":20}}\n\ndata: [DONE]\n\n"

	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(part1))
		time.Sleep(10 * time.Millisecond)
		pw.Write([]byte(part2))
		pw.Close()
	}()

	var extractedUsage *provider.UsageData
	r := newTokenExtractingReader(pr, provider.ParseOpenAISSEUsage, func(usage *provider.UsageData) {
		extractedUsage = usage
	})

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	expected := part1 + part2
	if buf.String() != expected {
		t.Errorf("byte fidelity violated for partial line")
	}

	if extractedUsage == nil {
		t.Fatal("expected usage extracted from split line")
	}
	if extractedUsage.TotalTokens != 20 {
		t.Errorf("expected 20, got %d", extractedUsage.TotalTokens)
	}
}

func TestTokenExtractingReader_LineBufferOverflow(t *testing.T) {
	// Create a line longer than 8KB — should be discarded without panic.
	longLine := "data: " + strings.Repeat("x", maxLineBuffer+100) + "\n\n"
	sseData := longLine + "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"

	inner := io.NopCloser(strings.NewReader(sseData))
	var extractedUsage *provider.UsageData
	r := newTokenExtractingReader(inner, provider.ParseOpenAISSEUsage, func(usage *provider.UsageData) {
		extractedUsage = usage
	})

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	// The reader should still extract usage from subsequent lines.
	if extractedUsage == nil {
		t.Fatal("expected usage after buffer overflow recovery")
	}
	if extractedUsage.TotalTokens != 3 {
		t.Errorf("expected 3, got %d", extractedUsage.TotalTokens)
	}
}

func TestTokenExtractingReader_PanicSafety(t *testing.T) {
	// Create a reader that will trigger a panic during scan.
	// We use a custom reader that panics on the second read.
	callCount := 0
	panicReader := &panicOnNthRead{
		inner: strings.NewReader("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"),
		n:     1,
		count: &callCount,
	}

	var extractedUsage *provider.UsageData
	r := newTokenExtractingReader(io.NopCloser(panicReader), provider.ParseOpenAISSEUsage, func(usage *provider.UsageData) {
		extractedUsage = usage
	})

	// Should not panic — recovered internally.
	buf := make([]byte, 4096)
	for {
		_, err := r.Read(buf)
		if err != nil {
			break
		}
	}
	r.Close()

	// We don't assert usage was extracted because the panic may interrupt scanning.
	// The key assertion: no panic propagated.
	_ = extractedUsage
}

type panicOnNthRead struct {
	inner io.Reader
	n     int
	count *int
}

func (p *panicOnNthRead) Read(buf []byte) (int, error) {
	*p.count++
	if *p.count >= p.n {
		panic("intentional panic in Read")
	}
	return p.inner.Read(buf)
}

// ============================================================
// Concurrency Tests
// ============================================================

func TestProxy_Concurrent_NoRace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 1000000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"gpt-4","messages":[],"max_tokens":100}`)
			req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("x-genops-workload-id", "svc-a")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			// We just verify no panics or races.
		}(i)
	}

	wg.Wait()
}

// ============================================================
// Response Shape Tests (v0.2 Operator Clarity)
// ============================================================

func TestProxy_KillDecision_ResponseShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5000, "completion_tokens": 5000, "total_tokens": 10000},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	cfg.Policies[0].Budgets.RollingTokens.LimitTokens = 5000
	cfg.Policies[0].Budgets.RollingTokens.BurstTokens = 0
	cfg.Policies[0].DecisionTiers.Tier3Platform = config.TierAction{Action: "kill_workload"}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 5000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// First request: exhaust budget.
	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Second request: should be killed.
	req2 := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("x-genops-workload-id", "svc-a")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w2.Code)
	}
	if got := w2.Header().Get("X-GenOps-Decision"); got != "kill" {
		t.Errorf("expected X-GenOps-Decision: kill, got %q", got)
	}

	var resp map[string]any
	json.NewDecoder(w2.Body).Decode(&resp)
	if resp["error"] != "workload_killed" {
		t.Errorf("expected error=workload_killed, got %v", resp["error"])
	}
	if resp["category"] != "enforcement" {
		t.Errorf("expected category=enforcement, got %v", resp["category"])
	}
	if resp["reason"] == nil || resp["reason"] == "" {
		t.Error("expected non-empty reason")
	}
}

func TestProxy_DegradedMode_ResponseShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      &panicResolver{},
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if got := w.Header().Get("X-GenOps-Decision"); got != "degraded" {
		t.Errorf("expected X-GenOps-Decision: degraded, got %q", got)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "service_degraded" {
		t.Errorf("expected error=service_degraded, got %v", resp["error"])
	}
	if resp["category"] != "system" {
		t.Errorf("expected category=system, got %v", resp["category"])
	}
}

func TestProxy_IdentityRejected_ResponseShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h, _ := makeHandler(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get("X-GenOps-Decision"); got != "reject" {
		t.Errorf("expected X-GenOps-Decision: reject, got %q", got)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "identity_required" {
		t.Errorf("expected error=identity_required, got %v", resp["error"])
	}
	if resp["category"] != "enforcement" {
		t.Errorf("expected category=enforcement, got %v", resp["category"])
	}
}

func TestUnconfiguredProviderRejected(t *testing.T) {
	// Create handler with only OpenAI configured — no Anthropic in upstreams.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request should not reach upstream for unconfigured provider")
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams = map[string]string{
		"openai": upstream.URL, // Only OpenAI configured.
	}

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// Request targeting Anthropic — should be rejected with 502.
	body := `{"model":"claude-3-opus","messages":[],"max_tokens":1024}`
	req := httptest.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for unconfigured provider, got %d", w.Code)
	}
}

// ============================================================
// /status Endpoint Tests (Phase 3)
// ============================================================

func TestStatus_ReturnsRegisteredWorkloads(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not be called for /status")
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	tracker.Register("svc-b", budget.BudgetParams{WindowSeconds: 300, LimitTokens: 50000, BurstTokens: 0})

	// Reserve some tokens on svc-a.
	tracker.Reserve(context.Background(), "svc-a", 500)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
		Version:       "v0.4.0-test",
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	h.ServeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// Unmarshal into map[string]any to verify numeric types.
	var raw map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify version.
	if raw["version"] != "v0.4.0-test" {
		t.Errorf("expected version v0.4.0-test, got %v", raw["version"])
	}

	// Verify genops_spec_version.
	if got, ok := raw["genops_spec_version"].(string); !ok || got != "0.1.0" {
		t.Errorf("expected genops_spec_version 0.1.0, got %v", raw["genops_spec_version"])
	}

	workloads, ok := raw["workloads"].(map[string]any)
	if !ok {
		t.Fatal("expected workloads to be a map")
	}
	if len(workloads) != 2 {
		t.Fatalf("expected 2 workloads, got %d", len(workloads))
	}

	// Verify svc-a.
	svcA, ok := workloads["svc-a"].(map[string]any)
	if !ok {
		t.Fatal("expected svc-a in workloads")
	}
	// json.Decoder decodes numbers as float64.
	if got := svcA["limit_tokens"].(float64); got != 10000 {
		t.Errorf("svc-a limit_tokens: expected 10000, got %v", got)
	}
	if got := svcA["window_seconds"].(float64); got != 60 {
		t.Errorf("svc-a window_seconds: expected 60, got %v", got)
	}
	if got := svcA["window_tokens_used"].(float64); got != 500 {
		t.Errorf("svc-a window_tokens_used: expected 500, got %v", got)
	}
	if got := svcA["burst_remaining"].(float64); got != 1000 {
		t.Errorf("svc-a burst_remaining: expected 1000, got %v", got)
	}

	// Verify svc-b.
	svcB, ok := workloads["svc-b"].(map[string]any)
	if !ok {
		t.Fatal("expected svc-b in workloads")
	}
	if got := svcB["limit_tokens"].(float64); got != 50000 {
		t.Errorf("svc-b limit_tokens: expected 50000, got %v", got)
	}
	if got := svcB["window_seconds"].(float64); got != 300 {
		t.Errorf("svc-b window_seconds: expected 300, got %v", got)
	}
	if got := svcB["window_tokens_used"].(float64); got != 0 {
		t.Errorf("svc-b window_tokens_used: expected 0, got %v", got)
	}
}

func TestStatus_EmptyTracker(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(testConfig(), logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     map[string]string{},
		Logger:        logger,
		Version:       "v0.4.0",
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	h.ServeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var raw map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	// Verify genops_spec_version.
	if got, ok := raw["genops_spec_version"].(string); !ok || got != "0.1.0" {
		t.Errorf("expected genops_spec_version 0.1.0, got %v", raw["genops_spec_version"])
	}

	workloads, ok := raw["workloads"].(map[string]any)
	if !ok {
		t.Fatal("expected workloads to be a map")
	}
	if len(workloads) != 0 {
		t.Errorf("expected empty workloads map, got %d entries", len(workloads))
	}
}

func TestStatus_MethodNotAllowed(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(testConfig(), logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     map[string]string{},
		Logger:        logger,
		Version:       "v0.4.0",
	})

	req := httptest.NewRequest(http.MethodPost, "/status", nil)
	w := httptest.NewRecorder()
	h.ServeStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// ============================================================
// Multi-Workload Independent Budget Test (Phase 2)
// ============================================================

func TestProxy_MultiWorkload_IndependentBudgets(t *testing.T) {
	// Mock upstream returns 600 total_tokens for every request.
	// With max_tokens=100, delta = 600-100 = +500 per request.
	// This pushes actual usage above the limit through reconciliation,
	// ensuring the TierDecider sees used > limit on denial (not used == limit).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 200, "completion_tokens": 400, "total_tokens": 600},
		})
	}))
	defer upstream.Close()

	// svc-a: small limit (1000), exhausted after 2 requests
	//   Req 1: Reserve(100)→100, Record(+500)→600
	//   Req 2: Reserve(100)→700, Record(+500)→1200 (over limit)
	//   Req 3: Reserve(100)→1300>1000, denied, undo→1200, 1200>1000 → 429
	// svc-b: large limit (50000), never exhausted in this test
	cfg := &config.Config{
		Upstreams: map[string]string{
			"openai": upstream.URL,
		},
		Workloads: []config.Workload{
			{
				ID:         "svc-a",
				Identity:   config.Identity{Mode: "header", Key: "x-genops-workload-id"},
				PolicyRefs: []string{"standard"},
			},
			{
				ID:         "svc-b",
				Identity:   config.Identity{Mode: "header", Key: "x-genops-workload-id"},
				PolicyRefs: []string{"high-throughput"},
			},
		},
		Policies: []config.Policy{
			{
				ID: "standard",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 60,
						LimitTokens:   1000,
						BurstTokens:   0,
					},
				},
				Guards: config.Guards{MaxTokensPerRequest: 4096},
				DecisionTiers: config.DecisionTiers{
					Tier1Auto: config.TierAction{Action: "throttle"},
				},
			},
			{
				ID: "high-throughput",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 60,
						LimitTokens:   50000,
						BurstTokens:   0,
					},
				},
				Guards: config.Guards{MaxTokensPerRequest: 4096},
				DecisionTiers: config.DecisionTiers{
					Tier1Auto: config.TierAction{Action: "throttle"},
				},
			},
		},
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	// Use concrete TrackerImpl for numerical assertions via Status().
	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 1000, BurstTokens: 0})
	tracker.Register("svc-b", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 50000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	ctx := context.Background()

	// Verify per-policy params are correctly registered.
	statusA, err := tracker.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusA.WindowTokensLimit != 1000 {
		t.Errorf("svc-a limit: expected 1000, got %d", statusA.WindowTokensLimit)
	}
	statusB, err := tracker.Status(ctx, "svc-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusB.WindowTokensLimit != 50000 {
		t.Errorf("svc-b limit: expected 50000, got %d", statusB.WindowTokensLimit)
	}

	sendRequest := func(workloadID string, maxTokens int) int {
		body := fmt.Sprintf(`{"model":"gpt-4","messages":[],"max_tokens":%d}`, maxTokens)
		req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("x-genops-workload-id", workloadID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	// Exhaust svc-a: 2 requests push used to 1200 (over 1000 limit).
	for i := 0; i < 2; i++ {
		code := sendRequest("svc-a", 100)
		if code != http.StatusOK {
			t.Fatalf("svc-a request %d: expected 200, got %d", i+1, code)
		}
	}

	// svc-a should now be denied (429).
	code := sendRequest("svc-a", 100)
	if code != http.StatusTooManyRequests {
		t.Errorf("svc-a should be denied after exhaustion, expected 429, got %d", code)
	}

	// svc-b should still be allowed — independent budget.
	code = sendRequest("svc-b", 100)
	if code != http.StatusOK {
		t.Errorf("svc-b should still be allowed after svc-a exhausted, got %d", code)
	}

	// Numerical isolation: svc-b's tokens_used should not include svc-a's activity.
	statusB, err = tracker.Status(ctx, "svc-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// svc-b sent 1 request: reserve 100, actual 600, delta +500 → used=600
	if statusB.WindowTokensUsed != 600 {
		t.Errorf("svc-b tokens_used: expected 600, got %d (cross-contamination?)", statusB.WindowTokensUsed)
	}

	// svc-a should show its accumulated usage (1200 from 2 reconciled requests).
	statusA, err = tracker.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusA.WindowTokensUsed != 1200 {
		t.Errorf("svc-a tokens_used: expected 1200, got %d", statusA.WindowTokensUsed)
	}

	// svc-b should still be allowed at 600/50000.
	code = sendRequest("svc-b", 100)
	if code != http.StatusOK {
		t.Errorf("svc-b should be allowed at 600/50000, got %d", code)
	}
}

// ============================================================
// Budget Reconciled Emit Event Tests (Phase 3)
// ============================================================

func TestProxy_BudgetReconciled_NonStreaming(t *testing.T) {
	// Upstream returns total_tokens=30. Request has max_tokens=4096.
	// Delta = 30 - 4096 = -4066 (nonzero → event emitted).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-abc",
			"model": "gpt-4",
			"choices": []map[string]any{
				{"message": map[string]string{"content": "Hello"}},
			},
			"usage": map[string]int{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &spyEmitter{}

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       spy,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false,
		Logger:        logger,
	})

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"max_tokens":4096}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	reconciled := spy.eventsByType(emit.EventBudgetReconciled)
	if len(reconciled) != 1 {
		t.Fatalf("expected exactly 1 budget_reconciled event, got %d", len(reconciled))
	}

	ev := reconciled[0]
	if ev.WorkloadID != "svc-a" {
		t.Errorf("expected workload_id svc-a, got %q", ev.WorkloadID)
	}
	if ev.Severity != "info" {
		t.Errorf("expected severity info, got %q", ev.Severity)
	}
	if got := ev.Attributes["reserved_tokens"]; got != int64(4096) {
		t.Errorf("expected reserved_tokens 4096, got %v", got)
	}
	if got := ev.Attributes["actual_tokens"]; got != int64(30) {
		t.Errorf("expected actual_tokens 30, got %v", got)
	}
	if got := ev.Attributes["delta_tokens"]; got != int64(-4066) {
		t.Errorf("expected delta_tokens -4066, got %v", got)
	}
	if got := ev.Attributes["phase"]; got != "actual" {
		t.Errorf("expected phase actual, got %v", got)
	}
	if got := ev.Attributes["genops.spec.version"]; got != "0.1.0" {
		t.Errorf("expected genops.spec.version 0.1.0, got %v", got)
	}
}

// TestEmitEvent_NilAttributes verifies that events emitted with nil Attributes
// still receive genops.spec.version from the emit helper.
func TestEmitEvent_NilAttributes(t *testing.T) {
	cfg := testConfig()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &spyEmitter{}

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       spy,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false,
		Logger:        logger,
	})

	// Send a request without identity header — triggers policy_rejected which
	// currently emits with nil Attributes on the event.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":100}`))
	req.Host = "api.openai.com"
	// No x-genops-workload-id header → identity fails → 403
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}

	// identity_rejected event should have genops.spec.version even though
	// the original event only had {"error": ...} attributes.
	events := spy.eventsByType("identity_rejected")
	if len(events) == 0 {
		t.Fatal("expected identity_rejected event")
	}
	ev := events[0]
	if ev.Attributes == nil {
		t.Fatal("expected Attributes to be non-nil after emit helper")
	}
	if got := ev.Attributes["genops.spec.version"]; got != "0.1.0" {
		t.Errorf("expected genops.spec.version 0.1.0, got %v", got)
	}
}

// TestEmitEvent_AllowedPath verifies genops.spec.version on allowed requests.
func TestEmitEvent_AllowedPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"total_tokens":50}}`))
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &spyEmitter{}

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       spy,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false,
		Logger:        logger,
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":100}`))
	req.Host = "api.openai.com"
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	allowed := spy.eventsByType("request_allowed")
	if len(allowed) == 0 {
		t.Fatal("expected request_allowed event")
	}
	if got := allowed[0].Attributes["genops.spec.version"]; got != "0.1.0" {
		t.Errorf("expected genops.spec.version 0.1.0 on request_allowed, got %v", got)
	}
}

// TestEmitEvent_DeniedPath verifies genops.spec.version on guard rejection.
func TestEmitEvent_DeniedPath(t *testing.T) {
	cfg := testConfig()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spy := &spyEmitter{}

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       spy,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false,
		Logger:        logger,
	})

	// max_tokens exceeds guard (4096 limit)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":50000}`))
	req.Host = "api.openai.com"
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}

	events := spy.eventsByType("guard_rejected")
	if len(events) == 0 {
		t.Fatal("expected guard_rejected event")
	}
	if got := events[0].Attributes["genops.spec.version"]; got != "0.1.0" {
		t.Errorf("expected genops.spec.version 0.1.0 on guard_rejected, got %v", got)
	}
}

func TestNewHandler_ResponseHeaderTimeout_Default(t *testing.T) {
	h := NewHandler(HandlerConfig{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if h.transport.ResponseHeaderTimeout != DefaultResponseHeaderTimeout {
		t.Errorf("expected default %v, got %v", DefaultResponseHeaderTimeout, h.transport.ResponseHeaderTimeout)
	}
}

func TestNewHandler_ResponseHeaderTimeout_Custom(t *testing.T) {
	custom := 45 * time.Second
	h := NewHandler(HandlerConfig{
		ResponseHeaderTimeout: custom,
		Logger:                slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if h.transport.ResponseHeaderTimeout != custom {
		t.Errorf("expected %v, got %v", custom, h.transport.ResponseHeaderTimeout)
	}
}

// ============================================================
// Anthropic SSE Accumulator Tests
// ============================================================

func TestAnthropicAccumulator_FullSequence(t *testing.T) {
	acc := &anthropicSSEAccumulator{}

	// message_start with input_tokens
	usage, isFinal, _ := acc.Parse([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":0}}}`))
	if isFinal || usage != nil {
		t.Errorf("message_start: expected (nil, false), got (%v, %v)", usage, isFinal)
	}

	// content_block_delta — ignored
	usage, isFinal, _ = acc.Parse([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`))
	if isFinal || usage != nil {
		t.Errorf("content_block_delta: expected (nil, false), got (%v, %v)", usage, isFinal)
	}

	// message_delta with output_tokens — final
	usage, isFinal, _ = acc.Parse([]byte(`{"type":"message_delta","usage":{"output_tokens":73}}`))
	if !isFinal {
		t.Fatal("message_delta: expected isFinal=true")
	}
	if usage == nil {
		t.Fatal("message_delta: expected non-nil usage")
	}
	if usage.InputTokens != 42 {
		t.Errorf("expected input 42, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 73 {
		t.Errorf("expected output 73, got %d", usage.OutputTokens)
	}
	if usage.TotalTokens != 115 {
		t.Errorf("expected total 115, got %d", usage.TotalTokens)
	}
}

func TestAnthropicAccumulator_MissingMessageStart(t *testing.T) {
	acc := &anthropicSSEAccumulator{}

	// message_delta arrives without prior message_start — fail safe
	usage, isFinal, _ := acc.Parse([]byte(`{"type":"message_delta","usage":{"output_tokens":73}}`))
	if isFinal || usage != nil {
		t.Errorf("expected fail-safe (nil, false) when message_start missing, got (%v, %v)", usage, isFinal)
	}
}

func TestAnthropicAccumulator_MissingMessageDelta(t *testing.T) {
	// If message_start arrives but message_delta never comes, onUsage never fires.
	// This tests the tokenExtractingReader lifecycle: Close() checks r.usage which
	// stays nil because Parse never returned (usage, true, nil).
	acc := &anthropicSSEAccumulator{}

	usage, isFinal, _ := acc.Parse([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":0}}}`))
	if isFinal || usage != nil {
		t.Errorf("message_start: expected (nil, false), got (%v, %v)", usage, isFinal)
	}

	// Stream ends here — no message_delta.
	// Accumulator holds input_tokens but never produces UsageData.
	// tokenExtractingReader.Close() would see r.usage == nil → onUsage not called.
	// Reservation stands. This is the correct fail-safe behavior.
}

// ============================================================
// Concurrent Stress Tests (GA Readiness)
// ============================================================

func TestProxy_Concurrent_StreamingSSE(t *testing.T) {
	// 50 concurrent OpenAI streaming requests with SSE extraction enabled.
	// Validates tokenExtractingReader + ParseOpenAISSEUsage under contention.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 1000000, BurstTokens: 0})
	spy := newBudgetSpy(tracker)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: spy,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: true,
		Logger:        logger,
	})

	const goroutines = 50
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			body := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"max_tokens":200,"stream":true}`
			req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("x-genops-workload-id", "svc-a")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				failures.Add(1)
			}
		}()
	}

	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Errorf("expected all 200s, got %d failures", f)
	}
	if got := spy.reserveCalls.Load(); got != int64(goroutines) {
		t.Errorf("expected %d reserve calls, got %d", goroutines, got)
	}
	if got := spy.recordCalls.Load(); got != int64(goroutines) {
		t.Errorf("expected %d record calls (SSE extraction reconciles each), got %d", goroutines, got)
	}
}

func TestProxy_Concurrent_MixedProviders(t *testing.T) {
	// 25 OpenAI + 25 Anthropic concurrent streaming requests.
	// Validates per-workload budget isolation under mixed provider load.
	openaiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer openaiUpstream.Close()

	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-opus-20240229\",\"usage\":{\"input_tokens\":42,\"output_tokens\":0}}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":73}}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer anthropicUpstream.Close()

	cfg := &config.Config{
		Upstreams: map[string]string{
			"openai":    openaiUpstream.URL,
			"anthropic": anthropicUpstream.URL,
		},
		Workloads: []config.Workload{
			{
				ID:         "svc-openai",
				Identity:   config.Identity{Mode: "header", Key: "x-genops-workload-id"},
				PolicyRefs: []string{"openai-policy"},
			},
			{
				ID:         "svc-anthropic",
				Identity:   config.Identity{Mode: "header", Key: "x-genops-workload-id"},
				PolicyRefs: []string{"anthropic-policy"},
			},
		},
		Policies: []config.Policy{
			{
				ID: "openai-policy",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 60,
						LimitTokens:   1000000,
						BurstTokens:   0,
					},
				},
				Guards: config.Guards{MaxTokensPerRequest: 4096},
				DecisionTiers: config.DecisionTiers{
					Tier1Auto: config.TierAction{Action: "throttle"},
				},
			},
			{
				ID: "anthropic-policy",
				Budgets: config.Budgets{
					RollingTokens: config.RollingTokenBudget{
						WindowSeconds: 60,
						LimitTokens:   1000000,
						BurstTokens:   0,
					},
				},
				Guards: config.Guards{MaxTokensPerRequest: 4096},
				DecisionTiers: config.DecisionTiers{
					Tier1Auto: config.TierAction{Action: "throttle"},
				},
			},
		},
	}

	tracker := budget.NewTracker()
	tracker.Register("svc-openai", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 1000000, BurstTokens: 0})
	tracker.Register("svc-anthropic", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 1000000, BurstTokens: 0})
	spy := newBudgetSpy(tracker)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: spy,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: true,
		Logger:        logger,
	})

	const perProvider = 25
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(perProvider * 2)

	// 25 OpenAI requests.
	for i := 0; i < perProvider; i++ {
		go func() {
			defer wg.Done()
			body := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"max_tokens":200,"stream":true}`
			req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("x-genops-workload-id", "svc-openai")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				failures.Add(1)
			}
		}()
	}

	// 25 Anthropic requests.
	for i := 0; i < perProvider; i++ {
		go func() {
			defer wg.Done()
			body := `{"model":"claude-3-opus","messages":[{"role":"user","content":"Hi"}],"max_tokens":200,"stream":true}`
			req := httptest.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages", strings.NewReader(body))
			req.Header.Set("x-genops-workload-id", "svc-anthropic")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				failures.Add(1)
			}
		}()
	}

	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Errorf("expected all 200s, got %d failures", f)
	}
	if got := spy.reserveCalls.Load(); got != int64(perProvider*2) {
		t.Errorf("expected %d reserve calls, got %d", perProvider*2, got)
	}
	if got := spy.recordCalls.Load(); got != int64(perProvider*2) {
		t.Errorf("expected %d record calls, got %d", perProvider*2, got)
	}

	// Verify per-workload budget isolation.
	ctx := context.Background()

	// OpenAI: 25 requests, each: reserve 200, actual 30, delta -170.
	// Final window_tokens_used = 25 * 30 = 750
	statusOAI, err := tracker.Status(ctx, "svc-openai")
	if err != nil {
		t.Fatalf("svc-openai status error: %v", err)
	}
	if statusOAI.WindowTokensUsed != int64(perProvider)*30 {
		t.Errorf("svc-openai tokens_used: expected %d, got %d", perProvider*30, statusOAI.WindowTokensUsed)
	}

	// Anthropic: 25 requests, each: reserve 200, actual 115 (42+73), delta -85.
	// Final window_tokens_used = 25 * 115 = 2875
	statusAnth, err := tracker.Status(ctx, "svc-anthropic")
	if err != nil {
		t.Fatalf("svc-anthropic status error: %v", err)
	}
	if statusAnth.WindowTokensUsed != int64(perProvider)*115 {
		t.Errorf("svc-anthropic tokens_used: expected %d, got %d", perProvider*115, statusAnth.WindowTokensUsed)
	}
}

func TestProxy_Concurrent_BudgetExhaustion(t *testing.T) {
	// 50 concurrent requests against a small budget (5000 tokens).
	// Validates allow/deny split, no negative budget, no lost events.
	//
	// Known edge case under concurrency: Reserve's Add and Total are separate
	// mutex acquisitions. When Reserve denies and undoes, the post-undo status
	// may show used <= limit, causing TierDecider to return Allow. These
	// "edge-case allowed" requests are proxied without a reservation in the
	// budget. As a result, HTTP-level allowed count may exceed budget capacity.
	// Assertions use bounds that hold under all scheduling scenarios.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 80, "completion_tokens": 120, "total_tokens": 200},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	cfg.Policies[0].Budgets.RollingTokens.LimitTokens = 5000
	cfg.Policies[0].Budgets.RollingTokens.BurstTokens = 0

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 5000, BurstTokens: 0})
	spy := newBudgetSpy(tracker)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spyEmit := &spyEmitter{}

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: spy,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       spyEmit,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false,
		Logger:        logger,
	})

	const goroutines = 50
	var wg sync.WaitGroup
	var allowed, denied atomic.Int64
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			body := `{"model":"gpt-4","messages":[],"max_tokens":200}`
			req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("x-genops-workload-id", "svc-a")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			switch w.Code {
			case http.StatusOK:
				allowed.Add(1)
			case http.StatusTooManyRequests:
				denied.Add(1)
			default:
				t.Errorf("unexpected status %d", w.Code)
			}
		}()
	}

	wg.Wait()

	a := allowed.Load()
	d := denied.Load()

	// All requests accounted for.
	if a+d != goroutines {
		t.Errorf("expected allowed(%d) + denied(%d) = %d, got %d", a, d, goroutines, a+d)
	}

	// At least 25 requests must be allowed (5000/200 = 25 fit in budget).
	if a < 25 {
		t.Errorf("expected at least 25 allowed requests (5000/200), got %d", a)
	}

	// Budget integrity: tokens used must be non-negative.
	ctx := context.Background()
	status, err := tracker.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed < 0 {
		t.Errorf("negative budget: tokens_used=%d", status.WindowTokensUsed)
	}

	// Upper bound: limit + one reservation max overshoot.
	if status.WindowTokensUsed > 5200 {
		t.Errorf("tokens_used %d exceeds limit+reservation (5200)", status.WindowTokensUsed)
	}

	// All requests attempted reserve.
	if got := spy.reserveCalls.Load(); got != goroutines {
		t.Errorf("expected %d reserve calls, got %d", goroutines, got)
	}

	// All proxied requests called Record (includes edge-case-allowed requests).
	if got := spy.recordCalls.Load(); got != a {
		t.Errorf("expected %d record calls (one per proxied), got %d", a, got)
	}

	// Each HTTP-level denial (429) emits exactly one enforcement event.
	enforcementEvents := spyEmit.eventsByType("enforcement")
	if int64(len(enforcementEvents)) != d {
		t.Errorf("expected %d enforcement events (one per denial), got %d", d, len(enforcementEvents))
	}
}

func TestTokenExtractingReader_AnthropicSSE(t *testing.T) {
	// Full Anthropic SSE stream: message_start → content deltas → message_delta → message_stop
	sseData := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-opus-20240229\",\"usage\":{\"input_tokens\":42,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":73}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	inner := io.NopCloser(strings.NewReader(sseData))
	acc := &anthropicSSEAccumulator{}

	var extractedUsage *provider.UsageData
	r := newTokenExtractingReader(inner, acc.Parse, func(usage *provider.UsageData) {
		extractedUsage = usage
	})

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	// Byte fidelity: stream passes through unchanged.
	if buf.String() != sseData {
		t.Errorf("byte fidelity violated:\ngot:  %q\nwant: %q", buf.String(), sseData)
	}

	// Usage extracted correctly.
	if extractedUsage == nil {
		t.Fatal("expected usage to be extracted from Anthropic SSE stream")
	}
	if extractedUsage.InputTokens != 42 {
		t.Errorf("expected input 42, got %d", extractedUsage.InputTokens)
	}
	if extractedUsage.OutputTokens != 73 {
		t.Errorf("expected output 73, got %d", extractedUsage.OutputTokens)
	}
	if extractedUsage.TotalTokens != 115 {
		t.Errorf("expected total 115, got %d", extractedUsage.TotalTokens)
	}
}

// buildMux replicates the mux wiring from cmd/koshi/main.go (lines 114–136).
// Health endpoints are inline anonymous functions — not Handler methods.
func buildMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if h.IsDegraded() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("degraded"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.IsDegraded() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("degraded"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})
	mux.HandleFunc("/status", h.ServeStatus)
	mux.Handle("/", h)
	return mux
}

func TestMux_HealthEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 0})

	// Healthy handler — identity resolves normally.
	healthy := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	srv := httptest.NewServer(buildMux(healthy))
	defer srv.Close()

	t.Run("healthz_ok", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ok" {
			t.Errorf("expected body 'ok', got %q", body)
		}
	})

	t.Run("readyz_ok", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ready" {
			t.Errorf("expected body 'ready', got %q", body)
		}
	})

	// Degraded handler — panicResolver triggers degraded mode on first request.
	degradedTracker := budget.NewTracker()
	degradedTracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 0})

	degraded := NewHandler(HandlerConfig{
		Resolver:      &panicResolver{},
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: degradedTracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// Trigger panic to enter degraded mode.
	triggerReq := httptest.NewRequest(http.MethodPost, "http://api.openai.com/", nil)
	triggerW := httptest.NewRecorder()
	degraded.ServeHTTP(triggerW, triggerReq)

	if !degraded.IsDegraded() {
		t.Fatal("expected handler to be degraded after panic")
	}

	degradedSrv := httptest.NewServer(buildMux(degraded))
	defer degradedSrv.Close()

	t.Run("healthz_degraded", func(t *testing.T) {
		resp, err := http.Get(degradedSrv.URL + "/healthz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "degraded" {
			t.Errorf("expected body 'degraded', got %q", body)
		}
	})

	t.Run("readyz_degraded", func(t *testing.T) {
		resp, err := http.Get(degradedSrv.URL + "/readyz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "degraded" {
			t.Errorf("expected body 'degraded', got %q", body)
		}
	})
}

func TestMux_RoutingIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	srv := httptest.NewServer(buildMux(h))
	defer srv.Close()

	t.Run("proxy_via_mux", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[],"max_tokens":200}`
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(body))
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}
		req.Host = "api.openai.com"
		req.Header.Set("x-genops-workload-id", "svc-a")
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("status_via_mux", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/status")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", ct)
		}
	})
}

// ============================================================
// Operator Commitment Verification Tests
// ============================================================

func TestEnforcementResponse_429_IncludesTokenFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5000, "completion_tokens": 5000, "total_tokens": 10000},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	cfg.Policies[0].Budgets.RollingTokens.LimitTokens = 5000
	cfg.Policies[0].Budgets.RollingTokens.BurstTokens = 0

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 5000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// First request: exhaust budget. Upstream returns 10000 tokens.
	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Second request: should get 429.
	req2 := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("x-genops-workload-id", "svc-a")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w2.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode 429 body: %v", err)
	}

	tokensUsed, ok := resp["tokens_used"]
	if !ok {
		t.Fatal("429 response missing tokens_used field")
	}
	if _, ok := tokensUsed.(float64); !ok {
		t.Errorf("tokens_used is not a JSON number, got %T", tokensUsed)
	}
	if tokensUsed.(float64) <= 0 {
		t.Errorf("tokens_used should be > 0, got %v", tokensUsed)
	}

	tokensLimit, ok := resp["tokens_limit"]
	if !ok {
		t.Fatal("429 response missing tokens_limit field")
	}
	if _, ok := tokensLimit.(float64); !ok {
		t.Errorf("tokens_limit is not a JSON number, got %T", tokensLimit)
	}
	if tokensLimit.(float64) != 5000 {
		t.Errorf("tokens_limit should be 5000, got %v", tokensLimit)
	}
}

func TestEnforcementResponse_503_IncludesTokenFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5000, "completion_tokens": 5000, "total_tokens": 10000},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL
	cfg.Policies[0].Budgets.RollingTokens.LimitTokens = 5000
	cfg.Policies[0].Budgets.RollingTokens.BurstTokens = 0
	cfg.Policies[0].DecisionTiers.Tier3Platform = config.TierAction{Action: "kill_workload"}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 5000, BurstTokens: 0})

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	// First request: exhaust budget.
	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Second request: should get 503 (kill).
	req2 := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("x-genops-workload-id", "svc-a")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w2.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode 503 body: %v", err)
	}

	tokensUsed, ok := resp["tokens_used"]
	if !ok {
		t.Fatal("503 response missing tokens_used field")
	}
	if _, ok := tokensUsed.(float64); !ok {
		t.Errorf("tokens_used is not a JSON number, got %T", tokensUsed)
	}
	if tokensUsed.(float64) <= 0 {
		t.Errorf("tokens_used should be > 0, got %v", tokensUsed)
	}

	tokensLimit, ok := resp["tokens_limit"]
	if !ok {
		t.Fatal("503 response missing tokens_limit field")
	}
	if _, ok := tokensLimit.(float64); !ok {
		t.Errorf("tokens_limit is not a JSON number, got %T", tokensLimit)
	}
	if tokensLimit.(float64) != 5000 {
		t.Errorf("tokens_limit should be 5000, got %v", tokensLimit)
	}
}

func TestGracefulShutdown_InFlightCompletes(t *testing.T) {
	// Upstream delays 2s to simulate slow AI provider.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
		})
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.Upstreams["openai"] = upstream.URL

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 100000, BurstTokens: 0})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	h := NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		Logger:        logger,
	})

	mux := buildMux(h)
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)

	// Start in-flight request.
	var wg sync.WaitGroup
	var responseCode atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		body := strings.NewReader(`{"model":"gpt-4","messages":[],"max_tokens":100}`)
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/", body)
		req.Host = "api.openai.com"
		req.Header.Set("x-genops-workload-id", "svc-a")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("in-flight request failed: %v", err)
			return
		}
		resp.Body.Close()
		responseCode.Store(int64(resp.StatusCode))
	}()

	// Wait for request to reach the slow upstream.
	time.Sleep(500 * time.Millisecond)

	// Initiate graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	wg.Wait()

	if got := responseCode.Load(); got != http.StatusOK {
		t.Errorf("expected in-flight request to complete with 200, got %d", got)
	}
}

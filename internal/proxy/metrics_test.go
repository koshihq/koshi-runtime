package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/emit"
	"github.com/koshihq/koshi-runtime/internal/enforce"
	"github.com/koshihq/koshi-runtime/internal/fanout"
	"github.com/koshihq/koshi-runtime/internal/identity"
	"github.com/koshihq/koshi-runtime/internal/policy"
)

func counterValue(cv *prometheus.CounterVec, labels ...string) float64 {
	c, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		return 0
	}
	return testutil.ToFloat64(c)
}

func makeMetricsHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	cfg := testConfig()
	cfg.Upstreams["openai"] = upstreamURL

	tracker := budget.NewTracker()
	tracker.Register("svc-a", budget.BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	emitter := emit.NewLogEmitter(logger)
	t.Cleanup(emitter.Close)

	return NewHandler(HandlerConfig{
		Resolver:      identity.NewHeaderResolver("x-genops-workload-id"),
		PolicyEngine:  policy.NewMapEngine(cfg, logger),
		BudgetTracker: tracker,
		FanoutTracker: &fanout.NoOpTracker{},
		Decider:       enforce.NewTierDecider(),
		Emitter:       emitter,
		Upstreams:     cfg.Upstreams,
		SSEExtraction: false,
		Logger:        logger,
	})
}

func TestMetrics_RequestAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"total_tokens":100}}`))
	}))
	defer upstream.Close()

	h := makeMetricsHandler(t, upstream.URL)

	beforeReq := counterValue(requestsTotal, "svc-a", "standard", "allow")
	beforeTokens := counterValue(tokensUsedTotal, "svc-a", "standard", "reservation")
	beforeDec := counterValue(enforcementDecisionsTotal, "allow", "standard")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":500}`))
	req.Host = "api.openai.com"
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := counterValue(requestsTotal, "svc-a", "standard", "allow") - beforeReq; got != 1 {
		t.Errorf("requests_total{allow} delta = %v, want 1", got)
	}
	if got := counterValue(tokensUsedTotal, "svc-a", "standard", "reservation") - beforeTokens; got != 500 {
		t.Errorf("tokens_used_total{reservation} delta = %v, want 500", got)
	}
	if got := counterValue(enforcementDecisionsTotal, "allow", "standard") - beforeDec; got != 1 {
		t.Errorf("enforcement_decisions_total{allow} delta = %v, want 1", got)
	}
}

func TestMetrics_GuardRejected(t *testing.T) {
	h := makeMetricsHandler(t, "http://unused")

	beforeReq := counterValue(requestsTotal, "svc-a", "standard", "deny")
	beforeDec := counterValue(enforcementDecisionsTotal, "deny", "standard")

	// max_tokens exceeds guard (policy guard is 4096)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":50000}`))
	req.Host = "api.openai.com"
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if got := counterValue(requestsTotal, "svc-a", "standard", "deny") - beforeReq; got != 1 {
		t.Errorf("requests_total{deny} delta = %v, want 1", got)
	}
	if got := counterValue(enforcementDecisionsTotal, "deny", "standard") - beforeDec; got != 1 {
		t.Errorf("enforcement_decisions_total{deny} delta = %v, want 1", got)
	}
}

func TestMetrics_NonStreamingReconciliation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"total_tokens":350}}`))
	}))
	defer upstream.Close()

	h := makeMetricsHandler(t, upstream.URL)

	beforeActual := counterValue(tokensUsedTotal, "svc-a", "standard", "actual")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":500}`))
	req.Host = "api.openai.com"
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := counterValue(tokensUsedTotal, "svc-a", "standard", "actual") - beforeActual; got != 350 {
		t.Errorf("tokens_used_total{actual} delta = %v, want 350", got)
	}
}

func TestMetrics_5xxRefund(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	h := makeMetricsHandler(t, upstream.URL)

	beforeRefund := counterValue(tokensUsedTotal, "svc-a", "standard", "refund")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"max_tokens":500}`))
	req.Host = "api.openai.com"
	req.Header.Set("x-genops-workload-id", "svc-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := counterValue(tokensUsedTotal, "svc-a", "standard", "refund") - beforeRefund; got != 500 {
		t.Errorf("tokens_used_total{refund} delta = %v, want 500", got)
	}
}

func TestMetrics_EnforcementLatencyRegistered(t *testing.T) {
	// Verify the histogram metric is registered and can be collected.
	desc := make(chan *prometheus.Desc, 1)
	enforcementLatency.(prometheus.Collector).Describe(desc)
	d := <-desc
	if d == nil {
		t.Fatal("enforcement_latency_seconds not registered")
	}
}

func TestStatus_DroppedEvents(t *testing.T) {
	cfg := testConfig()
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
		Version:       "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	h.ServeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var raw map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if _, ok := raw["dropped_events"]; !ok {
		t.Error("expected dropped_events field in /status response")
	}
	if got := raw["dropped_events"].(float64); got != 0 {
		t.Errorf("expected dropped_events 0, got %v", got)
	}

	// Verify genops_spec_version.
	if got, ok := raw["genops_spec_version"].(string); !ok || got != "0.1.0" {
		t.Errorf("expected genops_spec_version 0.1.0, got %v", raw["genops_spec_version"])
	}
}

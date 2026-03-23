package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/emit"
	"github.com/koshihq/koshi-runtime/internal/enforce"
	"github.com/koshihq/koshi-runtime/internal/fanout"
	"github.com/koshihq/koshi-runtime/internal/genops"
	"github.com/koshihq/koshi-runtime/internal/identity"
	"github.com/koshihq/koshi-runtime/internal/policy"
	"github.com/koshihq/koshi-runtime/internal/provider"
)

// DefaultResponseHeaderTimeout is the time to wait for upstream response
// headers before timing out. Bounds hung connections independently of the
// server's WriteTimeout (300s).
const DefaultResponseHeaderTimeout = 30 * time.Second

// Handler implements the Koshi enforcement proxy.
type Handler struct {
	resolver              identity.Resolver
	policyEngine          policy.Engine
	budgetTracker         budget.Tracker
	fanoutTracker         fanout.Tracker
	decider               enforce.Decider
	emitter               emit.Emitter
	upstreams             map[string]string
	sseExtraction         bool
	degraded              atomic.Bool
	logger                *slog.Logger
	transport             *http.Transport
	version               string
	genopsSpecVersion     string
	mode                  string // "listener" or "enforcement"
	listenerAccountingKey string // budget tracker key for listener mode
}

// HandlerConfig holds configuration for creating a Handler.
type HandlerConfig struct {
	Resolver      identity.Resolver
	PolicyEngine  policy.Engine
	BudgetTracker budget.Tracker
	FanoutTracker fanout.Tracker
	Decider       enforce.Decider
	Emitter       emit.Emitter
	Upstreams     map[string]string
	SSEExtraction          bool
	ResponseHeaderTimeout  time.Duration
	Logger                 *slog.Logger
	Version                string
	GenOpsSpecVersion      string
	Mode                   string // "listener" or "enforcement"; defaults to "enforcement"
	ListenerAccountingKey  string // budget tracker key for listener mode
}

// NewHandler creates a new enforcement proxy handler.
func NewHandler(cfg HandlerConfig) *Handler {
	rht := cfg.ResponseHeaderTimeout
	if rht == 0 {
		rht = DefaultResponseHeaderTimeout
	}

	specVer := cfg.GenOpsSpecVersion
	if specVer == "" {
		specVer = genops.SpecVersion
	}

	mode := cfg.Mode
	if mode == "" {
		mode = "enforcement"
	}

	return &Handler{
		resolver:              cfg.Resolver,
		policyEngine:          cfg.PolicyEngine,
		budgetTracker:         cfg.BudgetTracker,
		fanoutTracker:         cfg.FanoutTracker,
		decider:               cfg.Decider,
		emitter:               cfg.Emitter,
		upstreams:             cfg.Upstreams,
		sseExtraction:         cfg.SSEExtraction,
		logger:                cfg.Logger,
		version:               cfg.Version,
		genopsSpecVersion:     specVer,
		mode:                  mode,
		listenerAccountingKey: cfg.ListenerAccountingKey,
		transport: &http.Transport{
			MaxIdleConns:           100,
			MaxIdleConnsPerHost:    20,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ResponseHeaderTimeout:  rht,
		},
	}
}

// isListenerMode returns true if operating in listener (shadow) mode.
func (h *Handler) isListenerMode() bool {
	return h.mode == "listener"
}

// accountingKey returns the budget tracker key for the current request.
// In listener mode, uses the policy-scoped key. In enforcement mode, uses the workload ID.
func (h *Handler) accountingKey(workloadID string) string {
	if h.isListenerMode() && h.listenerAccountingKey != "" {
		return h.listenerAccountingKey
	}
	return workloadID
}

// IsDegraded returns whether the handler is in degraded pass-through mode.
func (h *Handler) IsDegraded() bool {
	return h.degraded.Load()
}

// emitEvent emits an event with genops.spec.version injected. It clones the
// attributes map to avoid shared-map mutation across callers.
func (h *Handler) emitEvent(ctx context.Context, event emit.Event) {
	clone := make(map[string]any, len(event.Attributes)+1)
	for k, v := range event.Attributes {
		clone[k] = v
	}
	clone["genops.spec.version"] = h.genopsSpecVersion
	event.Attributes = clone
	h.emitter.Emit(ctx, event)
}

// ServeHTTP implements the full enforcement flow.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Top-level panic recovery → degrade to pass-through.
	defer func() {
		if rec := recover(); rec != nil {
			stack := debug.Stack()
			h.degraded.Store(true)
			h.emitEvent(r.Context(), emit.Event{
				Type:     "degraded_panic",
				Severity: "error",
				Attributes: map[string]any{
					"panic":    fmt.Sprintf("%v", rec),
					"stack":    string(stack),
					"state":    "degraded",
					"category": "system",
				},
			})
			h.logger.Error("koshi: panic recovered, entering degraded mode", "panic", rec, "stack", string(stack))
			// Try to serve a 500 response if headers haven't been sent.
			writeErrorResponse(w, http.StatusInternalServerError, "service_degraded", "system", "internal_protection_triggered", "degraded", enforce.ReasonSystemDegraded)
		}
	}()

	// Degraded mode: pass-through with no enforcement.
	if h.degraded.Load() {
		h.emitEvent(r.Context(), emit.Event{
			Type:     "degraded_passthrough",
			Severity: "warn",
		})
		h.proxyDirect(w, r)
		return
	}

	// Step 1: Resolve workload identity.
	ident, err := h.resolver.Resolve(r)
	if err != nil {
		// Step 2: Check for default policy fallback.
		pol, ok := h.policyEngine.Lookup(identity.WorkloadIdentity{})
		if !ok {
			if h.isListenerMode() {
				// Listener mode: emit shadow event, proxy directly.
				h.emitEvent(r.Context(), shadowEvent(
					identity.WorkloadIdentity{}, ShadowWouldReject,
					enforce.ReasonIdentityMissing, "", "", 0, 0,
				))
				listenerDecisionsTotal.WithLabelValues("", ShadowWouldReject, enforce.ReasonIdentityMissing).Inc()
				h.proxyDirect(w, r)
				return
			}
			h.emitEvent(r.Context(), emit.Event{
				Type:       "identity_rejected",
				Severity:   "warn",
				Attributes: map[string]any{"error": err.Error(), "reason_code": enforce.ReasonIdentityMissing},
			})
			enforcementDecisionsTotal.WithLabelValues("deny", "none").Inc()
			writeErrorResponse(w, http.StatusForbidden, "identity_required", "enforcement", "missing_workload_identity", "reject", enforce.ReasonIdentityMissing)
			return
		}
		// Use default policy with empty identity.
		h.handleWithPolicy(w, r, identity.WorkloadIdentity{WorkloadID: "_default"}, pol)
		return
	}

	// Step 3: Lookup policy.
	pol, ok := h.policyEngine.Lookup(ident)
	if !ok {
		if h.isListenerMode() {
			// Listener mode: emit shadow event, proxy directly.
			h.emitEvent(r.Context(), shadowEvent(
				ident, ShadowWouldReject,
				enforce.ReasonPolicyNotFound, "", "", 0, 0,
			))
			listenerDecisionsTotal.WithLabelValues(ident.Namespace, ShadowWouldReject, enforce.ReasonPolicyNotFound).Inc()
			h.proxyDirect(w, r)
			return
		}
		h.emitEvent(r.Context(), emit.Event{
			Type:       "policy_rejected",
			WorkloadID: ident.WorkloadID,
			Severity:   "warn",
			Attributes: map[string]any{"reason_code": enforce.ReasonPolicyNotFound},
		})
		requestsTotal.WithLabelValues(ident.WorkloadID, "none", "deny").Inc()
		enforcementDecisionsTotal.WithLabelValues("deny", "none").Inc()
		writeErrorResponse(w, http.StatusForbidden, "policy_not_found", "enforcement", "no_matching_policy", "reject", enforce.ReasonPolicyNotFound)
		return
	}

	h.handleWithPolicy(w, r, ident, pol)
}

func (h *Handler) handleWithPolicy(w http.ResponseWriter, r *http.Request, ident identity.WorkloadIdentity, pol *config.Policy) {
	ctx := r.Context()

	// Step 4: Read and buffer request body for max_tokens extraction.
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// Extract max_tokens for reservation estimate.
	maxTokens := extractMaxTokens(bodyBytes)
	providerType := provider.DetectProvider(r.Host)
	if maxTokens == 0 {
		maxTokens = provider.DefaultMaxTokens(providerType)
	}

	streaming := isStreamingRequest(bodyBytes)

	enforcementStart := time.Now()

	providerName := provider.Name(providerType)
	acctKey := h.accountingKey(ident.WorkloadID)

	// Step 5: Check per-request guard.
	if guardDec := enforce.CheckPerRequestGuard(pol, maxTokens); guardDec != nil {
		if h.isListenerMode() {
			listenerLatency.Observe(time.Since(enforcementStart).Seconds())
			h.emitEvent(ctx, shadowEvent(ident, ShadowWouldThrottle, guardDec.ReasonCode, providerName, "", maxTokens, 0))
			listenerDecisionsTotal.WithLabelValues(ident.Namespace, ShadowWouldThrottle, guardDec.ReasonCode).Inc()
			// Continue to proxy — do not return.
		} else {
			enforcementLatency.Observe(time.Since(enforcementStart).Seconds())
			h.emitEvent(ctx, emit.Event{
				Type:       "guard_rejected",
				WorkloadID: ident.WorkloadID,
				Severity:   "warn",
				Attributes: map[string]any{"reason": guardDec.Reason, "reason_code": guardDec.ReasonCode},
			})
			requestsTotal.WithLabelValues(ident.WorkloadID, pol.ID, "deny").Inc()
			enforcementDecisionsTotal.WithLabelValues("deny", pol.ID).Inc()
			writeEnforcementResponse(w, *guardDec)
			return
		}
	}

	// Step 6: Reserve tokens (using accounting key).
	budgetStatus, allowed, err := h.budgetTracker.Reserve(ctx, acctKey, maxTokens)
	if err != nil {
		if h.isListenerMode() {
			// Listener mode: log and continue to proxy without budget tracking.
			h.logger.Warn("listener: budget reserve failed, continuing", "workload_id", ident.WorkloadID, "error", err)
		} else {
			enforcementLatency.Observe(time.Since(enforcementStart).Seconds())
			h.logger.Error("budget: unregistered workload", "workload_id", ident.WorkloadID, "error", err)
			enforcementDecisionsTotal.WithLabelValues("error", "none").Inc()
			writeErrorResponse(w, http.StatusInternalServerError, "budget_config_error", "system", "unregistered_workload", "error", enforce.ReasonBudgetConfigError)
			return
		}
	}

	// Step 7: Fanout check (no-op in v1).
	fanoutStatus, fanoutAllowed := h.fanoutTracker.Increment(ctx, "", 0)

	// Step 8: Evaluate enforcement decision.
	if !allowed || !fanoutAllowed {
		decision := h.decider.Evaluate(pol, budgetStatus, &fanoutStatus)
		if decision.Action != enforce.ActionAllow {
			if h.isListenerMode() {
				// Listener mode: emit shadow, continue to proxy.
				shadowDec := ShadowWouldThrottle
				if decision.Action == enforce.ActionKill {
					shadowDec = ShadowWouldKill
				}
				listenerLatency.Observe(time.Since(enforcementStart).Seconds())
				h.emitEvent(ctx, shadowEvent(ident, shadowDec, decision.ReasonCode, providerName, "", maxTokens, 0))
				listenerDecisionsTotal.WithLabelValues(ident.Namespace, shadowDec, decision.ReasonCode).Inc()
				// Fall through to proxy.
			} else {
				enforcementLatency.Observe(time.Since(enforcementStart).Seconds())
				h.emitEvent(ctx, emit.Event{
					Type:       "enforcement",
					WorkloadID: ident.WorkloadID,
					Severity:   "warn",
					Attributes: map[string]any{
						"action":          actionName(decision.Action),
						"decision":        actionName(decision.Action),
						"category":        "enforcement",
						"tier":            strconv.Itoa(decision.Tier),
						"reason":          decision.Reason,
						"reason_code":     decision.ReasonCode,
						"tokens_used":     budgetStatus.WindowTokensUsed,
						"tokens_limit":    budgetStatus.WindowTokensLimit,
						"burst_remaining": budgetStatus.BurstRemaining,
						"reserved_tokens": maxTokens,
					},
				})
				decision.TokensUsed = budgetStatus.WindowTokensUsed
				decision.TokensLimit = budgetStatus.WindowTokensLimit
				requestsTotal.WithLabelValues(ident.WorkloadID, pol.ID, "deny").Inc()
				enforcementDecisionsTotal.WithLabelValues("deny", pol.ID).Inc()
				writeEnforcementResponse(w, decision)
				return
			}
		}
	}

	// Step 9: Proxy to upstream.
	if h.isListenerMode() {
		listenerLatency.Observe(time.Since(enforcementStart).Seconds())
		h.emitEvent(ctx, shadowEvent(ident, ShadowAllow, "", providerName, "", maxTokens, 0))
		listenerDecisionsTotal.WithLabelValues(ident.Namespace, ShadowAllow, "").Inc()
		listenerTokensTotal.WithLabelValues(ident.Namespace, providerName, "reservation").Add(float64(maxTokens))
	} else {
		enforcementLatency.Observe(time.Since(enforcementStart).Seconds())
		h.emitEvent(ctx, emit.Event{
			Type:       "request_allowed",
			WorkloadID: ident.WorkloadID,
			Severity:   "info",
			Attributes: map[string]any{
				"estimated_tokens": strconv.FormatInt(maxTokens, 10),
				"streaming":        strconv.FormatBool(streaming),
				"phase":            "reservation",
			},
		})
		requestsTotal.WithLabelValues(ident.WorkloadID, pol.ID, "allow").Inc()
		tokensUsedTotal.WithLabelValues(ident.WorkloadID, pol.ID, "reservation").Add(float64(maxTokens))
		enforcementDecisionsTotal.WithLabelValues("allow", pol.ID).Inc()
	}

	h.proxyWithEnforcement(w, r, ident, pol, providerType, maxTokens, streaming, bodyBytes, acctKey)
}

func (h *Handler) proxyWithEnforcement(
	w http.ResponseWriter,
	r *http.Request,
	ident identity.WorkloadIdentity,
	pol *config.Policy,
	providerType provider.Type,
	reservedTokens int64,
	streaming bool,
	bodyBytes []byte,
	acctKey string,
) {
	upstreamURL := h.resolveUpstream(r)
	if upstreamURL == nil {
		writeErrorResponse(w, http.StatusBadGateway, "upstream_not_configured", "system", "no_upstream_for_provider", "reject", enforce.ReasonUpstreamNotConfigured)
		return
	}

	// For OpenAI streaming with SSE extraction, inject stream_options.
	if streaming && providerType == provider.OpenAI && h.sseExtraction {
		modified, err := injectStreamOptions(bodyBytes)
		if err == nil {
			bodyBytes = modified
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			r.ContentLength = int64(len(bodyBytes))
		}
	} else {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = upstreamURL
			req.Host = upstreamURL.Host
		},
		Transport: h.transport,
		FlushInterval: -1, // Immediate flush for SSE.
		ModifyResponse: func(resp *http.Response) error {
			ct := resp.Header.Get("Content-Type")

			// On upstream 5xx: return reservation to budget.
			if resp.StatusCode >= 500 {
				h.budgetTracker.Record(r.Context(), budget.UsageReport{
					WorkloadID: acctKey,
					Tokens:     -reservedTokens, // Return full reservation.
					Timestamp:  time.Now(),
				})
				h.emitEvent(r.Context(), emit.Event{
					Type:       emit.EventBudgetReconciled,
					WorkloadID: ident.WorkloadID,
					Severity:   "info",
					Attributes: map[string]any{
						"reserved_tokens": reservedTokens,
						"actual_tokens":   int64(0),
						"delta_tokens":    -reservedTokens,
						"phase":           "refund",
					},
				})
				tokensUsedTotal.WithLabelValues(ident.WorkloadID, pol.ID, "refund").Add(float64(reservedTokens))
				h.emitEvent(r.Context(), emit.Event{
					Type:       "upstream_error",
					WorkloadID: ident.WorkloadID,
					Severity:   "warn",
					Attributes: map[string]any{
						"status": strconv.Itoa(resp.StatusCode),
					},
				})
				return nil
			}

			if isSSEResponse(ct) {
				// SSE streaming response.
				var parseFunc sseParseFunc
				if streaming && h.sseExtraction {
					switch providerType {
					case provider.OpenAI:
						parseFunc = provider.ParseOpenAISSEUsage
					case provider.Anthropic:
						parseFunc = (&anthropicSSEAccumulator{}).Parse
					}
				}
				if parseFunc != nil {
					resp.Body = newTokenExtractingReader(resp.Body, parseFunc, func(usage *provider.UsageData) {
						if usage != nil {
							delta := usage.TotalTokens - reservedTokens
							h.budgetTracker.Record(r.Context(), budget.UsageReport{
								WorkloadID: acctKey,
								Tokens:     delta,
								Timestamp:  time.Now(),
							})
							if delta != 0 {
								h.emitEvent(r.Context(), emit.Event{
									Type:       emit.EventBudgetReconciled,
									WorkloadID: ident.WorkloadID,
									Severity:   "info",
									Attributes: map[string]any{
										"reserved_tokens": reservedTokens,
										"actual_tokens":   usage.TotalTokens,
										"delta_tokens":    delta,
										"phase":           "actual",
									},
								})
							}
							tokensUsedTotal.WithLabelValues(ident.WorkloadID, pol.ID, "actual").Add(float64(usage.TotalTokens))
						}
					})
				}
				// If not extracting (unsupported provider or SSE disabled), reservation stands.
				return nil
			}

			// Non-streaming response: extract usage from body.
			return h.handleNonStreamingResponse(r.Context(), resp, ident, pol.ID, providerType, reservedTokens, acctKey)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			h.emitEvent(r.Context(), emit.Event{
				Type:       "upstream_timeout",
				WorkloadID: ident.WorkloadID,
				Severity:   "error",
				Attributes: map[string]any{"error": err.Error()},
			})
			// Reservation stands on timeout (conservative).
			writeErrorResponse(w, http.StatusGatewayTimeout, "upstream_timeout", "system", "upstream_did_not_respond", "reject", enforce.ReasonUpstreamTimeout)
		},
	}

	proxy.ServeHTTP(w, r)
}

func (h *Handler) handleNonStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	ident identity.WorkloadIdentity,
	policyID string,
	providerType provider.Type,
	reservedTokens int64,
	acctKey string,
) error {
	bodyBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	// Replace body for downstream consumption.
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	resp.ContentLength = int64(len(bodyBytes))

	// Extract usage.
	parser := provider.GetParser(providerType)
	if parser == nil {
		return nil // Unknown provider, reservation stands.
	}

	usage, err := parser.ParseUsage(bodyBytes)
	if err != nil || usage == nil {
		return nil // Can't parse, reservation stands.
	}

	// Reconcile: delta = actual - reserved.
	delta := usage.TotalTokens - reservedTokens
	h.budgetTracker.Record(ctx, budget.UsageReport{
		WorkloadID: acctKey,
		Tokens:     delta,
		Timestamp:  time.Now(),
	})

	if delta != 0 {
		h.emitEvent(ctx, emit.Event{
			Type:       emit.EventBudgetReconciled,
			WorkloadID: ident.WorkloadID,
			Severity:   "info",
			Attributes: map[string]any{
				"reserved_tokens": reservedTokens,
				"actual_tokens":   usage.TotalTokens,
				"delta_tokens":    delta,
				"phase":           "actual",
			},
		})
	}
	tokensUsedTotal.WithLabelValues(ident.WorkloadID, policyID, "actual").Add(float64(usage.TotalTokens))

	return nil
}

// proxyDirect proxies a request without any enforcement (degraded mode).
func (h *Handler) proxyDirect(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.resolveUpstream(r)
	if upstreamURL == nil {
		writeErrorResponse(w, http.StatusBadGateway, "upstream_not_configured", "system", "no_upstream_for_provider", "reject", enforce.ReasonUpstreamNotConfigured)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = upstreamURL
			req.Host = upstreamURL.Host
		},
		Transport:     h.transport,
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// resolveUpstream determines the upstream URL from the request and upstream config.
// Routing is decoupled from policy — based on request Host, not resolved policy.
func (h *Handler) resolveUpstream(r *http.Request) *url.URL {
	host := strings.ToLower(r.Host)

	// Try to match against configured upstreams by provider detection.
	pt := provider.DetectProvider(host)
	var baseURL string

	switch pt {
	case provider.OpenAI:
		baseURL = h.upstreams["openai"]
	case provider.Anthropic:
		baseURL = h.upstreams["anthropic"]
	case provider.Google:
		baseURL = h.upstreams["google"]
	}

	if baseURL == "" {
		// Known provider detected but not configured in upstreams → reject.
		// Only fall back to the request's original URL for unknown providers
		// (e.g., explicit proxy mode where the client sets the full URL).
		if pt == provider.Unknown && r.URL.Scheme != "" && r.URL.Host != "" {
			return r.URL
		}
		return nil
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}

	u.Path = r.URL.Path
	u.RawQuery = r.URL.RawQuery
	return u
}

func writeErrorResponse(w http.ResponseWriter, statusCode int, errorCode, category, reason, decision, reasonCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GenOps-Decision", decision)
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error":       errorCode,
		"category":    category,
		"reason":      reason,
		"reason_code": reasonCode,
	})
}

func writeEnforcementResponse(w http.ResponseWriter, dec enforce.Decision) {
	switch dec.Action {
	case enforce.ActionThrottle:
		if dec.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(dec.RetryAfter.Seconds())))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GenOps-Decision", "throttle")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error":        "rate_limited",
			"category":     "enforcement",
			"reason":       dec.Reason,
			"reason_code":  dec.ReasonCode,
			"tokens_used":  dec.TokensUsed,
			"tokens_limit": dec.TokensLimit,
		})
	case enforce.ActionKill:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GenOps-Decision", "kill")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{
			"error":        "workload_killed",
			"category":     "enforcement",
			"reason":       dec.Reason,
			"reason_code":  dec.ReasonCode,
			"tokens_used":  dec.TokensUsed,
			"tokens_limit": dec.TokensLimit,
		})
	}
}

func actionName(a enforce.Action) string {
	switch a {
	case enforce.ActionAllow:
		return "allow"
	case enforce.ActionThrottle:
		return "throttle"
	case enforce.ActionKill:
		return "kill"
	default:
		return "unknown"
	}
}

// statusResponse is the JSON response DTO for GET /status.
type statusResponse struct {
	Version           string                       `json:"version"`
	GenOpsSpecVersion string                       `json:"genops_spec_version"`
	DroppedEvents     int64                        `json:"dropped_events"`
	Workloads         map[string]workloadStatusDTO `json:"workloads"`
}

type workloadStatusDTO struct {
	LimitTokens      int64 `json:"limit_tokens"`
	WindowTokensUsed int64 `json:"window_tokens_used"`
	BurstRemaining   int64 `json:"burst_remaining"`
	WindowSeconds    int   `json:"window_seconds"`
}

// ServeStatus handles GET /status — returns per-workload budget state.
func (h *Handler) ServeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := h.budgetTracker.StatusAll(r.Context())

	keys := make([]string, 0, len(all))
	for id := range all {
		keys = append(keys, id)
	}
	sort.Strings(keys)

	workloads := make(map[string]workloadStatusDTO, len(all))
	for _, id := range keys {
		ws := all[id]
		workloads[id] = workloadStatusDTO{
			LimitTokens:      ws.LimitTokens,
			WindowTokensUsed: ws.WindowTokensUsed,
			BurstRemaining:   ws.BurstRemaining,
			WindowSeconds:    ws.WindowSeconds,
		}
	}

	resp := statusResponse{
		Version:           h.version,
		GenOpsSpecVersion: h.genopsSpecVersion,
		DroppedEvents:     h.emitter.Dropped(),
		Workloads:         workloads,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("status: failed to encode response", "error", err)
	}
}

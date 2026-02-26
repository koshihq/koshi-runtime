package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/koshihq/koshi-runtime/internal/budget"
	"github.com/koshihq/koshi-runtime/internal/config"
	"github.com/koshihq/koshi-runtime/internal/emit"
	"github.com/koshihq/koshi-runtime/internal/enforce"
	"github.com/koshihq/koshi-runtime/internal/fanout"
	"github.com/koshihq/koshi-runtime/internal/genops"
	"github.com/koshihq/koshi-runtime/internal/identity"
	"github.com/koshihq/koshi-runtime/internal/policy"
	"github.com/koshihq/koshi-runtime/internal/proxy"
	"github.com/koshihq/koshi-runtime/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Load config.
	configPath := os.Getenv("KOSHI_CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/koshi/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err, "path", configPath)
		os.Exit(1)
	}

	logger.Info("config loaded", "workloads", len(cfg.Workloads), "policies", len(cfg.Policies))

	// Wire dependencies.
	// Use the first workload's identity key. Safe — config validation
	// ensures all workloads share the same identity key (v1 constraint).
	headerKey := "x-genops-workload-id"
	if len(cfg.Workloads) > 0 && cfg.Workloads[0].Identity.Key != "" {
		headerKey = cfg.Workloads[0].Identity.Key
	}

	resolver := identity.NewHeaderResolver(headerKey)
	policyEngine := policy.NewMapEngine(cfg, logger)
	emitter := emit.NewLogEmitter(logger)
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "koshi",
			Name:      "emitter_dropped_total",
			Help:      "Total emitter events dropped due to backpressure.",
		},
		func() float64 { return float64(emitter.Dropped()) },
	))

	// Create budget tracker and register each workload with its resolved
	// policy's budget params.
	budgetTracker := budget.NewTracker()

	for _, w := range cfg.Workloads {
		pol, ok := policyEngine.Lookup(identity.WorkloadIdentity{WorkloadID: w.ID})
		if !ok {
			// This shouldn't happen — config validation ensures policy_refs are valid.
			logger.Error("workload has no resolvable policy", "workload_id", w.ID)
			os.Exit(1)
		}
		rt := pol.Budgets.RollingTokens
		budgetTracker.Register(w.ID, budget.BudgetParams{
			WindowSeconds: rt.WindowSeconds,
			LimitTokens:   rt.LimitTokens,
			BurstTokens:   rt.BurstTokens,
		})
	}

	// Register _default workload if default_policy is configured.
	// Why: proxy.go:131 hardcodes WorkloadID="_default" when identity resolution
	// fails but default policy exists. That path calls Reserve("_default", ...),
	// so the tracker must have "_default" registered with the default policy's
	// budget params. If default_policy is nil, proxy returns 403 before reaching
	// Reserve — no registration needed.
	if cfg.DefaultPolicy != nil {
		rt := cfg.DefaultPolicy.Budgets.RollingTokens
		budgetTracker.Register("_default", budget.BudgetParams{
			WindowSeconds: rt.WindowSeconds,
			LimitTokens:   rt.LimitTokens,
			BurstTokens:   rt.BurstTokens,
		})
	}

	fanoutTracker := &fanout.NoOpTracker{}
	decider := enforce.NewTierDecider()

	handler := proxy.NewHandler(proxy.HandlerConfig{
		Resolver:              resolver,
		PolicyEngine:          policyEngine,
		BudgetTracker:         budgetTracker,
		FanoutTracker:         fanoutTracker,
		Decider:               decider,
		Emitter:               emitter,
		Upstreams:             cfg.Upstreams,
		SSEExtraction:         cfg.SSEExtractionEnabled(),
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeout) * time.Second,
		Logger:                logger,
		Version:               version.Version,
		GenOpsSpecVersion:     genops.SpecVersion,
	})

	// Build HTTP mux with health endpoints.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if handler.IsDegraded() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("degraded"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if handler.IsDegraded() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("degraded"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})
	mux.HandleFunc("/status", handler.ServeStatus)
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", handler)

	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start server.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("koshi starting", "addr", listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Wait for shutdown signal or server error.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-stop:
		logger.Info("shutting down")
	case err := <-serverErr:
		logger.Error("server error, shutting down", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	emitter.Close()
	logger.Info("shutdown complete")
}

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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("stream", "runtime")

	// Role dispatch.
	switch os.Getenv("KOSHI_ROLE") {
	case "injector":
		runInjector(logger)
		return
	case "certgen-secret":
		runCertgenSecret(logger)
		return
	case "certgen-cabundle":
		runCertgenCABundle(logger)
		return
	}

	eventLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("stream", "event")

	// Load config.
	configPath := os.Getenv("KOSHI_CONFIG_PATH")

	var cfg *config.Config
	if configPath != "" {
		var err error
		cfg, err = config.Load(configPath)
		if err != nil {
			logger.Error("failed to load config", "error", err, "path", configPath)
			os.Exit(1)
		}
		logger.Info("config loaded", "mode", cfg.Mode.Type, "source", configPath, "workloads", len(cfg.Workloads), "policies", len(cfg.Policies))
	} else {
		cfg = config.DefaultListenerConfig()
		logger.Info("no config path set, using default listener config", "mode", cfg.Mode.Type)
	}

	// Wire dependencies based on mode.
	emitter := emit.NewLogEmitter(eventLogger)
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "koshi",
			Name:      "emitter_dropped_total",
			Help:      "Total emitter events dropped due to backpressure.",
		},
		func() float64 { return float64(emitter.Dropped()) },
	))

	budgetTracker := budget.NewTracker()
	fanoutTracker := &fanout.NoOpTracker{}
	decider := enforce.NewTierDecider()

	var resolver identity.Resolver
	var policyEngine policy.Engine
	var listenerAccountingKey string

	if cfg.Mode.Type == "listener" {
		// Listener mode: use PodResolver (env vars set by webhook at admission).
		resolver = identity.NewPodResolver()

		// Check for policy override (annotation-driven).
		policyOverride := os.Getenv("KOSHI_POLICY_OVERRIDE")
		if policyOverride != "" {
			// Look up the named policy and use OverrideEngine.
			var overridePol *config.Policy
			for i := range cfg.Policies {
				if cfg.Policies[i].ID == policyOverride {
					overridePol = &cfg.Policies[i]
					break
				}
			}
			if overridePol == nil {
				logger.Error("KOSHI_POLICY_OVERRIDE references unknown policy", "policy_id", policyOverride)
				os.Exit(1)
			}
			policyEngine = policy.NewOverrideEngine(overridePol)
			listenerAccountingKey = "listener_policy/" + policyOverride
			rt := overridePol.Budgets.RollingTokens
			budgetTracker.Register(listenerAccountingKey, budget.BudgetParams{
				WindowSeconds: rt.WindowSeconds,
				LimitTokens:   rt.LimitTokens,
				BurstTokens:   rt.BurstTokens,
			})
			logger.Info("listener: using policy override", "policy_id", policyOverride, "accounting_key", listenerAccountingKey)
		} else {
			// No override — use default policy with MapEngine.
			policyEngine = policy.NewMapEngine(cfg, logger)
			listenerAccountingKey = "_default"
			if cfg.DefaultPolicy != nil {
				rt := cfg.DefaultPolicy.Budgets.RollingTokens
				budgetTracker.Register("_default", budget.BudgetParams{
					WindowSeconds: rt.WindowSeconds,
					LimitTokens:   rt.LimitTokens,
					BurstTokens:   rt.BurstTokens,
				})
			}
			logger.Info("listener: using default policy", "accounting_key", listenerAccountingKey)
		}
	} else {
		// Enforcement mode: use HeaderResolver.
		headerKey := "x-genops-workload-id"
		if len(cfg.Workloads) > 0 && cfg.Workloads[0].Identity.Key != "" {
			headerKey = cfg.Workloads[0].Identity.Key
		}
		resolver = identity.NewHeaderResolver(headerKey)
		policyEngine = policy.NewMapEngine(cfg, logger)

		// Register each workload with its resolved policy's budget params.
		for _, w := range cfg.Workloads {
			pol, ok := policyEngine.Lookup(identity.WorkloadIdentity{WorkloadID: w.ID})
			if !ok {
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
		if cfg.DefaultPolicy != nil {
			rt := cfg.DefaultPolicy.Budgets.RollingTokens
			budgetTracker.Register("_default", budget.BudgetParams{
				WindowSeconds: rt.WindowSeconds,
				LimitTokens:   rt.LimitTokens,
				BurstTokens:   rt.BurstTokens,
			})
		}
	}

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
		Mode:                  cfg.Mode.Type,
		ListenerAccountingKey: listenerAccountingKey,
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

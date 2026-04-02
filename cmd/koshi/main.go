package main

import (
	"context"
	"fmt"
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
	koshiMode := os.Getenv("KOSHI_MODE")

	var cfg *config.Config
	if configPath != "" && os.Getenv("KOSHI_POD_NAMESPACE") != "" {
		// Sidecar file-config mode: ConfigMap-mounted custom config in an injected sidecar.
		var err error
		cfg, err = config.Parse(configPath)
		if err != nil {
			logger.Error("failed to parse sidecar config", "error", err, "path", configPath)
			os.Exit(1)
		}
		if err := cfg.ValidateSidecarConfig(); err != nil {
			logger.Error("invalid sidecar config", "error", err, "path", configPath)
			os.Exit(1)
		}

		// Read pod identity from webhook-injected env vars.
		ns := os.Getenv("KOSHI_POD_NAMESPACE")
		wKind := os.Getenv("KOSHI_WORKLOAD_KIND")
		wName := os.Getenv("KOSHI_WORKLOAD_NAME")
		if wKind == "" || wName == "" {
			logger.Error("sidecar file-config mode requires KOSHI_WORKLOAD_KIND and KOSHI_WORKLOAD_NAME",
				"kind", wKind, "name", wName)
			os.Exit(1)
		}

		// Mode: annotation/env only. cfg.Mode.Type is ignored for sidecars.
		resolvedMode := "listener"
		if koshiMode == "enforcement" {
			resolvedMode = "enforcement"
		}
		if cfg.Mode.Type != "" && cfg.Mode.Type != resolvedMode {
			logger.Warn("sidecar config mode.type differs from annotation — using annotation",
				"config_mode", cfg.Mode.Type, "annotation_mode", resolvedMode)
		}
		cfg.Mode.Type = resolvedMode

		// Policy: require explicit selection via runtime.getkoshi.ai/policy annotation.
		policyOverride := os.Getenv("KOSHI_POLICY_OVERRIDE")
		if policyOverride == "" {
			logger.Error("sidecar custom config requires runtime.getkoshi.ai/policy annotation (KOSHI_POLICY_OVERRIDE)")
			os.Exit(1)
		}
		found := false
		for _, p := range cfg.Policies {
			if p.ID == policyOverride {
				found = true
				break
			}
		}
		if !found {
			logger.Error("KOSHI_POLICY_OVERRIDE references unknown policy in sidecar config", "policy_id", policyOverride)
			os.Exit(1)
		}

		// Synthesize workload from pod identity.
		workloadID := fmt.Sprintf("%s/%s/%s", ns, wKind, wName)
		cfg.Workloads = append(cfg.Workloads, config.Workload{
			ID:         workloadID,
			Type:       "sidecar",
			Identity:   config.Identity{Mode: "pod"},
			PolicyRefs: []string{policyOverride},
		})

		// Default listen address for sidecars.
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = ":15080"
		}

		logger.Info("sidecar file-config mode", "mode", resolvedMode, "workload_id", workloadID, "policy", policyOverride, "source", configPath)
	} else if configPath != "" {
		var err error
		cfg, err = config.Load(configPath)
		if err != nil {
			logger.Error("failed to load config", "error", err, "path", configPath)
			os.Exit(1)
		}
		logger.Info("config loaded", "mode", cfg.Mode.Type, "source", configPath, "workloads", len(cfg.Workloads), "policies", len(cfg.Policies))
	} else if koshiMode == "enforcement" {
		cfg = config.DefaultEnforcementSidecarConfig()

		// Synthesize single workload from pod identity env vars.
		ns := os.Getenv("KOSHI_POD_NAMESPACE")
		wKind := os.Getenv("KOSHI_WORKLOAD_KIND")
		wName := os.Getenv("KOSHI_WORKLOAD_NAME")
		if ns == "" || wKind == "" || wName == "" {
			logger.Error("sidecar enforcement mode requires KOSHI_POD_NAMESPACE, KOSHI_WORKLOAD_KIND, and KOSHI_WORKLOAD_NAME",
				"namespace", ns, "kind", wKind, "name", wName)
			os.Exit(1)
		}

		// Resolve policy: use override annotation or default to sidecar-baseline.
		policyID := config.DefaultSidecarPolicyID
		if override := os.Getenv("KOSHI_POLICY_OVERRIDE"); override != "" {
			found := false
			for _, p := range cfg.Policies {
				if p.ID == override {
					found = true
					break
				}
			}
			if !found {
				logger.Error("KOSHI_POLICY_OVERRIDE references unknown built-in policy", "policy_id", override)
				os.Exit(1)
			}
			policyID = override
		}

		// WorkloadID must match PodResolver.Resolve() format exactly.
		workloadID := fmt.Sprintf("%s/%s/%s", ns, wKind, wName)
		cfg.Workloads = append(cfg.Workloads, config.Workload{
			ID:         workloadID,
			Type:       "sidecar",
			Identity:   config.Identity{Mode: "pod"},
			PolicyRefs: []string{policyID},
		})
		logger.Info("sidecar enforcement mode", "workload_id", workloadID, "policy", policyID)
	} else if koshiMode == "" {
		cfg = config.DefaultListenerConfig()
		logger.Info("no config path set, using default listener config", "mode", cfg.Mode.Type)
	} else {
		logger.Error("unsupported KOSHI_MODE value", "mode", koshiMode)
		os.Exit(1)
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
		// Enforcement mode: choose resolver based on workload identity mode.
		if len(cfg.Workloads) > 0 && cfg.Workloads[0].Identity.Mode == "pod" {
			resolver = identity.NewPodResolver()
		} else {
			headerKey := "x-genops-workload-id"
			if len(cfg.Workloads) > 0 && cfg.Workloads[0].Identity.Key != "" {
				headerKey = cfg.Workloads[0].Identity.Key
			}
			resolver = identity.NewHeaderResolver(headerKey)
		}
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

	listenAddr := resolveListenAddr(cfg.ListenAddr, os.Getenv("KOSHI_LISTEN_ADDR"))

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

// resolveListenAddr picks the listen address with precedence:
// env override > config value > default :8080.
func resolveListenAddr(cfgAddr, envOverride string) string {
	if envOverride != "" {
		return envOverride
	}
	if cfgAddr != "" {
		return cfgAddr
	}
	return ":8080"
}

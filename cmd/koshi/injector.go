package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/koshihq/koshi-runtime/internal/inject"
	corev1 "k8s.io/api/core/v1"
)

func runInjector(logger *slog.Logger) {
	certPath := os.Getenv("KOSHI_TLS_CERT")
	keyPath := os.Getenv("KOSHI_TLS_KEY")
	if certPath == "" {
		certPath = "/etc/koshi-tls/tls.crt"
	}
	if keyPath == "" {
		keyPath = "/etc/koshi-tls/tls.key"
	}

	sidecarImage := os.Getenv("KOSHI_INJECT_IMAGE")
	if sidecarImage == "" {
		sidecarImage = "koshi:latest"
	}

	sidecarPort := 15080
	if portStr := os.Getenv("KOSHI_SIDECAR_PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			sidecarPort = p
		}
	}

	sidecarPullPolicy := corev1.PullIfNotPresent
	if pp := os.Getenv("KOSHI_SIDECAR_PULL_POLICY"); pp != "" {
		sidecarPullPolicy = corev1.PullPolicy(pp)
	}

	var sidecarResources corev1.ResourceRequirements
	if resJSON := os.Getenv("KOSHI_SIDECAR_RESOURCES"); resJSON != "" {
		if err := json.Unmarshal([]byte(resJSON), &sidecarResources); err != nil {
			logger.Warn("failed to parse KOSHI_SIDECAR_RESOURCES, using no limits", "error", err)
		}
	}

	cfg := inject.WebhookConfig{
		SidecarImage:      sidecarImage,
		SidecarPullPolicy: sidecarPullPolicy,
		SidecarPort:       sidecarPort,
		SidecarResources:  sidecarResources,
		ConfigPath:       "/etc/koshi",
		ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   "15080",
			"prometheus.io/path":   "/metrics",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", inject.ServeWebhook(cfg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})

	tlsCfg, err := inject.LoadTLSConfig(certPath, keyPath)
	if err != nil {
		logger.Error("failed to load TLS config", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              ":8443",
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("koshi injector starting", "addr", ":8443")
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-stop:
		logger.Info("injector shutting down")
	case err := <-serverErr:
		logger.Error("injector server error", "error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	logger.Info("injector shutdown complete")
}

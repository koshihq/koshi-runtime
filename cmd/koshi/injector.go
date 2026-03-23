package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/koshihq/koshi-runtime/internal/inject"
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

	cfg := inject.WebhookConfig{
		SidecarImage: sidecarImage,
		SidecarPort:  sidecarPort,
		ConfigPath:   "/etc/koshi",
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

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/koshihq/koshi-runtime/internal/certgen"
)

func runCertgenSecret(logger *slog.Logger) {
	ctx := context.Background()
	if err := certgen.RunSecretPhase(ctx, logger); err != nil {
		logger.Error("certgen secret phase failed", "error", err)
		os.Exit(1)
	}
	logger.Info("certgen secret phase complete")
}

func runCertgenCABundle(logger *slog.Logger) {
	ctx := context.Background()
	if err := certgen.RunCABundlePhase(ctx, logger); err != nil {
		logger.Error("certgen cabundle phase failed", "error", err)
		os.Exit(1)
	}
	logger.Info("certgen cabundle phase complete")
}

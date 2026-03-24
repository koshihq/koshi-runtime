package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/koshihq/koshi-runtime/internal/certgen"
)

func runCertgen(logger *slog.Logger) {
	ctx := context.Background()
	if err := certgen.Run(ctx, logger); err != nil {
		logger.Error("certgen failed", "error", err)
		os.Exit(1)
	}
	logger.Info("certgen complete")
}

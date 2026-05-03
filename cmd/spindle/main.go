package main

import (
	"context"
	"log/slog"
	"os"

	tlog "tangled.org/core/log"
	"tangled.org/core/spindle"
	"tangled.org/core/spindle/config"
	dockerengine "tangled.org/core/spindle/engines/docker"
	"tangled.org/core/spindle/engines/nixery"
	"tangled.org/core/spindle/models"
)

func main() {
	logger := tlog.New("spindle")
	slog.SetDefault(logger)

	ctx := context.Background()
	ctx = tlog.IntoContext(ctx, logger)

	cfg, err := config.Load(ctx)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(-1)
	}

	dockerEng, err := dockerengine.New(ctx, cfg)
	if err != nil {
		logger.Error("failed to create docker engine", "error", err)
		os.Exit(-1)
	}

	nixeryEng, err := nixery.New(ctx, cfg)
	if err != nil {
		logger.Error("failed to create nixery engine", "error", err)
		os.Exit(-1)
	}

	s, err := spindle.New(ctx, cfg, map[string]models.Engine{
		"docker": dockerEng,
		"nixery": nixeryEng,
	})
	if err != nil {
		logger.Error("failed to create spindle", "error", err)
		os.Exit(-1)
	}

	if err := s.Start(ctx); err != nil {
		logger.Error("error running spindle", "error", err)
		os.Exit(-1)
	}
}

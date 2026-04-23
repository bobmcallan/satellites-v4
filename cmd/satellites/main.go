// Command satellites is the satellites server binary. It serves /healthz
// (and future endpoints added by later epics) and shuts down gracefully on
// SIGINT/SIGTERM within a 10-second drain bound.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/httpserver"
	"github.com/bobmcallan/satellites/internal/mcpserver"
)

func main() {
	startedAt := time.Now()

	cfg, err := config.Load()
	if err != nil {
		satarbor.Default().Error().Str("error", err.Error()).Msg("config load failed")
		os.Exit(1)
	}

	logger := satarbor.New(cfg.LogLevel)
	logger.Info().
		Str("binary", "satellites-server").
		Str("version", config.Version).
		Str("build", config.Build).
		Str("commit", config.GitCommit).
		Str("env", cfg.Env).
		Str("fly_machine_id", cfg.FlyMachineID).
		Msgf("satellites-server %s", config.GetFullVersion())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	users := auth.NewMemoryUserStore()
	sessions := auth.NewMemorySessionStore()
	providers := auth.BuildProviderSet(cfg)
	states := auth.NewStateStore(10 * time.Minute)
	authHandlers := &auth.Handlers{
		Users:     users,
		Sessions:  sessions,
		Logger:    logger,
		Cfg:       cfg,
		Providers: providers,
		States:    states,
	}

	srv := httpserver.New(cfg, logger, startedAt, authHandlers)

	mcp := mcpserver.New(cfg, logger, startedAt)
	mcpAuth := mcpserver.AuthMiddleware(mcpserver.AuthDeps{
		Sessions: sessions,
		Users:    users,
		APIKeys:  cfg.APIKeys,
		Logger:   logger,
	})
	srv.Mount("/mcp", mcpAuth(mcp))

	if err := srv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error().Str("error", err.Error()).Msg("server terminated with error")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped cleanly")
}

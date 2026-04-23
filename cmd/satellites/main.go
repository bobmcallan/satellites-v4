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
	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/httpserver"
	"github.com/bobmcallan/satellites/internal/mcpserver"
	"github.com/bobmcallan/satellites/internal/portal"
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

	portalHandlers, err := portal.New(cfg, logger, sessions, users, startedAt)
	if err != nil {
		logger.Error().Str("error", err.Error()).Msg("portal init failed")
		os.Exit(1)
	}

	srv := httpserver.New(cfg, logger, startedAt, authHandlers, portalHandlers)

	// Optional SurrealDB connection + document surface. When DB_DSN is
	// empty we keep booting (tests, dev without Surreal) but the MCP doc
	// tools are disabled and /healthz omits db_ok.
	var docStore document.Store
	if cfg.DBDSN != "" {
		dbCfg, err := db.ParseDSN(cfg.DBDSN)
		if err != nil {
			logger.Error().Str("error", err.Error()).Msg("db dsn parse failed")
			os.Exit(1)
		}
		conn, err := db.Connect(ctx, dbCfg)
		if err != nil {
			logger.Error().Str("error", err.Error()).Msg("db connect failed")
			os.Exit(1)
		}
		docStore = document.NewSurrealStore(conn)
		srv.SetHealthCheck(func(hcCtx context.Context) error { return db.Ping(hcCtx, conn) })
		if _, err := document.SeedIfEmpty(ctx, docStore, logger, cfg.DocsDir); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("document seed failed")
		}
	}

	mcp := mcpserver.New(cfg, logger, startedAt, mcpserver.Deps{
		DocStore: docStore,
		DocsDir:  cfg.DocsDir,
	})
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

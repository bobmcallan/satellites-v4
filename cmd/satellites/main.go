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
	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/httpserver"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/mcpserver"
	"github.com/bobmcallan/satellites/internal/portal"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
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
	// After the workspace store is wired (below), main() may set
	// authHandlers.OnUserCreated to seed each new user's default workspace.

	// Optional SurrealDB connection + document/project surfaces. When
	// DB_DSN is empty we keep booting (tests, dev without Surreal) but the
	// MCP doc/project tools are disabled and /healthz omits db_ok.
	var (
		docStore         document.Store
		projStore        project.Store
		ledgerStore      ledger.Store
		storyStore       story.Store
		wsStore          workspace.Store
		contractStore    contract.Store
		defaultProjectID string
		dbPing           httpserver.HealthCheck
	)
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
		surrealDocs := document.NewSurrealStore(conn)
		docStore = surrealDocs
		projStore = project.NewSurrealStore(conn)
		ledgerStore = ledger.NewSurrealStore(conn)
		storyStore = story.NewSurrealStore(conn, ledgerStore)
		wsStore = workspace.NewSurrealStore(conn)
		contractStore = contract.NewSurrealStore(conn, docStore, storyStore)
		dbPing = func(hcCtx context.Context) error { return db.Ping(hcCtx, conn) }

		// Seed the system user's default workspace so bootstrap writes
		// (default project, seeded documents) land in a workspace from day
		// one. Idempotent — safe across reboots.
		systemWsID, err := workspace.EnsureDefault(ctx, wsStore, logger, project.DefaultOwnerUserID, time.Now().UTC())
		if err != nil {
			logger.Warn().Str("error", err.Error()).Msg("system workspace seed failed")
		}
		// Grant the synthetic "apikey" user admin access to the system
		// workspace so Bearer-API-key callers share the system scope. The
		// alternative — minting a per-API-key workspace — would require
		// per-key accounting that feature-order:4 can add later.
		if systemWsID != "" {
			if err := wsStore.AddMember(ctx, systemWsID, "apikey", workspace.RoleAdmin, "system", time.Now().UTC()); err != nil {
				logger.Warn().Str("error", err.Error()).Msg("apikey system membership seed failed")
			}
		}

		// Seed default project, then idempotently stamp any legacy
		// document rows that pre-date the project primitive.
		id, err := project.SeedDefault(ctx, projStore, logger, systemWsID)
		if err != nil {
			logger.Error().Str("error", err.Error()).Msg("default project seed failed")
			os.Exit(1)
		}
		defaultProjectID = id
		if n, err := surrealDocs.BackfillProjectID(ctx, defaultProjectID); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("document backfill failed")
		} else if n > 0 {
			logger.Info().Int("rows", n).Str("project_id", defaultProjectID).Msg("document project_id backfilled")
		}

		if n, err := surrealDocs.MigrateLegacyRows(ctx, time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("document migrate legacy rows failed")
		} else if n > 0 {
			logger.Info().Int("rows", n).Msg("document legacy rows migrated to v4 schema")
		}

		if _, err := document.SeedIfEmpty(ctx, docStore, logger, systemWsID, defaultProjectID, cfg.DocsDir); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("document seed failed")
		}

		if surrealLedger, ok := ledgerStore.(*ledger.SurrealStore); ok {
			if n, err := surrealLedger.MigrateLegacyRows(ctx, time.Now().UTC()); err != nil {
				logger.Warn().Str("error", err.Error()).Msg("ledger migrate legacy rows failed")
			} else if n > 0 {
				logger.Info().Int("rows", n).Msg("ledger legacy rows migrated to v4 schema")
			}
		}

		// Backfill workspace_id across primitives. Idempotent on every
		// boot — second invocation finds no rows with empty workspace_id.
		if _, err := workspace.BackfillPrimitives(ctx, wsStore, projStore, storyStore, ledgerStore, docStore, logger, time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("workspace backfill failed")
		}

		// Wire user-creation → EnsureDefault once the workspace store is up.
		// New DevMode / OAuth users will get a personal workspace on first
		// login. Idempotent per user.
		authHandlers.OnUserCreated = func(hookCtx context.Context, userID string) {
			if _, err := workspace.EnsureDefault(hookCtx, wsStore, logger, userID, time.Now().UTC()); err != nil {
				logger.Warn().Str("user_id", userID).Str("error", err.Error()).Msg("default workspace seed for user failed")
			}
		}
	}

	portalHandlers, err := portal.New(cfg, logger, sessions, users, projStore, ledgerStore, storyStore, wsStore, startedAt)
	if err != nil {
		logger.Error().Str("error", err.Error()).Msg("portal init failed")
		os.Exit(1)
	}

	srv := httpserver.New(cfg, logger, startedAt, authHandlers, portalHandlers)
	if dbPing != nil {
		srv.SetHealthCheck(dbPing)
	}

	mcp := mcpserver.New(cfg, logger, startedAt, mcpserver.Deps{
		DocStore:         docStore,
		DocsDir:          cfg.DocsDir,
		ProjectStore:     projStore,
		DefaultProjectID: defaultProjectID,
		LedgerStore:      ledgerStore,
		StoryStore:       storyStore,
		WorkspaceStore:   wsStore,
		ContractStore:    contractStore,
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

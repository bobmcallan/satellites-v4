// Command satellites is the satellites server binary. It serves /healthz
// (and future endpoints added by later epics) and shuts down gracefully on
// SIGINT/SIGTERM within a 10-second drain bound.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/dispatcher"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/httpserver"
	"github.com/bobmcallan/satellites/internal/hub"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/mcpserver"
	"github.com/bobmcallan/satellites/internal/portal"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
	"github.com/bobmcallan/satellites/internal/wshandler"
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
		sessionStore     session.Store
		grantStore       rolegrant.Store
		taskStore        task.Store
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
		sessionStore = session.NewSurrealStore(conn)
		grantStore = rolegrant.NewSurrealStore(conn, docStore)
		taskStore = task.NewSurrealStore(conn)
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

		// Seed the system-scope orchestrator role + agent documents that
		// the SessionStart path uses to mint orchestrator grants. System
		// scope means workspace_id=systemWsID, project_id=nil. Idempotent:
		// skip when a document with the canonical name already exists.
		// Story_7d9c4b1b.
		if err := seedOrchestratorDocs(ctx, docStore, systemWsID, time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("orchestrator docs seed failed")
		}

		// Backfill required_role on pre-existing contract documents.
		// Contracts without a required_role field in their structured
		// payload receive role_orchestrator so the process-order gate's
		// new grant check (story_85675c33) can hit a stable target.
		// Idempotent: contracts that already carry required_role are
		// untouched.
		if n, err := stampRequiredRoleOnContracts(ctx, docStore, time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("contract required_role backfill failed")
		} else if n > 0 {
			logger.Info().Int("rows", n).Msg("contract required_role backfill stamped")
		}

		// Migrate pre-6.5 contract_instance rows off the legacy
		// claimed_by_session_id column (story_4608a82c): first stamp
		// claimed_via_grant_id by resolving each row's legacy session
		// through the session registry, then UNSET the column. The two
		// operations are idempotent — a clean DB short-circuits both.
		if surrealContracts, ok := contractStore.(*contract.SurrealStore); ok {
			sessionMap, err := buildSessionGrantLookup(ctx, sessionStore)
			if err != nil {
				logger.Warn().Str("error", err.Error()).Msg("contract grant backfill: session lookup failed")
			} else {
				stamped, missed, err := surrealContracts.BackfillClaimedViaGrant(ctx, sessionMap, time.Now().UTC())
				if err != nil {
					logger.Warn().Str("error", err.Error()).Msg("contract grant backfill failed")
				} else if stamped > 0 || missed > 0 {
					logger.Info().Int("stamped", stamped).Int("missed", missed).Msg("contract claimed_via_grant_id backfill complete")
				}
			}
			if err := surrealContracts.DropLegacySessionColumn(ctx); err != nil {
				logger.Warn().Str("error", err.Error()).Msg("contract drop legacy session column failed")
			}
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

	// Websocket hub (slice 10.1) + workspace-aware AuthHub (slice 10.2).
	// The hub is shared across the process; emit hooks (slice 10.3) will
	// attach publishers to the same instance at the store layer.
	sharedHub := hub.New()
	var authHub *hub.AuthHub
	var wsHandlers *wshandler.Handler
	if wsStore != nil && ledgerStore != nil {
		audit := &ledgerMismatchAudit{
			ledger:    ledgerStore,
			projectID: defaultProjectID,
			logger:    logger,
		}
		authHub = hub.NewAuthHub(sharedHub, wsStore, audit)
		wsHandlers = wshandler.New(wshandler.Deps{
			AuthHub:  authHub,
			Sessions: &sessionResolverAdapter{sessions: sessions, users: users},
			Logger:   logger,
		})
	}

	registrars := []httpserver.RouteRegistrar{authHandlers, portalHandlers}
	if wsHandlers != nil {
		registrars = append(registrars, wsHandlers)
	}
	srv := httpserver.New(cfg, logger, startedAt, registrars...)
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
		SessionStore:     sessionStore,
		RoleGrantStore:   grantStore,
		TaskStore:        taskStore,
	})
	mcpAuth := mcpserver.AuthMiddleware(mcpserver.AuthDeps{
		Sessions: sessions,
		Users:    users,
		APIKeys:  cfg.APIKeys,
		Logger:   logger,
	})
	srv.Mount("/mcp", mcpAuth(mcp))

	// Dispatch watchdog: scans for expired claims and reclaims them
	// into the queue. Story_b4513c8c. Runs only when the task store is
	// wired (DB_DSN present).
	if taskStore != nil {
		disp := dispatcher.New(taskStore, ledgerStore, logger, dispatcher.Options{})
		if err := disp.Start(ctx); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("dispatcher watchdog start failed")
		} else {
			defer disp.Stop()
		}
	}

	if err := srv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error().Str("error", err.Error()).Msg("server terminated with error")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped cleanly")
}

// seedOrchestratorDocs creates the system-scope `role_orchestrator` and
// `agent_claude_orchestrator` documents that the SessionStart path uses
// to mint orchestrator grants. Idempotent: existing rows are left
// untouched. Called from main during boot; a nil docStore short-circuits
// so early-boot tests that don't configure Surreal stay green.
// Story_7d9c4b1b.
func seedOrchestratorDocs(ctx context.Context, docStore document.Store, workspaceID string, now time.Time) error {
	if docStore == nil {
		return nil
	}
	// Create the role first so we can reference its id from the agent's
	// permitted_roles payload.
	role, err := docStore.GetByName(ctx, "", "role_orchestrator", nil)
	if err != nil {
		role, err = docStore.Create(ctx, document.Document{
			WorkspaceID: workspaceID,
			ProjectID:   nil,
			Type:        document.TypeRole,
			Name:        "role_orchestrator",
			Scope:       document.ScopeSystem,
			Status:      document.StatusActive,
			Body:        "Orchestrator role — the interactive Claude session's authorisation bundle. Holds every orchestrator-surface MCP verb (document_*, story_*, ledger_*, project_*, repo_*, contract_*, task_*, session_whoami, agent_role_*). Required hooks: SessionStart, PreToolUse, enforce. Seeded by platform bootstrap per pr_contract_separation.",
			Structured:  []byte(`{"allowed_mcp_verbs":["document_*","story_*","ledger_*","project_*","repo_*","workspace_*","principle_*","contract_*","skill_*","reviewer_*","agent_*","role_*","session_whoami","satellites_info"],"required_hooks":["SessionStart","PreToolUse","enforce"],"claim_requirements":[],"default_context_policy":"fresh-per-claim"}`),
			Tags:        []string{"v4", "agents-roles", "seed"},
			CreatedBy:   "system",
		}, now)
		if err != nil {
			return fmt.Errorf("seed role_orchestrator: %w", err)
		}
	}
	if _, err := docStore.GetByName(ctx, "", "agent_claude_orchestrator", nil); err == nil {
		return nil
	}
	structured := `{"provider_chain":[{"provider":"claude","model":"opus-4","tier":"opus"}],"tier":"opus","permitted_roles":["` + role.ID + `"],"tool_ceiling":["*"]}`
	if _, err := docStore.Create(ctx, document.Document{
		WorkspaceID: workspaceID,
		ProjectID:   nil,
		Type:        document.TypeAgent,
		Name:        "agent_claude_orchestrator",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Body:        "Claude orchestrator agent — the interactive session's delivery-agent configuration. provider_chain=claude/opus, tier=opus, tool_ceiling=['*']. permitted_roles pins role_orchestrator so the SessionStart grant path resolves. Seeded by platform bootstrap.",
		Structured:  []byte(structured),
		Tags:        []string{"v4", "agents-roles", "seed"},
		CreatedBy:   "system",
	}, now); err != nil {
		return fmt.Errorf("seed agent_claude_orchestrator: %w", err)
	}
	return nil
}

// stampRequiredRoleOnContracts scans every active type=contract row and,
// when the row's structured payload lacks a required_role field, writes
// an updated payload with required_role=role_orchestrator. Returns the
// number of rows stamped. Idempotent — a second call finds no rows
// without required_role. Story_85675c33.
func stampRequiredRoleOnContracts(ctx context.Context, docStore document.Store, now time.Time) (int, error) {
	if docStore == nil {
		return 0, nil
	}
	rows, err := docStore.List(ctx, document.ListOptions{Type: document.TypeContract}, nil)
	if err != nil {
		return 0, fmt.Errorf("list contracts: %w", err)
	}
	stamped := 0
	for _, row := range rows {
		updated, ok := addRequiredRoleIfMissing(row.Structured, "role_orchestrator")
		if !ok {
			continue
		}
		structuredVal := updated
		if _, err := docStore.Update(ctx, row.ID, document.UpdateFields{Structured: &structuredVal}, "system", now, nil); err != nil {
			return stamped, fmt.Errorf("update contract %s: %w", row.ID, err)
		}
		stamped++
	}
	return stamped, nil
}

// buildSessionGrantLookup walks every registered session and returns a
// map session_id → orchestrator_grant_id for the subset carrying a
// grant. Used by the boot-time contract_instance grant backfill in
// story_4608a82c. Empty map on nil store — the caller tolerates that.
func buildSessionGrantLookup(ctx context.Context, sessions session.Store) (map[string]string, error) {
	out := make(map[string]string)
	if sessions == nil {
		return out, nil
	}
	rows, err := sessions.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.OrchestratorGrantID == "" {
			continue
		}
		out[row.SessionID] = row.OrchestratorGrantID
	}
	return out, nil
}

// addRequiredRoleIfMissing inserts `required_role=roleName` into the
// JSON payload if the key is absent. Returns (newPayload, true) when a
// mutation is needed; (nil, false) when the key already exists or the
// payload is not a JSON object. Malformed JSON (non-object, non-empty)
// is left untouched — the caller logs via the return value.
func addRequiredRoleIfMissing(raw []byte, roleName string) ([]byte, bool) {
	if len(raw) == 0 {
		// Synthesize a minimal payload so the claim gate can resolve it.
		out, _ := json.Marshal(map[string]any{"required_role": roleName})
		return out, true
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	if _, exists := obj["required_role"]; exists {
		return nil, false
	}
	obj["required_role"] = roleName
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

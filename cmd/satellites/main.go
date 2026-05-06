// Command satellites is the satellites server binary. It serves /healthz
// (and future endpoints added by later epics) and shuts down gracefully on
// SIGINT/SIGTERM within a 10-second drain bound.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ternarybob/arbor"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/dispatcher"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/httpserver"
	"github.com/bobmcallan/satellites/internal/hub"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/mcpserver"
	"github.com/bobmcallan/satellites/internal/permhook"
	"github.com/bobmcallan/satellites/internal/portal"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/ratelimit"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
	"github.com/bobmcallan/satellites/internal/wshandler"
)

func main() {
	startedAt := time.Now()

	cfg, cfgWarnings := config.Load()

	logger := satarbor.New(cfg.LogLevel)
	logger.Info().
		Str("binary", "satellites-server").
		Str("version", config.Version).
		Str("build", config.Build).
		Str("commit", config.GitCommit).
		Str("env", cfg.Env).
		Str("fly_machine_id", os.Getenv("FLY_MACHINE_ID")).
		Msgf("satellites-server %s", config.GetFullVersion())

	if path := cfg.LoadedTOMLPath(); path != "" {
		logger.Info().Str("path", path).Msg("config: loaded TOML")
	}

	for _, w := range cfgWarnings {
		logger.Warn().Str("warning", w).Msg("config: startup warning")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var users auth.UserStore = auth.NewMemoryUserStore()
	var sessions auth.SessionStore = auth.NewMemorySessionStore()
	providers := auth.BuildProviderSet(cfg)
	states := auth.NewStateStore(10 * time.Minute)
	for _, p := range providers.Enabled() {
		// Story_40e3bd27: dual-route registration is the cutover compat
		// layer per pr_no_unrequested_compat. Surface both forms in the
		// boot log so operators can verify the OAuth client config has
		// both Authorized redirect URIs registered.
		logger.Info().
			Str("provider", p.Name).
			Str("legacy_start", "/auth/"+p.Name+"/start").
			Str("legacy_callback", "/auth/"+p.Name+"/callback").
			Str("v3_aligned_login", "/api/auth/login/"+p.Name).
			Str("v3_aligned_callback", "/api/auth/callback/"+p.Name).
			Str("redirect_url", p.OAuth2.RedirectURL).
			Msg("oauth: routes registered (legacy + v3-aligned for cutover)")
	}
	authHandlers := &auth.Handlers{
		Users:        users,
		Sessions:     sessions,
		Logger:       logger,
		Cfg:          cfg,
		Providers:    providers,
		States:       states,
		LoginLimiter: ratelimit.New(10, time.Minute),
	}
	// After the workspace store is wired (below), main() may set
	// authHandlers.OnUserCreated to seed each new user's default workspace.

	// Optional SurrealDB connection + document/project surfaces. When
	// SATELLITES_DB_DSN is empty we keep booting (tests, dev without Surreal) but the
	// MCP doc/project tools are disabled and /healthz omits db_ok.
	var (
		docStore         document.Store
		projStore        project.Store
		ledgerStore      ledger.Store
		storyStore       story.Store
		wsStore          workspace.Store
		sessionStore     session.Store
		taskStore        task.Store
		repoStore        repo.Store
		changelogStore   changelog.Store
		repoIndexer      codeindex.Indexer
		defaultProjectID string
		dbPing           httpserver.HealthCheck

		oauthServer *auth.OAuthServer
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
		// MCP OAuth 2.1 Authorization Server — needs Surreal for client/
		// session/code/refresh-token persistence. Wired here inside the
		// db-available block so the !DBDSN path skips it entirely (the
		// fallback warning lives in the bearerValidator block below).
		oauthStore := auth.NewSurrealOAuthStore(conn, logger)
		// resolveSessionUser closes over the SessionStore + UserStore so
		// the OAuth-AS shortcircuit at /oauth/authorize can detect an
		// already-logged-in browser session and skip the mcp_session_id
		// bridge entirely. Returns "" when no cookie / invalid session,
		// causing the AS to fall through to the bridge dance.
		resolveSessionUser := func(r *http.Request) string {
			id := auth.ReadCookie(r)
			if id == "" {
				return ""
			}
			sess, err := sessions.Get(id)
			if err != nil {
				return ""
			}
			user, err := users.GetByID(sess.UserID)
			if err != nil {
				return ""
			}
			return user.ID
		}
		oauthServer = auth.NewOAuthServer(auth.OAuthServerConfig{
			JWTSecret:          cfg.JWTSecret,
			Issuer:             cfg.OAuthIssuer,
			AccessTokenTTL:     cfg.OAuthAccessTokenTTL,
			RefreshTokenTTL:    cfg.OAuthRefreshTokenTTL,
			CodeTTL:            cfg.OAuthCodeTTL,
			Store:              oauthStore,
			Logger:             logger,
			DevMode:            cfg.DevMode,
			ResolveSessionUser: resolveSessionUser,
		})
		authHandlers.OAuthServer = oauthServer

		surrealDocs := document.NewSurrealStore(conn)
		docStore = surrealDocs
		projStore = project.NewSurrealStore(conn)
		ledgerStore = ledger.NewSurrealStore(conn)
		storyStore = story.NewSurrealStore(conn, ledgerStore)
		wsStore = workspace.NewSurrealStore(conn)
		sessionStore = session.NewSurrealStore(conn)
		// story_0ab83f82: replace MemorySessionStore with the durable
		// Surreal-backed implementation when SATELLITES_DB_DSN is set so cookie
		// sessions survive Fly rolling restarts.
		sessions = auth.NewSurrealSessionStore(conn)
		authHandlers.Sessions = sessions
		// story_7512783a: replace MemoryUserStore with the durable
		// Surreal-backed implementation. OAuth-minted users now persist
		// across restarts, so cookies don't orphan after a deploy.
		users = auth.NewSurrealUserStore(conn)
		authHandlers.Users = users
		taskStore = task.NewSurrealStore(conn)
		repoStore = repo.NewSurrealStore(conn)
		changelogStore = changelog.NewSurrealStore(conn)
		repoIndexer = codeindex.NewLocalIndexer(filepath.Join(os.TempDir(), "satellites-repos"))
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

		// epic:setup-as-data-v1 (sty_db196ff4 / sty_6c3f8091): role +
		// agent + artifact seeds previously written by in-Go helpers
		// (seedOrchestratorDocs / seedReviewerDocs / seedLifecycleAgents
		// / agentprocess.SeedSystemDefault) are now seeded by configseed
		// below from config/seed/{roles,agents,artifacts}/*.md.
		// configseed is the single writer for every doc the substrate
		// boots with — operators tighten the substrate by editing the
		// seed files, not Go code.

		// story_7bfd629c: load system-tier configuration from
		// ./config/seed/{roles,agents,contracts,workflows,artifacts,...}/*.md
		// and ./config/help/*.md. Markdown is the single source of truth.
		// Failures log at warn — the platform stays bootable on a
		// malformed file.
		if summary, err := configseed.RunAll(ctx, docStore,
			configseed.ResolveSeedDir(), configseed.ResolveHelpDir(),
			systemWsID, "system", time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("configseed run failed")
		} else {
			logger.Info().
				Int("loaded", summary.Loaded).
				Int("created", summary.Created).
				Int("updated", summary.Updated).
				Int("skipped", summary.Skipped).
				Int("errors", len(summary.Errors)).
				Msg("configseed run complete")
			for _, e := range summary.Errors {
				logger.Warn().Str("path", e.Path).Str("reason", e.Reason).Msg("configseed entry failed")
			}
		}

		// sty_8f6b90c8: archive scope=system type=principle rows whose
		// Name no longer matches a seed file under system/principles/.
		// Catches orphans left after a principle is moved to project
		// tier or deleted from the seed dir. Idempotent.
		if archived, err := configseed.SweepOrphanedSystemPrinciples(ctx, docStore, configseed.ResolveSeedDir(), logger, time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("system principle sweep failed")
		} else if archived > 0 {
			logger.Info().Int("archived", archived).Msg("orphan system principles archived")
		}

		// sty_c1200f75: migrate any pre-existing tasks at status=enqueued
		// to status=published. The substrate now distinguishes planned
		// (agent-local) from published (queue-visible); existing rows
		// were all queue-visible by definition. Idempotent.
		if taskStore != nil {
			if n, err := task.MigrateEnqueuedToPublished(ctx, taskStore, time.Now().UTC()); err != nil {
				logger.Warn().Str("error", err.Error()).Msg("task migrate enqueued->published failed")
			} else if n > 0 {
				logger.Info().Int("rows", n).Msg("task enqueued->published migration complete")
			}
		}

		// story_b1108d4a: migrate legacy skill→contract bindings into
		// agent.skill_refs so the new contract_next resolution path
		// surfaces skills via the allocated agent. Idempotent — skills
		// without a binding or already migrated are no-ops. Runs AFTER
		// configseed loads the lifecycle agents so the matching agent
		// docs exist.
		document.MigrateSkillContractBindings(ctx, docStore, logger, time.Now().UTC())

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

		// Wire user-creation → workspace.EnsureDefault. Each new user gets
		// a personal workspace on first login. Project creation is now an
		// explicit action — sty_c975ebeb removed the auto-seeded "Default"
		// project because it conflated multi-repo scopes with single-repo
		// stories. Users land on /projects with an empty-state panel until
		// they create a project (project_create + repo_add for code-backed
		// projects per sty_14dfd05b, or via the portal).
		authHandlers.OnUserCreated = func(hookCtx context.Context, userID string) {
			now := time.Now().UTC()
			if _, err := workspace.EnsureDefault(hookCtx, wsStore, logger, userID, now); err != nil {
				logger.Warn().Str("user_id", userID).Str("error", err.Error()).Msg("default workspace seed for user failed")
			}
		}

		// One-shot migration: archive legacy per-user "Default" projects
		// that the old EnsureDefault hook minted on first login. Idempotent
		// — a second invocation finds none active. sty_c975ebeb.
		if _, err := project.ArchiveLegacyDefaults(ctx, projStore, logger, time.Now().UTC()); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("archive legacy defaults failed")
		}

		// sty_14dfd05b: re-canonicalise existing repos.git_remote values,
		// backfill repo rows from any project still carrying the legacy
		// projects.git_remote column, then UNSET the column. Idempotent
		// — second invocation recanonicalises 0 / backfills 0.
		if conn != nil && repoStore != nil {
			if recanon, backfilled, mErr := repo.MigrateRemotesCanonical(ctx, conn, repoStore, logger, time.Now().UTC()); mErr != nil {
				logger.Warn().Str("error", mErr.Error()).Msg("repo remotes canonical migration failed")
			} else if recanon > 0 || backfilled > 0 {
				logger.Info().Int("recanonicalised", recanon).Int("backfilled", backfilled).Msg("repo remotes canonical migration complete")
			}
		}

		// sty_c6d76a5b retired the lifecycle.validation_mode KV row —
		// every close now goes through the review-task gate
		// unconditionally; mode resolution is gone.

		// sty_51571015 retired the in-process reviewer service. Reviews
		// are now dispatched via `agent_dispatch` (the orchestrator
		// claims kind:review tasks and spawns the appropriate reviewer
		// agent in an isolated worktree). The substrate keeps emitting
		// kind:review task events; only the listener that auto-claimed
		// + auto-verdicted is gone.
	}

	portalHandlers, err := portal.New(cfg, logger, sessions, users, projStore, ledgerStore, storyStore, taskStore, docStore, repoStore, repoIndexer, wsStore, startedAt)
	if err != nil {
		logger.Error().Str("error", err.Error()).Msg("portal init failed")
		os.Exit(1)
	}
	if oauthServer != nil {
		// Defense-in-depth: when the AS shortcircuit at /oauth/authorize
		// doesn't fire (e.g. user not yet logged in) the browser bounces
		// through /?mcp_session=… and the portal landing must complete
		// the flow if the user is already authenticated.
		portalHandlers.SetOAuthServer(oauthServer)
	}
	portalHandlers.SetChangelogStore(changelogStore)

	// Websocket hub (slice 10.1) + workspace-aware AuthHub (slice 10.2) +
	// store-layer emit hooks (slice 10.3). One hub instance per process.
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

		// Attach the store-layer publisher so ledger / task / story
		// mutations fan to the hub on every write.
		publisher := &hubPublisher{authHub: authHub}
		attachPublisher(ledgerStore, publisher)
		attachPublisher(taskStore, publisher)
		attachPublisher(storyStore, publisher)

		// sty_0233fabd: wire the open-tasks gate. UpdateStatus rejects
		// transitions to done|cancelled while the chain has any task at
		// status=published or status=planned. UpdateStatusDerived bypasses.
		if g, ok := storyStore.(interface {
			SetOpenTasksFunc(story.OpenTasksFunc)
		}); ok && taskStore != nil {
			ts := taskStore
			g.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
				rows, err := ts.List(ctx, task.ListOptions{StoryID: storyID, Limit: 500}, memberships)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0)
				for _, r := range rows {
					if r.Status == task.StatusPublished || r.Status == task.StatusPlanned {
						ids = append(ids, r.ID)
					}
				}
				return ids, nil
			})
		}
	}

	// sty_dc2998c5: nightly sweep that flips closed tasks older than
	// SATELLITES_TASKS_RETENTION_DAYS (default 90) into archived. The
	// row stays put + ledger anchors are untouched; archived rows just
	// fall out of the default task_list query. Boot-time first pass
	// runs immediately so a freshly-deployed build doesn't wait 24h
	// to catch up on history.
	if taskStore != nil {
		go runTaskRetentionSweep(ctx, taskStore, cfg, logger)
	}

	bearerValidator := auth.NewBearerValidator(auth.BearerValidatorConfig{
		CacheTTL:  cfg.OAuthTokenCacheTTL,
		JWTSecret: []byte(cfg.JWTSecret),
	})

	if oauthServer == nil {
		logger.Warn().Msg("oauth: DB unavailable — MCP OAuth 2.1 endpoints disabled (clients must use SATELLITES_API_KEYS bearer or session cookie)")
	}
	tokenExchange := &auth.TokenExchange{
		Sessions:  sessions,
		Users:     users,
		Validator: bearerValidator,
	}
	registrars := []httpserver.RouteRegistrar{authHandlers, portalHandlers, tokenExchange}
	if wsHandlers != nil {
		registrars = append(registrars, wsHandlers)
	}
	if oauthServer != nil {
		registrars = append(registrars, &oauthRoutes{srv: oauthServer})
	}
	// story_c08856b2: /hooks/enforce HTTP handler resolves a tool
	// call's allow/deny via the active CI's allocated agent (or the
	// session-default-install row when no CI is claimed).
	if sessionStore != nil && ledgerStore != nil && docStore != nil {
		permHandler := &permhook.Handler{
			Resolver: &permhook.Resolver{
				Sessions: sessionStore,
				Ledger:   ledgerStore,
				Docs:     docStore,
			},
			Logger: logger,
		}
		registrars = append(registrars, permHandler)
	}
	srv := httpserver.New(cfg, logger, startedAt, registrars...)
	if dbPing != nil {
		srv.SetHealthCheck(dbPing)
	}
	srv.SetLLMPinger(newGeminiPinger(cfg.GeminiAPIKey))

	mcp := mcpserver.New(cfg, logger, startedAt, mcpserver.Deps{
		DocStore:         docStore,
		DocsDir:          cfg.DocsDir,
		ProjectStore:     projStore,
		DefaultProjectID: defaultProjectID,
		LedgerStore:      ledgerStore,
		StoryStore:       storyStore,
		WorkspaceStore:   wsStore,
		SessionStore:     sessionStore,
		TaskStore:        taskStore,
		RepoStore:        repoStore,
		ChangelogStore:   changelogStore,
		Indexer:          repoIndexer,
		AuditReadTTL:     auditReadTTL(),
	})
	// Sty_088f6d5c: install the portal_replicate action vocabulary
	// from the seeded replicate_vocabulary document. configseed has
	// already loaded config/seed/replicate_vocabulary/default.md by
	// this point. Failures fall back to the canonical-only vocabulary
	// so the tool stays callable with built-in action names.
	if err := mcp.LoadReplicateVocabularyFromDoc(ctx, "default"); err != nil {
		logger.Warn().Str("error", err.Error()).Msg("portal_replicate vocabulary load failed (canonical-only fallback)")
	}
	// sty_cd8b89c6: snapshot the registered MCP tool catalogue into the
	// document store so the portal /mcp page renders 1:1 what Claude
	// sees through tools/list. Boot-time only; no runtime drift window.
	if err := mcp.MaterialiseCatalogue(ctx); err != nil {
		logger.Warn().Str("error", err.Error()).Msg("mcp catalogue snapshot failed (page will render empty-state)")
	}
	// sty_8868eaf4 / sty_87e203c1: project-tier seed pass. Walks
	// config/seed/wksp_*/proj_*/<kind>/*.md and upserts each as a scope=project
	// document. Runs in a goroutine — must not impede startup. Each
	// project dir whose project_id resolves to an existing project row
	// produces a kind:project-seed-run ledger row; missing projects log
	// a warning and skip (no auto-create).
	go func() {
		pairs, derr := configseed.DiscoverProjectDirs(configseed.ResolveSeedDir())
		if derr != nil {
			logger.Warn().Str("error", derr.Error()).Msg("project seed discovery failed")
			return
		}
		for _, d := range pairs {
			result, perr := mcp.RunProjectSeed(ctx, d.ProjectID, "system")
			if perr != nil {
				logger.Warn().
					Str("workspace_id", d.WorkspaceID).
					Str("project_id", d.ProjectID).
					Str("error", perr.Error()).
					Msg("project seed run failed")
				continue
			}
			logger.Info().
				Str("workspace_id", d.WorkspaceID).
				Str("project_id", d.ProjectID).
				Int("loaded", result.Loaded).
				Int("created", result.Created).
				Int("updated", result.Updated).
				Int("skipped", result.Skipped).
				Int("errors", len(result.Errors)).
				Msg("project seed run complete")
			for _, e := range result.Errors {
				logger.Warn().Str("workspace_id", d.WorkspaceID).Str("project_id", d.ProjectID).Str("path", e.Path).Str("reason", e.Reason).Msg("project seed entry failed")
			}
		}
	}()
	mcpAuth := mcpserver.AuthMiddleware(mcpserver.AuthDeps{
		Sessions:       sessions,
		Users:          users,
		APIKeys:        cfg.APIKeys,
		Logger:         logger,
		OAuthValidator: bearerValidator,
	})
	srv.Mount("/mcp", mcpAuth(mcp))

	// Dispatch watchdog: scans for expired claims and reclaims them
	// into the queue. Story_b4513c8c. Runs only when the task store is
	// wired (SATELLITES_DB_DSN present).
	if taskStore != nil {
		disp := dispatcher.New(taskStore, ledgerStore, logger, dispatcher.Options{})
		if err := disp.Start(ctx); err != nil {
			logger.Warn().Str("error", err.Error()).Msg("dispatcher watchdog start failed")
		} else {
			defer disp.Stop()
		}
	}

	// sty_51571015 retired the in-process reviewer service goroutine.
	// kind:review tasks now flow to the orchestrator session, which
	// reads its seed-prescribed dispatch mechanism (per the
	// default_agent_process artifact body) and runs the appropriate
	// reviewer agent via `claude -p` from its own Bash tool — same
	// path as every other dispatch.

	// sty_509a46fa: embedding ingestion worker + repo reindex worker
	// removed. Both depended on Task.Payload as a job envelope, which is
	// itself gone. Semantic search and repo reindex are no longer
	// background-job features; reintroduce as substrate-native verbs if
	// needed.

	if err := srv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error().Str("error", err.Error()).Msg("server terminated with error")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped cleanly")
}

// geminiPinger is the httpserver.LLMPinger adapter for the Google
// Generative Language API. Configured() reflects whether the API key is
// set; Ping() does a tiny GET against the /v1beta/models?key=...
// endpoint with a 5 s timeout. Anything other than 2xx is reported as
// unreachable so /healthz can colour-code the badge. The probe is
// independent of the close-time reviewer client — keeping it isolated
// here avoids dragging the reviewer's internal HTTP shape into the
// liveness path. Sty_558c0431.
type geminiPinger struct {
	apiKey string
	client *http.Client
}

func newGeminiPinger(apiKey string) *geminiPinger {
	return &geminiPinger{apiKey: apiKey, client: &http.Client{Timeout: 5 * time.Second}}
}

func (g *geminiPinger) Configured() bool { return g != nil && g.apiKey != "" }

func (g *geminiPinger) Ping(ctx context.Context) error {
	if g == nil || g.apiKey == "" {
		return errors.New("gemini: api key not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://generativelanguage.googleapis.com/v1beta/models?key="+url.QueryEscape(g.apiKey), nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gemini probe: status %d", resp.StatusCode)
	}
	return nil
}

// oauthRoutes adapts auth.OAuthServer to the httpserver.RouteRegistrar
// interface. The five route bindings are the surface the MCP-spec OAuth
// 2.1 chain (RFC 9728 / 8414 / 7591 + PKCE) requires from any compliant
// authorization server.
type oauthRoutes struct{ srv *auth.OAuthServer }

func (o *oauthRoutes) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", o.srv.HandleAuthorizationServer)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", o.srv.HandleProtectedResource)
	mux.HandleFunc("POST /oauth/register", o.srv.HandleRegister)
	mux.HandleFunc("/oauth/authorize", o.srv.HandleAuthorize)
	mux.HandleFunc("POST /oauth/token", o.srv.HandleToken)
}

// projectListAller is the optional ListAll surface a project store may
// implement. Both project.MemoryStore and project.SurrealStore satisfy
// it today; the interface is declared here so cmd/satellites stays
// decoupled from the concrete types.
type projectListAller interface {
	ListAll(ctx context.Context) ([]project.Project, error)
}

// runTaskRetentionSweep is the boot-time goroutine that runs the task
// retention sweep on a 24h cadence (sty_dc2998c5). The cutoff is
// SATELLITES_TASKS_RETENTION_DAYS days back (default 90). The sweep
// only flips closed → archived; ledger anchors persist + the row stays
// in place. Best-effort — errors log + the loop keeps ticking.
func runTaskRetentionSweep(ctx context.Context, store task.Store, cfg *config.Config, logger arbor.ILogger) {
	if store == nil {
		return
	}
	retention := taskRetentionDays()
	// Initial pass + 24h ticker.
	runOnce := func() {
		now := time.Now().UTC()
		cutoff := now.Add(-time.Duration(retention) * 24 * time.Hour)
		res, err := task.Sweep(ctx, store, cutoff, now, nil)
		if err != nil {
			logger.Warn().Str("error", err.Error()).Msg("task retention sweep failed")
			return
		}
		if res.Archived > 0 || res.Errored > 0 {
			logger.Info().
				Int("scanned", res.Scanned).
				Int("archived", res.Archived).
				Int("errored", res.Errored).
				Int("retention_days", retention).
				Msg("task retention sweep complete")
		}
	}
	runOnce()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// taskRetentionDays returns the resolved retention window in days. Reads
// SATELLITES_TASKS_RETENTION_DAYS; falls back to 90 when unset, empty,
// non-numeric, or non-positive. Surfaced as a helper so tests can flip
// the env without dragging the full Config layer through.
func taskRetentionDays() int {
	const fallback = 90
	raw := os.Getenv("SATELLITES_TASKS_RETENTION_DAYS")
	if raw == "" {
		return fallback
	}
	n, err := strconvAtoiSafe(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// strconvAtoiSafe wraps strconv.Atoi but trims whitespace first so the
// env var lookup tolerates trailing newlines from `echo -n` patterns.
func strconvAtoiSafe(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// auditReadTTL returns the resolved ephemeral-row TTL applied to read
// MCP-call audit rows. Reads SATELLITES_AUDIT_READ_TTL_HOURS; falls
// back to 720h (30 days) when unset / non-numeric / non-positive.
// Sty_1493c077.
func auditReadTTL() time.Duration {
	const fallback = 720 * time.Hour
	raw := strings.TrimSpace(os.Getenv("SATELLITES_AUDIT_READ_TTL_HOURS"))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Hour
}

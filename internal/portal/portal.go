// Package portal hosts the satellites v4 SSR handlers. It owns the login page,
// the authenticated landing, and the static-asset mount. Later epics attach
// primitive views to this surface.
package portal

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ternarybob/arbor"

	"encoding/json"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
	"github.com/bobmcallan/satellites/pages"
)

// Portal wires template rendering, the auth dependencies, and the static
// filesystem into a set of http.Handlers.
type Portal struct {
	tmpl              *template.Template
	cfg               *config.Config
	logger            arbor.ILogger
	sessions          auth.SessionStore
	users             auth.UserStoreByID
	projects          project.Store
	ledger            ledger.Store
	stories           story.Store
	tasks             task.Store
	documents         document.Store
	repos             repo.Store
	changelog         changelog.Store
	indexer           codeindex.Indexer
	workspaces        workspace.Store
	startedAt         time.Time
	globalAdminEmails map[string]struct{}
	oauthServer       *auth.OAuthServer
}

// SetOAuthServer wires the MCP-spec OAuth Authorization Server so the
// portal landing handler can complete a pending OAuth flow when the
// user arrives at /?mcp_session=… already authenticated. nil disables
// the bridge — used by tests that don't construct an OAuthServer.
func (p *Portal) SetOAuthServer(srv *auth.OAuthServer) {
	p.oauthServer = srv
}

// SetChangelogStore wires the optional changelog store (sty_12af0bdc).
// nil disables the changelog panel on the project page — the panel
// still appears but renders empty.
func (p *Portal) SetChangelogStore(s changelog.Store) {
	p.changelog = s
}

// New constructs the Portal handler set. Template parsing errors return
// immediately so main() can exit with a clear message. Nil store args
// disable the corresponding page group (the handlers render a "disabled"
// panel or 404). A nil workspaces store keeps the pre-tenant behaviour
// (membership scoping disabled) — used by tests that don't need it. A
// nil tasks store keeps the /tasks page reachable but renders empty
// columns (sty_c6d76a5b checkpoint 14 retired the contract-instance
// panel; the contract walk now projects from tasks).
func New(cfg *config.Config, logger arbor.ILogger, sessions auth.SessionStore, users auth.UserStoreByID, projects project.Store, ledgerStore ledger.Store, stories story.Store, tasks task.Store, documents document.Store, repos repo.Store, indexer codeindex.Indexer, workspaces workspace.Store, startedAt time.Time) (*Portal, error) {
	tmpl, err := pages.Templates()
	if err != nil {
		return nil, err
	}
	return &Portal{
		tmpl:              tmpl,
		cfg:               cfg,
		logger:            logger,
		sessions:          sessions,
		users:             users,
		projects:          projects,
		ledger:            ledgerStore,
		stories:           stories,
		tasks:             tasks,
		documents:         documents,
		repos:             repos,
		indexer:           indexer,
		workspaces:        workspaces,
		startedAt:         startedAt,
		globalAdminEmails: auth.LoadGlobalAdminEmails(),
	}, nil
}

// globalAdminChip reports whether the GLOBAL ADMIN nav badge should
// render for this request. story_3548cde2: shown when (a) the user is
// a global_admin AND (b) the active workspace differs from any
// workspace the user is a member of, signalling a cross-tenancy
// session.
func (p *Portal) globalAdminChip(user auth.User, active wsChip, memberships []string) bool {
	if !auth.IsGlobalAdmin(user, p.globalAdminEmails) {
		return false
	}
	if active.ID == "" {
		return false
	}
	for _, m := range memberships {
		if m == active.ID {
			return false
		}
	}
	return true
}

// wsChip is the view-model for a workspace shown in the switcher and
// breadcrumb. Kept terse so the same shape works for the dropdown items
// and the header label.
type wsChip struct {
	ID   string
	Name string
}

// displayWorkspaceName trims the seeded "Personal (<userID>)" suffix that
// workspace.EnsureDefault stamps on a freshly minted personal workspace
// (sty_26e2f2e5). The full id stays in storage; the switcher button, mobile
// active-line, and SSR <title> only need the short label. Custom names —
// anything that doesn't match the seeded shape exactly — are returned as-is.
func displayWorkspaceName(name string) string {
	if strings.HasPrefix(name, workspace.DefaultNamePrefix+" (") && strings.HasSuffix(name, ")") {
		return workspace.DefaultNamePrefix
	}
	return name
}

// buildPageTitle composes the SSR <title> per story_f7152e83. Pattern:
//
//	SATELLITES — <project|workspace>[ — <page>]
//
// projectName takes precedence over the active workspace name when both
// are provided. Either may be empty (e.g. unauthenticated landing). The
// separator is the em-dash U+2014.
func buildPageTitle(active wsChip, projectName, pageName string) string {
	parts := []string{"SATELLITES"}
	if projectName != "" {
		parts = append(parts, projectName)
	} else if active.Name != "" {
		parts = append(parts, active.Name)
	}
	if pageName != "" {
		parts = append(parts, pageName)
	}
	return strings.Join(parts, " — ")
}

// WSConfig is the websocket bootstrap payload emitted in the page head.
// WorkspaceID is empty on unauthenticated pages (login), causing the
// script bootstrap + connection-indicator widget to render as no-ops.
// Debug flips the debug panel behind `?debug=true`.
type WSConfig struct {
	WorkspaceID string
	Debug       bool
}

// buildWSConfig resolves the websocket bootstrap payload from the
// active workspace and the `?debug=true` query param.
func buildWSConfig(active wsChip, r *http.Request) WSConfig {
	return WSConfig{
		WorkspaceID: active.ID,
		Debug:       r.URL.Query().Get("debug") == "true",
	}
}

// memberWorkspaces returns the caller's full workspace membership set as
// view-model chips, plus the canonical id slice the store reads expect.
func (p *Portal) memberWorkspaces(r *http.Request, user auth.User) ([]wsChip, []string) {
	if p.workspaces == nil {
		return nil, nil
	}
	list, err := p.workspaces.ListByMember(r.Context(), user.ID)
	if err != nil || len(list) == 0 {
		return []wsChip{}, []string{}
	}
	chips := make([]wsChip, 0, len(list))
	ids := make([]string, 0, len(list))
	for _, w := range list {
		chips = append(chips, wsChip{ID: w.ID, Name: displayWorkspaceName(w.Name)})
		ids = append(ids, w.ID)
	}
	return chips, ids
}

// currentSession reads the session cookie. Returns (Session{}, false) when
// no valid session is present.
func (p *Portal) currentSession(r *http.Request) (auth.Session, bool) {
	id := auth.ReadCookie(r)
	if id == "" {
		return auth.Session{}, false
	}
	sess, err := p.sessions.Get(id)
	if err != nil {
		return auth.Session{}, false
	}
	return sess, true
}

// activeWorkspace returns the user's current scope chip + the id slice
// the store reads expect. When the session has an ActiveWorkspaceID and
// the user is still a member of it, scope narrows to that single workspace.
// Otherwise scope spans every workspace the user belongs to.
func (p *Portal) activeWorkspace(r *http.Request, user auth.User) (wsChip, []wsChip, []string) {
	chips, ids := p.memberWorkspaces(r, user)
	if chips == nil {
		return wsChip{}, nil, nil
	}
	if len(chips) == 0 {
		return wsChip{}, chips, []string{}
	}
	sess, ok := p.currentSession(r)
	if ok && sess.ActiveWorkspaceID != "" {
		for _, c := range chips {
			if c.ID == sess.ActiveWorkspaceID {
				return c, chips, []string{c.ID}
			}
		}
	}
	return chips[0], chips, ids
}

// resolveMemberships mirrors the MCP handler helper: nil when the workspace
// store is absent (pre-tenant tests), empty slice when the user has no
// memberships (deny-all), non-empty slice of workspace ids otherwise.
// When the session has a valid ActiveWorkspaceID the slice narrows to that
// workspace (sticky session scope); otherwise it spans every membership.
func (p *Portal) resolveMemberships(r *http.Request, user auth.User) []string {
	_, _, ids := p.activeWorkspace(r, user)
	return ids
}

// Register attaches the portal's routes to mux. Uses `{$}` for the exact-
// path landing so Go's ServeMux doesn't treat GET / as a subtree and clash
// with the `/mcp` mount point.
func (p *Portal) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", p.handleLanding)
	mux.HandleFunc("GET /login", p.handleLogin)
	mux.HandleFunc("GET /projects", p.handleProjectsList)
	mux.HandleFunc("GET /projects/{id}", p.handleProjectDetail)
	mux.HandleFunc("GET /projects/{id}/configuration", p.handleProjectConfiguration)
	mux.HandleFunc("GET /projects/{id}/ledger", p.handleProjectLedger)
	mux.HandleFunc("GET /projects/{id}/stories/{story_id}", p.handleStoryDetail)
	mux.HandleFunc("GET /projects/{id}/tasks", p.handleProjectTasks)
	mux.HandleFunc("GET /stories/{story_id}/walk", p.handleStoryWalk)
	mux.HandleFunc("GET /api/stories/{story_id}/composite", p.handleStoryComposite)
	mux.HandleFunc("GET /api/stories/{story_id}/activity", p.handleStoryActivity)
	mux.HandleFunc("GET /tasks", p.handleTasks)
	mux.HandleFunc("GET /api/tasks/{task_id}", p.handleTaskDrawer)
	mux.HandleFunc("GET /ledger", p.handleLedgerRedirect)
	mux.HandleFunc("GET /projects/{id}/api/ledger", p.handleProjectLedgerJSON)
	mux.HandleFunc("GET /documents", p.handleDocumentsList)
	mux.HandleFunc("GET /documents/{id}", p.handleDocumentDetail)
	mux.HandleFunc("GET /documents/{id}/versions/{version}", p.handleDocumentVersionDetail)
	mux.HandleFunc("GET /repo", p.handleRepoView)
	mux.HandleFunc("GET /api/repos/{id}/symbols", p.handleRepoSymbols)
	mux.HandleFunc("GET /api/repos/{id}/symbols/{symbol_id}", p.handleRepoSymbolSource)
	mux.HandleFunc("GET /api/repos/{id}/diff", p.handleRepoDiff)
	mux.HandleFunc("POST /api/stories/{id}/status", p.handleStoryStatusUpdate)
	mux.HandleFunc("GET /agents", p.handleAgents)
	mux.HandleFunc("GET /workspaces/select", p.handleWorkspaceSelect)
	mux.HandleFunc("POST /theme", p.handleThemeSet)
	mux.HandleFunc("GET /settings", p.handleSettings)
	mux.HandleFunc("GET /config", p.handleConfigPage)
	mux.HandleFunc("GET /help", p.handleHelpIndex)
	mux.HandleFunc("GET /help/{slug}", p.handleHelpDetail)
	mux.HandleFunc("GET /admin/system-config", p.handleAdminSystemConfig)
	mux.HandleFunc("POST /admin/system-config/reseed", p.handleAdminSystemConfigReseed)
	mux.HandleFunc("GET /admin/kv", p.handleAdminKV)
	mux.HandleFunc("POST /admin/kv/set", p.handleAdminKVSet)
	mux.HandleFunc("POST /admin/kv/delete", p.handleAdminKVDelete)
	static, err := pages.Static()
	if err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	}
}

type landingData struct {
	Title           string
	Version         string
	Build           string
	Commit          string
	StartedAt       string
	User            auth.User
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

type loginData struct {
	Title           string
	Version         string
	Commit          string
	Next            string
	GoogleEnabled   bool
	GithubEnabled   bool
	DevModeEnabled  bool
	DevUsername     string
	DevPassword     string
	DevMode         bool
	NoAuthBanner    bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

type projectsListData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Projects        []projectRow
	Disabled        bool
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

type projectDetailData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	OwnerYou        bool
	Composite       projectWorkspaceComposite
	Panels          []panel
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// projectRow is the view-model for a project — formats the timestamps to
// RFC3339 strings so the template stays free of time-formatting logic.
type projectRow struct {
	ID          string
	Name        string
	Status      string
	OwnerUserID string
	CreatedAt   string
	UpdatedAt   string
	// MCPURL is the resolved connection string the meta panel renders
	// in its `mcp` row (sty_0495f550). Either the persisted value or
	// the derived `<config.PublicURL>/mcp?project_id=<id>`. Empty when
	// the public base URL is unset; the template renders the
	// not-configured empty-state in that case.
	MCPURL string
	// MCPDerived is true when MCPURL was computed from PublicURL +
	// project_id rather than read from a persisted Project.MCPURL
	// field. The template renders a small (derived) suffix when set
	// — the schema-gap follow-up will land the persisted-value path.
	MCPDerived bool
}

// handleLanding gates GET / on a valid session. Authenticated users get
// the index.html dashboard; unauthenticated visitors get the landing page
// (story_92210e4a) — a merged hero + signin surface — instead of being
// redirected to /login. The mux pattern `GET /{$}` ensures only the exact
// "/" path reaches this handler.
func (p *Portal) handleLanding(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.renderLanding(w, r)
		return
	}
	// MCP OAuth bridge: when an already-logged-in user lands here with
	// the mcp_session_id cookie set (e.g. /oauth/authorize redirected
	// here without short-circuiting, or the AS shortcircuit failed mid-
	// flight), complete the pending OAuth flow and redirect to the MCP
	// client's redirect_uri instead of rendering the dashboard. The
	// bridge cookie is cleared on every attempt.
	if p.oauthServer != nil {
		if c, err := r.Cookie("mcp_session_id"); err == nil && c.Value != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     "mcp_session_id",
				Value:    "",
				Path:     "/",
				HttpOnly: true,
				Secure:   p.cfg.Env == "prod",
				SameSite: http.SameSiteLaxMode,
				MaxAge:   -1,
			})
			redirectURL, err := p.oauthServer.CompleteAuthorization(r.Context(), c.Value, user.ID)
			if err == nil {
				p.logger.Info().Str("user_id", user.ID).Msg("oauth: completed authorization via portal landing bridge")
				http.Redirect(w, r, redirectURL, http.StatusSeeOther)
				return
			}
			p.logger.Warn().Str("error", err.Error()).Msg("oauth: portal landing bridge complete failed; rendering dashboard")
		}
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	data := landingData{
		Title:           buildPageTitle(active, "", ""),
		Version:         config.Version,
		Build:           config.Build,
		Commit:          config.GitCommit,
		StartedAt:       p.startedAt.UTC().Format(time.RFC3339),
		User:            user,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		p.logger.Error().Str("template", "index.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// renderLanding emits the public landing page (story_92210e4a). Wordmark,
// subhead, OAuth buttons (gated by cfg), email/password form, 01/02/03
// grid, theme picker. Used by handleLanding when unauthenticated and by
// handleLogin (redirects to /).
func (p *Portal) renderLanding(w http.ResponseWriter, r *http.Request) {
	devEnabled := p.cfg.Env != "prod" && p.cfg.DevMode
	googleEnabled := p.cfg.GoogleClientID != "" && p.cfg.GoogleClientSecret != ""
	githubEnabled := p.cfg.GithubClientID != "" && p.cfg.GithubClientSecret != ""
	data := loginData{
		Title:           buildPageTitle(wsChip{}, "", ""),
		Version:         config.Version,
		Commit:          config.GitCommit,
		Next:            r.URL.Query().Get("next"),
		GoogleEnabled:   googleEnabled,
		GithubEnabled:   githubEnabled,
		DevModeEnabled:  devEnabled,
		DevMode:         devEnabled,
		DevUsername:     p.cfg.DevUsername,
		DevPassword:     p.cfg.DevPassword,
		NoAuthBanner:    !googleEnabled && !githubEnabled && !devEnabled,
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: "/",
	}
	if err := p.tmpl.ExecuteTemplate(w, "landing.html", data); err != nil {
		p.logger.Error().Str("template", "landing.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleLogin redirects /login → / so the landing page is the single
// canonical signin surface (story_92210e4a). The redirect preserves any
// `next` query param so the post-signin handler can land the user back on
// their target page.
func (p *Portal) handleLogin(w http.ResponseWriter, r *http.Request) {
	target := "/"
	if next := r.URL.Query().Get("next"); next != "" {
		target = "/?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleProjectsList renders the caller's projects, newest-first. A nil
// project.Store (no-DB dev) renders the Disabled panel instead of 500.
func (p *Portal) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	data := projectsListData{
		Title:           buildPageTitle(active, "", "projects"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if p.projects == nil {
		data.Disabled = true
	} else {
		list, err := p.projects.ListByOwner(r.Context(), user.ID, memberships)
		if err != nil {
			p.logger.Error().Str("error", err.Error()).Msg("projects list failed")
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		rows := make([]projectRow, 0, len(list))
		for _, pr := range list {
			rows = append(rows, viewRow(pr))
		}
		data.Projects = rows
	}
	if err := p.tmpl.ExecuteTemplate(w, "projects_list.html", data); err != nil {
		p.logger.Error().Str("template", "projects_list.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleProjectDetail renders the project by id. Cross-owner access returns
// 404 (not 403) so no owner-existence signal leaks.
func (p *Portal) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.projects == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	active, chips, memberships := p.activeWorkspace(r, user)
	pr, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || pr.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	filters := parseProjectWorkspaceFilters(r)
	isAdmin := p.isWorkspaceAdmin(r.Context(), active.ID, user.ID)
	composite := buildProjectWorkspaceComposite(r.Context(), p.stories, p.documents, p.repos, p.ledger, p.changelog, p.tasks, pr.ID, filters, memberships, isAdmin)
	data := projectDetailData{
		Title:           buildPageTitle(active, pr.Name, ""),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         p.projectRowWithMCP(pr, r),
		OwnerYou:        true,
		Composite:       composite,
		Panels:          defaultPanels(),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_detail.html", data); err != nil {
		p.logger.Error().Str("template", "project_detail.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type projectConfigurationData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	OwnerYou        bool
	Composite       projectConfigurationComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleProjectConfiguration renders the per-project Contracts + Skills
// surface. Cross-owner access returns 404 to avoid leaking project
// existence (mirrors handleProjectDetail).
func (p *Portal) handleProjectConfiguration(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.projects == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	active, chips, memberships := p.activeWorkspace(r, user)
	pr, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || pr.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	composite := buildProjectConfigurationComposite(r.Context(), p.documents, pr.ID, memberships)
	data := projectConfigurationData{
		Title:           buildPageTitle(active, pr.Name, "configuration"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(pr),
		OwnerYou:        true,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_configuration.html", data); err != nil {
		p.logger.Error().Str("template", "project_configuration.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type projectLedgerData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	Composite       ledgerComposite
	Disabled        bool
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleProjectLedger renders the upgraded ledger inspection view per
// docs/ui-design.md §2.4 (story_a9f8be3c). Default newest 50 rows;
// search + filter sidebar from query string; tailing toggle + N-new
// pill driven client-side. Owner-scoped; cross-owner returns 404.
func (p *Portal) handleProjectLedger(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.projects == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	active, chips, memberships := p.activeWorkspace(r, user)
	proj, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || proj.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	filters := parseLedgerFilters(r)
	data := projectLedgerData{
		Title:           buildPageTitle(active, proj.Name, "ledger"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(proj),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if p.ledger == nil {
		data.Disabled = true
	} else {
		data.Composite = buildLedgerComposite(r.Context(), p.ledger, proj.ID, filters, memberships)
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_ledger.html", data); err != nil {
		p.logger.Error().Str("template", "project_ledger.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleProjectLedgerJSON returns the ledger composite as JSON for the
// Alpine ledger_view.js factory's reload + filter-change path.
func (p *Portal) handleProjectLedgerJSON(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.projects == nil || p.ledger == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	_, _, memberships := p.activeWorkspace(r, user)
	proj, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || proj.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	composite := buildLedgerComposite(r.Context(), p.ledger, proj.ID, parseLedgerFilters(r), memberships)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(composite); err != nil {
		p.logger.Error().Str("error", err.Error()).Msg("ledger json encode failed")
	}
}

// handleLedgerRedirect resolves /ledger to the user's current project's
// ledger page — picks the first project in the active workspace. When
// no project exists, sends to /projects so the user can create one.
func (p *Portal) handleLedgerRedirect(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.projects == nil {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	_, _, memberships := p.activeWorkspace(r, user)
	list, err := p.projects.ListByOwner(r.Context(), user.ID, memberships)
	if err != nil || len(list) == 0 {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+list[0].ID+"/ledger", http.StatusSeeOther)
}

type storyDetailData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	Story           storyRow
	Composite       storyComposite
	Disabled        bool
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

type storyRow struct {
	ID                 string
	Title              string
	Description        string
	AcceptanceCriteria string
	Status             string
	Priority           string
	Category           string
	Tags               []string
	CreatedAt          string
	UpdatedAt          string
}

func viewStoryRow(s story.Story) storyRow {
	return storyRow{
		ID:                 s.ID,
		Title:              s.Title,
		Description:        s.Description,
		AcceptanceCriteria: s.AcceptanceCriteria,
		Status:             s.Status,
		Priority:           s.Priority,
		Category:           s.Category,
		Tags:               s.Tags,
		CreatedAt:          s.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:          s.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// handleStoryDetail renders the upgraded five-panel story view per
// docs/ui-design.md §2.2 (story_3b450d9e). Owner-scoped via project;
// cross-owner → 404. The composite (task chain + verdicts + commits
// + ledger excerpts + delivery strip) is built once via
// buildStoryComposite so the SSR template and the JSON composite
// endpoint render the same shape.
func (p *Portal) handleStoryDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.projects == nil || p.stories == nil {
		http.NotFound(w, r)
		return
	}
	projID := r.PathValue("id")
	storyID := r.PathValue("story_id")
	active, chips, memberships := p.activeWorkspace(r, user)
	proj, err := p.projects.GetByID(r.Context(), projID, memberships)
	if err != nil || proj.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	composite, err := buildStoryComposite(r.Context(), p.stories, p.documents, p.ledger, p.tasks, storyID, memberships)
	if err != nil || composite.Story.ID == "" || composite.Story.ID != storyID {
		http.NotFound(w, r)
		return
	}
	// Cross-project guard: composite.Story.ProjectID must match the
	// route's project — protects against story_id smuggled across
	// projects within the same membership set.
	s, err := p.stories.GetByID(r.Context(), storyID, memberships)
	if err != nil || s.ProjectID != proj.ID {
		http.NotFound(w, r)
		return
	}
	data := storyDetailData{
		Title:           buildPageTitle(active, proj.Name, s.Title),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(proj),
		Story:           composite.Story,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "story_detail.html", data); err != nil {
		p.logger.Error().Str("template", "story_detail.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleStoryComposite serves the story-view composite as JSON for the
// reconnect-refetch path (per docs/ui-design.md §3 reconnect policy).
// Workspace-scoped via memberships; cross-workspace → 404. Cross-owner
// project check is mirrored from handleStoryDetail.
func (p *Portal) handleStoryComposite(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.stories == nil {
		http.NotFound(w, r)
		return
	}
	storyID := r.PathValue("story_id")
	_, _, memberships := p.activeWorkspace(r, user)
	s, err := p.stories.GetByID(r.Context(), storyID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if p.projects != nil {
		proj, err := p.projects.GetByID(r.Context(), s.ProjectID, memberships)
		if err != nil || proj.OwnerUserID != user.ID {
			http.NotFound(w, r)
			return
		}
	}
	composite, err := buildStoryComposite(r.Context(), p.stories, p.documents, p.ledger, p.tasks, storyID, memberships)
	if err != nil || composite.Story.ID == "" || composite.Story.ID != storyID {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(composite); err != nil {
		p.logger.Error().Str("error", err.Error()).Msg("composite encode failed")
	}
}

// handleStoryActivity serves the per-story activity-log backfill (sty_e55f335e).
// Returns the curated subset of ledger rows scoped to the story —
// substrate-internal lifecycle events (plan, role-grant, agent-compose,
// action-claim, close-request, evidence, artifact, verdict, review q/a) —
// in time order. Workspace-scoped via memberships; cross-owner project
// check is mirrored from handleStoryComposite. The resolved kind set is
// returned alongside so the panel can render "showing N kinds" without
// a follow-up call.
func (p *Portal) handleStoryActivity(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.stories == nil || p.ledger == nil {
		http.NotFound(w, r)
		return
	}
	storyID := r.PathValue("story_id")
	active, _, memberships := p.activeWorkspace(r, user)
	s, err := p.stories.GetByID(r.Context(), storyID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if p.projects != nil {
		proj, err := p.projects.GetByID(r.Context(), s.ProjectID, memberships)
		if err != nil || proj.OwnerUserID != user.ID {
			http.NotFound(w, r)
			return
		}
	}
	kinds := resolveStoryActivityKinds(r.Context(), p.ledger, active.ID, s.ProjectID, memberships)
	rows := buildStoryActivity(r.Context(), p.ledger, s.ProjectID, storyID, kinds, memberships)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(storyActivityComposite{
		StoryID: storyID,
		Kinds:   kinds,
		Rows:    rows,
	}); err != nil {
		p.logger.Error().Str("error", err.Error()).Msg("story activity encode failed")
	}
}

type tasksPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       tasksComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleTasks renders the workspace-scoped task queue per ui-design
// §2.3 (story_f2d71c27). Three columns: in_flight / enqueued /
// recently closed. Live updates come from the workspace websocket.
// Unauth → /login. Empty memberships → empty composite (no leakage).
func (p *Portal) handleTasks(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	composite := buildTasksComposite(r.Context(), p.tasks, memberships)
	data := tasksPageData{
		Title:           buildPageTitle(active, "", "tasks"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "tasks.html", data); err != nil {
		p.logger.Error().Str("template", "tasks.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleTaskDrawer serves the per-task drawer payload as JSON for the
// click-to-open detail panel. Workspace-scoped via memberships;
// missing task or cross-workspace request → 404.
func (p *Portal) handleTaskDrawer(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.tasks == nil {
		http.NotFound(w, r)
		return
	}
	taskID := r.PathValue("task_id")
	_, _, memberships := p.activeWorkspace(r, user)
	d, err := buildTaskDrawer(r.Context(), p.tasks, p.ledger, "", taskID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(d); err != nil {
		p.logger.Error().Str("error", err.Error()).Msg("task drawer encode failed")
	}
}

type documentsListData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       documentsComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

type documentDetailData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	Detail          documentDetailComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleDocumentsList renders the documents browser at /documents per
// docs/ui-design.md §2.5 (story_5bc06738). Type tabs + search + sort
// in the querystring; cards rendered SSR with Alpine hydration.
func (p *Portal) handleDocumentsList(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	data := documentsListData{
		Title:           buildPageTitle(active, "", "documents"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildDocumentsComposite(r.Context(), p.documents, parseDocumentFilters(r), memberships),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "documents_list.html", data); err != nil {
		p.logger.Error().Str("template", "documents_list.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleDocumentDetail renders the per-document detail page with body,
// structured payload, linked stories, and version history.
func (p *Portal) handleDocumentDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.documents == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	active, chips, memberships := p.activeWorkspace(r, user)
	// Project for the linked-stories scan: pull from the document's
	// own project_id when set; otherwise pass empty (system-scope
	// docs aren't expected to have story citations).
	doc, err := p.documents.GetByID(r.Context(), id, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	projectID := ""
	var projRow projectRow
	if doc.ProjectID != nil {
		projectID = *doc.ProjectID
		if p.projects != nil {
			if proj, perr := p.projects.GetByID(r.Context(), projectID, memberships); perr == nil {
				projRow = viewRow(proj)
			}
		}
	}
	detail, err := buildDocumentDetail(r.Context(), p.documents, p.stories, projectID, id, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := documentDetailData{
		Title:           buildPageTitle(active, projRow.Name, detail.Document.Name),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         projRow,
		Detail:          detail,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "document_detail.html", data); err != nil {
		p.logger.Error().Str("template", "document_detail.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type documentVersionDetailData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	Document        documentCard
	VersionRow      versionDetailView
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleDocumentVersionDetail renders a single historical body of a
// document at /documents/{id}/versions/{version}. The user can compare
// against the live document by opening /documents/{id} in another tab.
func (p *Portal) handleDocumentVersionDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.documents == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	versionStr := r.PathValue("version")
	versionInt, err := strconv.Atoi(versionStr)
	if err != nil || versionInt <= 0 {
		http.NotFound(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	doc, err := p.documents.GetByID(r.Context(), id, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	versions, err := p.documents.ListVersions(r.Context(), id, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var match *document.DocumentVersion
	for i := range versions {
		if versions[i].Version == versionInt {
			match = &versions[i]
			break
		}
	}
	if match == nil {
		http.NotFound(w, r)
		return
	}
	var projRow projectRow
	if doc.ProjectID != nil && p.projects != nil {
		if proj, perr := p.projects.GetByID(r.Context(), *doc.ProjectID, memberships); perr == nil {
			projRow = viewRow(proj)
		}
	}
	data := documentVersionDetailData{
		Title:           buildPageTitle(active, projRow.Name, doc.Name+" · v"+versionStr),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         projRow,
		Document:        documentCardFor(doc),
		VersionRow:      versionDetailFromRow(*match),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "document_version_detail.html", data); err != nil {
		p.logger.Error().Str("template", "document_version_detail.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type repoViewData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       repoComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleRepoView renders the /repo page per ui-design §2.6
// (story_d4685302). When no project or no repo registered, renders the
// empty-state. Picks the user's first project's repo (first one in the
// project's repo list).
func (p *Portal) handleRepoView(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	projectID := ""
	if p.projects != nil {
		list, err := p.projects.ListByOwner(r.Context(), user.ID, memberships)
		if err == nil && len(list) > 0 {
			projectID = list[0].ID
		}
	}
	data := repoViewData{
		Title:           buildPageTitle(active, "", "repo"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildRepoComposite(r.Context(), p.repos, projectID, memberships, p.isWorkspaceAdmin(r.Context(), active.ID, user.ID)),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "repo.html", data); err != nil {
		p.logger.Error().Str("template", "repo.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleRepoSymbols proxies to codeindex.SearchSymbols. The RepoID
// from the path resolves to a Repo row; we use the repo's GitRemote
// as the codeindex key (matches what the production indexer uses
// when boot-loading repos).
func (p *Portal) handleRepoSymbols(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.repos == nil || p.indexer == nil {
		http.NotFound(w, r)
		return
	}
	repoID := r.PathValue("id")
	_, _, memberships := p.activeWorkspace(r, user)
	row, err := p.repos.GetByID(r.Context(), repoID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	body, err := p.indexer.SearchSymbols(r.Context(), row.GitRemote, q.Get("q"), q.Get("kind"), q.Get("language"))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"index_unavailable","symbols":[]}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleRepoSymbolSource proxies to codeindex.GetSymbolSource for the
// drawer view.
func (p *Portal) handleRepoSymbolSource(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.repos == nil || p.indexer == nil {
		http.NotFound(w, r)
		return
	}
	repoID := r.PathValue("id")
	symbolID := r.PathValue("symbol_id")
	_, _, memberships := p.activeWorkspace(r, user)
	row, err := p.repos.GetByID(r.Context(), repoID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := p.indexer.GetSymbolSource(r.Context(), row.GitRemote, symbolID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"index_unavailable"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleRepoDiff returns the branch-diff JSON for the repo at /api/repos/{id}/diff.
// Query params: from, to. Reads the diff via repo.Store.Diff which
// walks the persisted commit chain.
func (p *Portal) handleRepoDiff(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.repos == nil {
		http.NotFound(w, r)
		return
	}
	repoID := r.PathValue("id")
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	_, _, memberships := p.activeWorkspace(r, user)
	if _, err := p.repos.GetByID(r.Context(), repoID, memberships); err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := p.repos.Diff(r.Context(), repoID, from, to, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(d); err != nil {
		p.logger.Error().Str("error", err.Error()).Msg("repo diff encode failed")
	}
}

type agentsPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       agentsComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleAgents renders the /agents page.
func (p *Portal) handleAgents(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	data := agentsPageData{
		Title:           buildPageTitle(active, "", "agents"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildAgentsComposite(r.Context(), p.documents, memberships, parseAgentFilter(r)),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "agents.html", data); err != nil {
		p.logger.Error().Str("template", "agents.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type configPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       configComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleConfigPage renders the top-menu /config page (story_644a2eb1).
// Lists every visible Configuration document, lets the operator select
// one via the dropdown, and renders the resolved workflow / contracts /
// skills / principles sections for the selected Configuration. The
// selection is carried in `?id=<configID>` so the SSR page is bookmarkable.
func (p *Portal) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	composite := buildConfigComposite(r.Context(), p.documents, memberships, active.ID, "", user.ID)
	data := configPageData{
		Title:           buildPageTitle(active, "", "config"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "configuration.html", data); err != nil {
		p.logger.Error().Str("template", "configuration.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// isWorkspaceAdmin returns true when the user holds RoleAdmin in
// workspaceID. False for unauthenticated, non-member, or when the
// workspaces store is absent (pre-tenant tests).
func (p *Portal) isWorkspaceAdmin(ctx context.Context, workspaceID, userID string) bool {
	if p.workspaces == nil || workspaceID == "" || userID == "" {
		return false
	}
	role, err := p.workspaces.GetRole(ctx, workspaceID, userID)
	if err != nil {
		return false
	}
	return role == workspace.RoleAdmin
}

// handleWorkspaceSelect persists the chosen workspace on the session and
// redirects back to ?next= (default /). Rejects unauthenticated callers
// (redirect to login) and rejects switching to a workspace the user is
// not a member of (302 back to ?next= without changing session — the
// caller's view stays scoped to whatever they had before).
func (p *Portal) handleWorkspaceSelect(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	target := r.URL.Query().Get("id")
	next := r.URL.Query().Get("next")
	if next == "" {
		next = "/"
	}
	if p.workspaces == nil || target == "" {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	is, err := p.workspaces.IsMember(r.Context(), target, user.ID)
	if err != nil || !is {
		// Cross-workspace switch attempt — silently ignore. The next
		// request still resolves the prior active workspace.
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	sess, ok := p.currentSession(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if err := p.sessions.SetActiveWorkspace(sess.ID, target); err != nil {
		p.logger.Warn().Str("error", err.Error()).Msg("set active workspace failed")
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func viewRow(p project.Project) projectRow {
	return projectRow{
		ID:          p.ID,
		Name:        p.Name,
		Status:      p.Status,
		OwnerUserID: p.OwnerUserID,
		CreatedAt:   p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// projectRowWithMCP is viewRow + the resolved MCP URL fields. Used by
// the project detail / configuration handlers; the projects-list page
// stays on the lighter viewRow shape so the row is cheap to compute.
//
// MCP URL resolution chain:
//  1. pr.MCPURL persisted on the project row (explicit override).
//  2. cfg.PublicURL — admin-set base URL for the deployment.
//  3. Derived from the inbound request (`<scheme>://<host>`) — V3
//     parity, no env var required. The user is already connected to
//     the satellites portal, so the host they reached is the right
//     one to paste back into .mcp.json.
//
// (1) and (2) cover ops scenarios where the public URL differs from
// what the browser sees (e.g. internal preview vs canonical prod URL).
// (3) is the default and removes the "set SATELLITES_PUBLIC_URL or
// the panel is empty" footgun.
func (p *Portal) projectRowWithMCP(pr project.Project, r *http.Request) projectRow {
	row := viewRow(pr)
	base := p.cfg.PublicURL
	if base == "" && r != nil {
		base = baseURLFromRequest(r)
	}
	row.MCPURL = project.ResolveMCPURL(pr, base)
	row.MCPDerived = pr.MCPURL == "" && row.MCPURL != ""
	return row
}

// baseURLFromRequest reconstructs the externally-visible base URL the
// caller used to reach this server. Mirrors the OAuthServer's
// issuerForRequest pattern: prefer X-Forwarded-Proto when set (Fly's
// edge populates it), fall back to the request's TLS state, default
// to http.
func baseURLFromRequest(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (p *Portal) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := url.QueryEscape(r.URL.RequestURI())
	http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
}

// resolveUser returns the user when a valid session cookie is present,
// otherwise zero + false. A missing user row on a present session id is
// treated as unauthenticated.
func (p *Portal) resolveUser(r *http.Request) (auth.User, bool) {
	id := auth.ReadCookie(r)
	if id == "" {
		return auth.User{}, false
	}
	sess, err := p.sessions.Get(id)
	if err != nil {
		return auth.User{}, false
	}
	user, err := p.users.GetByID(sess.UserID)
	if err != nil {
		return auth.User{}, false
	}
	return user, true
}

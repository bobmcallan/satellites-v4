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
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
	"github.com/bobmcallan/satellites/pages"
)

// Portal wires template rendering, the auth dependencies, and the static
// filesystem into a set of http.Handlers.
type Portal struct {
	tmpl       *template.Template
	cfg        *config.Config
	logger     arbor.ILogger
	sessions   auth.SessionStore
	users      auth.UserStoreByID
	projects   project.Store
	ledger     ledger.Store
	stories    story.Store
	contracts  contract.Store
	tasks      task.Store
	documents  document.Store
	repos      repo.Store
	indexer    codeindex.Indexer
	grants     rolegrant.Store
	workspaces workspace.Store
	startedAt  time.Time
}

// New constructs the Portal handler set. Template parsing errors return
// immediately so main() can exit with a clear message. Nil store args
// disable the corresponding page group (the handlers render a "disabled"
// panel or 404). A nil workspaces store keeps the pre-tenant behaviour
// (membership scoping disabled) — used by tests that don't need it. A
// nil contracts store renders the story-view CI timeline as empty
// (slice 11.1: contract instances panel needs the store; absence means
// no panel content rather than a 500). A nil tasks store keeps the
// /tasks page reachable but renders empty columns (slice 11.2 same
// degradation pattern).
func New(cfg *config.Config, logger arbor.ILogger, sessions auth.SessionStore, users auth.UserStoreByID, projects project.Store, ledgerStore ledger.Store, stories story.Store, contracts contract.Store, tasks task.Store, documents document.Store, repos repo.Store, indexer codeindex.Indexer, grants rolegrant.Store, workspaces workspace.Store, startedAt time.Time) (*Portal, error) {
	tmpl, err := pages.Templates()
	if err != nil {
		return nil, err
	}
	return &Portal{
		tmpl:       tmpl,
		cfg:        cfg,
		logger:     logger,
		sessions:   sessions,
		users:      users,
		projects:   projects,
		ledger:     ledgerStore,
		stories:    stories,
		contracts:  contracts,
		tasks:      tasks,
		documents:  documents,
		repos:      repos,
		indexer:    indexer,
		grants:     grants,
		workspaces: workspaces,
		startedAt:  startedAt,
	}, nil
}

// wsChip is the view-model for a workspace shown in the switcher and
// breadcrumb. Kept terse so the same shape works for the dropdown items
// and the header label.
type wsChip struct {
	ID   string
	Name string
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
		chips = append(chips, wsChip{ID: w.ID, Name: w.Name})
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
	mux.HandleFunc("GET /projects/{id}/ledger", p.handleProjectLedger)
	mux.HandleFunc("GET /projects/{id}/stories", p.handleStoriesList)
	mux.HandleFunc("GET /projects/{id}/stories/{story_id}", p.handleStoryDetail)
	mux.HandleFunc("GET /api/stories/{story_id}/composite", p.handleStoryComposite)
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
	mux.HandleFunc("POST /api/repos/{id}/reindex", p.handleRepoReindex)
	mux.HandleFunc("GET /roles", p.handleRoles)
	mux.HandleFunc("GET /agents", p.handleAgents)
	mux.HandleFunc("GET /grants", p.handleGrants)
	mux.HandleFunc("POST /api/grants/{id}/release", p.handleGrantRelease)
	mux.HandleFunc("GET /workspaces/select", p.handleWorkspaceSelect)
	mux.HandleFunc("POST /theme", p.handleThemeSet)
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
	WSConfig        WSConfig
}

type projectDetailData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	OwnerYou        bool
	Workspaces      []wsChip
	ActiveWorkspace wsChip
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
	active, chips, _ := p.activeWorkspace(r, user)
	data := landingData{
		Title:           "home",
		Version:         config.Version,
		Build:           config.Build,
		Commit:          config.GitCommit,
		StartedAt:       p.startedAt.UTC().Format(time.RFC3339),
		User:            user,
		Workspaces:      chips,
		ActiveWorkspace: active,
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
	data := loginData{
		Title:           "satellites",
		Version:         config.Version,
		Commit:          config.GitCommit,
		Next:            r.URL.Query().Get("next"),
		GoogleEnabled:   p.cfg.GoogleClientID != "" && p.cfg.GoogleClientSecret != "",
		GithubEnabled:   p.cfg.GithubClientID != "" && p.cfg.GithubClientSecret != "",
		DevModeEnabled:  p.cfg.Env != "prod" && p.cfg.DevMode,
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
		Title:           "projects",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Workspaces:      chips,
		ActiveWorkspace: active,
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
	data := projectDetailData{
		Title:           pr.Name,
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(pr),
		OwnerYou:        true,
		Workspaces:      chips,
		ActiveWorkspace: active,
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_detail.html", data); err != nil {
		p.logger.Error().Str("template", "project_detail.html").Str("error", err.Error()).Msg("template render failed")
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
		Title:           proj.Name + " · ledger",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(proj),
		Workspaces:      chips,
		ActiveWorkspace: active,
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

type storiesListData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	Stories         []storyRow
	StatusAll       bool
	Disabled        bool
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	WSConfig        WSConfig
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

// handleStoriesList renders the project's stories. Default filter excludes
// done + cancelled (matches the MCP story_list default intent); ?status=all
// lifts the filter, ?status=<value> applies it exactly.
func (p *Portal) handleStoriesList(w http.ResponseWriter, r *http.Request) {
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
	data := storiesListData{
		Title:           proj.Name + " · stories",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(proj),
		Workspaces:      chips,
		ActiveWorkspace: active,
		WSConfig:        buildWSConfig(active, r),
	}
	if p.stories == nil {
		data.Disabled = true
	} else {
		statusParam := r.URL.Query().Get("status")
		data.StatusAll = statusParam == "all"
		opts := story.ListOptions{}
		if statusParam != "" && statusParam != "all" {
			opts.Status = statusParam
		}
		list, err := p.stories.List(r.Context(), proj.ID, opts, memberships)
		if err != nil {
			p.logger.Error().Str("error", err.Error()).Msg("stories list failed")
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		rows := make([]storyRow, 0, len(list))
		for _, s := range list {
			if !data.StatusAll && (s.Status == story.StatusDone || s.Status == story.StatusCancelled) {
				continue
			}
			rows = append(rows, viewStoryRow(s))
		}
		data.Stories = rows
	}
	if err := p.tmpl.ExecuteTemplate(w, "stories_list.html", data); err != nil {
		p.logger.Error().Str("template", "stories_list.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleStoryDetail renders the upgraded five-panel story view per
// docs/ui-design.md §2.2 (story_3b450d9e). Owner-scoped via project;
// cross-owner → 404. The composite (CIs + verdicts + commits + ledger
// excerpts + delivery strip) is built once via buildStoryComposite so
// the SSR template and the JSON composite endpoint render the same
// shape.
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
	composite, err := buildStoryComposite(r.Context(), p.stories, p.contracts, p.ledger, storyID, memberships)
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
		Title:           s.Title,
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(proj),
		Story:           composite.Story,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
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
	composite, err := buildStoryComposite(r.Context(), p.stories, p.contracts, p.ledger, storyID, memberships)
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

type tasksPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       tasksComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
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
		Title:           "tasks",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
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
		Title:           "documents",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildDocumentsComposite(r.Context(), p.documents, parseDocumentFilters(r), memberships),
		Workspaces:      chips,
		ActiveWorkspace: active,
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
		Title:           detail.Document.Name,
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         projRow,
		Detail:          detail,
		Workspaces:      chips,
		ActiveWorkspace: active,
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
		Title:           doc.Name,
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         projRow,
		Document:        documentCardFor(doc),
		VersionRow:      versionDetailFromRow(*match),
		Workspaces:      chips,
		ActiveWorkspace: active,
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
		Title:           "repo",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildRepoComposite(r.Context(), p.repos, projectID, memberships, p.isWorkspaceAdmin(r.Context(), active.ID, user.ID)),
		Workspaces:      chips,
		ActiveWorkspace: active,
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

// handleRepoReindex enqueues a reindex task for the repo at
// /api/repos/{id}/reindex. Admin gate per AC: the caller must hold
// RoleAdmin in the active workspace; non-admins → 403. Returns 202
// with the task id on success.
func (p *Portal) handleRepoReindex(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.repos == nil || p.tasks == nil {
		http.NotFound(w, r)
		return
	}
	repoID := r.PathValue("id")
	active, _, memberships := p.activeWorkspace(r, user)
	if !p.isWorkspaceAdmin(r.Context(), active.ID, user.ID) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	row, err := p.repos.GetByID(r.Context(), repoID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	taskID := repo.EnqueueReindex(r.Context(), p.tasks, p.ledger, row, "portal", row.HeadSHA, time.Now().UTC())
	if taskID == "" {
		http.Error(w, `{"error":"enqueue_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	body, _ := json.Marshal(map[string]any{
		"task_id": taskID,
		"repo_id": row.ID,
	})
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

type rolesPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       rolesComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	WSConfig        WSConfig
}

type agentsPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       agentsComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	WSConfig        WSConfig
}

type grantsPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       grantsComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	WSConfig        WSConfig
}

// handleRoles renders the /roles page per ui-design#roles
// (story_5cc349a9). Lists role documents with active-grant counts.
func (p *Portal) handleRoles(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	data := rolesPageData{
		Title:           "roles",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildRolesComposite(r.Context(), p.documents, p.grants, memberships),
		Workspaces:      chips,
		ActiveWorkspace: active,
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "roles.html", data); err != nil {
		p.logger.Error().Str("template", "roles.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
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
		Title:           "agents",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildAgentsComposite(r.Context(), p.documents, memberships),
		Workspaces:      chips,
		ActiveWorkspace: active,
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "agents.html", data); err != nil {
		p.logger.Error().Str("template", "agents.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleGrants renders the /grants live panel. The IsAdmin flag drives
// the visibility of the Revoke button.
func (p *Portal) handleGrants(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	data := grantsPageData{
		Title:           "grants",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       buildGrantsComposite(r.Context(), p.grants, p.documents, memberships, p.isWorkspaceAdmin(r.Context(), active.ID, user.ID)),
		Workspaces:      chips,
		ActiveWorkspace: active,
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "grants.html", data); err != nil {
		p.logger.Error().Str("template", "grants.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleGrantRelease releases a role-grant on behalf of an admin
// caller. Non-admins receive 403; missing grants return 404.
func (p *Portal) handleGrantRelease(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.grants == nil {
		http.NotFound(w, r)
		return
	}
	active, _, memberships := p.activeWorkspace(r, user)
	if !p.isWorkspaceAdmin(r.Context(), active.ID, user.ID) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if _, err := p.grants.Release(r.Context(), id, "revoked via portal", time.Now().UTC(), memberships); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
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

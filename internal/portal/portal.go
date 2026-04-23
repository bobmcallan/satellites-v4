// Package portal hosts the satellites v4 SSR handlers. It owns the login page,
// the authenticated landing, and the static-asset mount. Later epics attach
// primitive views to this surface.
package portal

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ternarybob/arbor"

	"encoding/json"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
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
	workspaces workspace.Store
	startedAt  time.Time
}

// New constructs the Portal handler set. Template parsing errors return
// immediately so main() can exit with a clear message. Nil store args
// disable the corresponding page group (the handlers render a "disabled"
// panel or 404). A nil workspaces store keeps the pre-tenant behaviour
// (membership scoping disabled) — used by tests that don't need it.
func New(cfg *config.Config, logger arbor.ILogger, sessions auth.SessionStore, users auth.UserStoreByID, projects project.Store, ledgerStore ledger.Store, stories story.Store, workspaces workspace.Store, startedAt time.Time) (*Portal, error) {
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
		workspaces: workspaces,
		startedAt:  startedAt,
	}, nil
}

// resolveMemberships mirrors the MCP handler helper: nil when the workspace
// store is absent (pre-tenant tests), empty slice when the user has no
// memberships (deny-all), non-empty slice of workspace ids otherwise.
func (p *Portal) resolveMemberships(r *http.Request, user auth.User) []string {
	if p.workspaces == nil {
		return nil
	}
	list, err := p.workspaces.ListByMember(r.Context(), user.ID)
	if err != nil || len(list) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, w.ID)
	}
	return out
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
	static, err := pages.Static()
	if err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	}
}

type landingData struct {
	Title     string
	Version   string
	Build     string
	Commit    string
	StartedAt string
	User      auth.User
}

type loginData struct {
	Title          string
	Version        string
	Commit         string
	Next           string
	GoogleEnabled  bool
	GithubEnabled  bool
	DevModeEnabled bool
}

type projectsListData struct {
	Title    string
	Version  string
	Commit   string
	User     auth.User
	Projects []projectRow
	Disabled bool
}

type projectDetailData struct {
	Title    string
	Version  string
	Commit   string
	User     auth.User
	Project  projectRow
	OwnerYou bool
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

// handleLanding gates GET / on a valid session. Unauth redirects to /login
// preserving the original URL via ?next=. The mux pattern `GET /{$}` ensures
// only the exact "/" path reaches this handler.
func (p *Portal) handleLanding(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	data := landingData{
		Title:     "home",
		Version:   config.Version,
		Build:     config.Build,
		Commit:    config.GitCommit,
		StartedAt: p.startedAt.UTC().Format(time.RFC3339),
		User:      user,
	}
	if err := p.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		p.logger.Error().Str("template", "index.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleLogin renders /login with provider buttons derived from cfg.
func (p *Portal) handleLogin(w http.ResponseWriter, r *http.Request) {
	data := loginData{
		Title:          "sign in",
		Version:        config.Version,
		Commit:         config.GitCommit,
		Next:           r.URL.Query().Get("next"),
		GoogleEnabled:  p.cfg.GoogleClientID != "" && p.cfg.GoogleClientSecret != "",
		GithubEnabled:  p.cfg.GithubClientID != "" && p.cfg.GithubClientSecret != "",
		DevModeEnabled: p.cfg.Env != "prod" && p.cfg.DevMode,
	}
	if err := p.tmpl.ExecuteTemplate(w, "login.html", data); err != nil {
		p.logger.Error().Str("template", "login.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// handleProjectsList renders the caller's projects, newest-first. A nil
// project.Store (no-DB dev) renders the Disabled panel instead of 500.
func (p *Portal) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	data := projectsListData{
		Title:   "projects",
		Version: config.Version,
		Commit:  config.GitCommit,
		User:    user,
	}
	if p.projects == nil {
		data.Disabled = true
	} else {
		list, err := p.projects.ListByOwner(r.Context(), user.ID, p.resolveMemberships(r, user))
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
	pr, err := p.projects.GetByID(r.Context(), id, p.resolveMemberships(r, user))
	if err != nil || pr.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	data := projectDetailData{
		Title:    pr.Name,
		Version:  config.Version,
		Commit:   config.GitCommit,
		User:     user,
		Project:  viewRow(pr),
		OwnerYou: true,
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_detail.html", data); err != nil {
		p.logger.Error().Str("template", "project_detail.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type projectLedgerData struct {
	Title    string
	Version  string
	Commit   string
	User     auth.User
	Project  projectRow
	Entries  []ledgerRow
	Limit    int
	Disabled bool
}

// ledgerRow pre-formats ledger fields for the template.
type ledgerRow struct {
	ID        string
	Type      string
	Actor     string
	Content   string
	CreatedAt string
}

// handleProjectLedger renders a read-only tail of the project's ledger,
// newest-first. Owner-scoped; cross-owner returns 404. ?limit= clamps via
// the Store's normalisation (default 100, max 500).
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
	memberships := p.resolveMemberships(r, user)
	proj, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || proj.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	data := projectLedgerData{
		Title:   proj.Name + " · ledger",
		Version: config.Version,
		Commit:  config.GitCommit,
		User:    user,
		Project: viewRow(proj),
	}
	if p.ledger == nil {
		data.Disabled = true
	} else {
		limit := 0
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			}
		}
		entries, err := p.ledger.List(r.Context(), proj.ID, ledger.ListOptions{Limit: limit}, memberships)
		if err != nil {
			p.logger.Error().Str("error", err.Error()).Msg("ledger list failed")
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		rows := make([]ledgerRow, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, ledgerRow{
				ID:        e.ID,
				Type:      e.Type,
				Actor:     e.Actor,
				Content:   e.Content,
				CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		data.Entries = rows
		data.Limit = len(rows)
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_ledger.html", data); err != nil {
		p.logger.Error().Str("template", "project_ledger.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type storiesListData struct {
	Title      string
	Version    string
	Commit     string
	User       auth.User
	Project    projectRow
	Stories    []storyRow
	StatusAll  bool
	Disabled   bool
}

type storyDetailData struct {
	Title     string
	Version   string
	Commit    string
	User      auth.User
	Project   projectRow
	Story     storyRow
	History   []historyRow
	Disabled  bool
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

type historyRow struct {
	CreatedAt string
	From      string
	To        string
	Actor     string
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
	memberships := p.resolveMemberships(r, user)
	proj, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || proj.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	data := storiesListData{
		Title:   proj.Name + " · stories",
		Version: config.Version,
		Commit:  config.GitCommit,
		User:    user,
		Project: viewRow(proj),
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

// handleStoryDetail renders a single story with its status-change history
// drawn from the ledger. Owner-scoped via project; cross-owner → 404.
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
	memberships := p.resolveMemberships(r, user)
	proj, err := p.projects.GetByID(r.Context(), projID, memberships)
	if err != nil || proj.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	s, err := p.stories.GetByID(r.Context(), storyID, memberships)
	if err != nil || s.ProjectID != proj.ID {
		http.NotFound(w, r)
		return
	}
	data := storyDetailData{
		Title:   s.Title,
		Version: config.Version,
		Commit:  config.GitCommit,
		User:    user,
		Project: viewRow(proj),
		Story:   viewStoryRow(s),
	}
	if p.ledger != nil {
		entries, err := p.ledger.List(r.Context(), proj.ID, ledger.ListOptions{Type: story.LedgerEntryType, Limit: 50}, memberships)
		if err != nil {
			p.logger.Error().Str("error", err.Error()).Msg("ledger list failed")
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		rows := make([]historyRow, 0)
		for _, e := range entries {
			var payload struct {
				StoryID string `json:"story_id"`
				From    string `json:"from"`
				To      string `json:"to"`
				Actor   string `json:"actor"`
			}
			if err := json.Unmarshal([]byte(e.Content), &payload); err != nil {
				continue
			}
			if payload.StoryID != s.ID {
				continue
			}
			rows = append(rows, historyRow{
				CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
				From:      payload.From,
				To:        payload.To,
				Actor:     payload.Actor,
			})
		}
		data.History = rows
	}
	if err := p.tmpl.ExecuteTemplate(w, "story_detail.html", data); err != nil {
		p.logger.Error().Str("template", "story_detail.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
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

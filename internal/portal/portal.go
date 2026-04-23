// Package portal hosts the satellites v4 SSR handlers. It owns the login page,
// the authenticated landing, and the static-asset mount. Later epics attach
// primitive views to this surface.
package portal

import (
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/pages"
)

// Portal wires template rendering, the auth dependencies, and the static
// filesystem into a set of http.Handlers.
type Portal struct {
	tmpl      *template.Template
	cfg       *config.Config
	logger    arbor.ILogger
	sessions  auth.SessionStore
	users     auth.UserStoreByID
	startedAt time.Time
}

// New constructs the Portal handler set. Template parsing errors return
// immediately so main() can exit with a clear message.
func New(cfg *config.Config, logger arbor.ILogger, sessions auth.SessionStore, users auth.UserStoreByID, startedAt time.Time) (*Portal, error) {
	tmpl, err := pages.Templates()
	if err != nil {
		return nil, err
	}
	return &Portal{
		tmpl:      tmpl,
		cfg:       cfg,
		logger:    logger,
		sessions:  sessions,
		users:     users,
		startedAt: startedAt,
	}, nil
}

// Register attaches the portal's routes to mux. Uses `{$}` for the exact-
// path landing so Go's ServeMux doesn't treat GET / as a subtree and clash
// with the `/mcp` mount point.
func (p *Portal) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", p.handleLanding)
	mux.HandleFunc("GET /login", p.handleLogin)
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

// handleLanding gates GET / on a valid session. Unauth redirects to /login
// preserving the original URL via ?next=. The mux pattern `GET /{$}` ensures
// only the exact "/" path reaches this handler.
func (p *Portal) handleLanding(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		next := url.QueryEscape(r.URL.RequestURI())
		http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
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

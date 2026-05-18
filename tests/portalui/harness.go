//go:build portalui

// Package portalui hosts the chromedp-driven E2E suite for the portal's
// websocket connection indicator widget (story_0e5328cd, follow-up to the
// 10.4 widget shipped in story_ac3e4057).
//
// Tests in this package boot an in-process satellites server using the
// production constructors (auth.Handlers, portal.New, wshandler.New)
// wired against the package-internal memory stores plus a harness-local
// wshandler.EventSource stub (harness_source.go), then drive a headless
// Chromium via github.com/chromedp/chromedp to assert the widget's
// state transitions.
//
// The package is gated by the `portalui` build tag so the chromedp + ws
// transitive deps stay out of the default `go test ./...` run. Invoke
// the suite explicitly:
//
//	go test -tags=portalui ./tests/portalui/... -timeout=120s
//
// Tests skip cleanly via t.Skip when no chromium binary is reachable —
// see chrome.go.
package portalui

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ternarybob/arbor"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/httpserver"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/portal"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
	"github.com/bobmcallan/satellites/internal/wshandler"
)

// Harness owns the in-process satellites server plus the test-only knobs
// (DisableWS / EnableWS, PublishEvent) the chromedp tests use to simulate
// outage and inject observable hub events.

// HarnessProjectName is the explicit project the portalui harness creates
// after seeding its dev user. Tests assert visibility against this name
// (rather than the legacy "Default" auto-seed string) — sty_c975ebeb.
const HarnessProjectName = "harness-test"

type Harness struct {
	Server      *httptest.Server
	BaseURL     string
	// Source is the harness-local wshandler.EventSource — the test
	// substitute for production SurrealLiveSource (sty_010a0543).
	// Tests fan events out through Source.Publish (called via
	// PublishEvent / UpdateStoryStatus).
	Source      *harnessSource
	UserID      string
	WorkspaceID string

	// AuthHandlers is the live auth handler set, exposed so OAuth tests can
	// inject a ProviderSet pointing at a stub provider before the test
	// drives chromedp.
	AuthHandlers *auth.Handlers

	// SessionCookieValue is the pre-baked session id; tests inject it via
	// chromedp's network.SetCookie under auth.CookieName so the indicator
	// widget renders without driving the login form.
	SessionCookieValue string

	// Stores exposed for chromedp tests that need to seed fixtures
	// (project + story rows the story-view page reads). Kept on the
	// harness so each test owns its own server + isolated state.
	Projects  *project.MemoryStore
	Stories   *story.MemoryStore
	Ledger    *ledger.MemoryStore
	Tasks     *task.MemoryStore
	Documents *document.MemoryStore
	Repos     *repo.MemoryStore

	// wsEnabled gates the /ws upgrade. When false, /ws returns 503 and any
	// previously upgraded conns are closed (see DisableWS).
	wsEnabled atomic.Bool

	// connTracker holds every net.Conn that handled a /ws request. Closing
	// these conns terminates the upgraded websockets without restarting the
	// test server.
	tracker *connTracker
}

// StartHarness boots the in-process server, seeds a user + workspace +
// session, and returns a ready-to-use Harness. Call Close on cleanup.
func StartHarness(t *testing.T) *Harness {
	t.Helper()

	cfg := &config.Config{
		Port:        0, // unused — httptest binds the listener
		Env:         "dev",
		LogLevel:    "warn",
		DevMode:     true,
		DevUsername: "dev@local",
		DevPassword: "letmein",
		DocsDir:     t.TempDir(),
	}

	logger := satarbor.New(cfg.LogLevel)
	startedAt := time.Now()

	users := auth.NewMemoryUserStore()
	sessions := auth.NewMemorySessionStore()

	// Seed the session user — id matches the DevMode shape (see auth.Handlers
	// authenticate()) but we register up-front so we can mint a session
	// without driving the login flow.
	user := auth.User{
		ID:          "dev-user",
		Email:       cfg.DevUsername,
		DisplayName: "Dev User",
		Provider:    "devmode",
	}
	users.Add(user)

	wsStore := workspace.NewMemoryStore()
	now := time.Now().UTC()
	ws, err := wsStore.Create(context.Background(), user.ID, "personal", now)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	ledgerStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledgerStore)
	projectStore := project.NewMemoryStore()
	// Production no longer auto-seeds a per-user Default project on first
	// login (sty_c975ebeb). The harness creates one explicit project so
	// tests still have something to render against; assertions reference
	// this name rather than the legacy "Default" literal.
	if _, err := projectStore.Create(context.Background(), user.ID, ws.ID, HarnessProjectName, now); err != nil {
		t.Fatalf("seed harness project: %v", err)
	}
	docStore := document.NewMemoryStore()
	taskStore := task.NewMemoryStore()
	repoStore := repo.NewMemoryStore()

	portalHandlers, err := portal.New(cfg, logger, sessions, users, projectStore, ledgerStore, storyStore, taskStore, docStore, repoStore, codeindex.NewStub(), wsStore, startedAt)
	if err != nil {
		t.Fatalf("portal.New: %v", err)
	}

	authHandlers := &auth.Handlers{
		Users:    users,
		Sessions: sessions,
		Logger:   logger,
		Cfg:      cfg,
		States:   auth.NewStateStore(10 * time.Minute),
	}

	source := newHarnessSource(wsStore)

	wsHandlers := wshandler.New(wshandler.Deps{
		Source: source,
		Sessions: wshandler.SessionResolverFunc(func(_ context.Context, sid string) (auth.User, error) {
			sess, err := sessions.Get(sid)
			if err != nil {
				return auth.User{}, err
			}
			return users.GetByID(sess.UserID)
		}),
		Logger: logger,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	authHandlers.Register(mux)
	portalHandlers.Register(mux)

	// Wrap the mux with the production security-headers middleware so
	// chromedp tests render under the same Content-Security-Policy as
	// pprod (story_a7297367). Without this, CSP-gated regressions like
	// the Alpine unsafe-eval block silently pass the harness while
	// breaking real users.
	wrapped := httpserver.SecurityHeaders(false, mux)

	h := &Harness{
		Source:       source,
		AuthHandlers: authHandlers,
		UserID:       user.ID,
		WorkspaceID:  ws.ID,
		Projects:     projectStore,
		Stories:      storyStore,
		Ledger:       ledgerStore,
		Tasks:        taskStore,
		Documents:    docStore,
		Repos:        repoStore,
		tracker:      newConnTracker(),
	}
	h.wsEnabled.Store(true)

	// Wrap the wshandler with the kill-switch + connection tracker so
	// DisableWS can return 503 to new attempts and close in-flight conns.
	mux.Handle("GET /ws", h.gateWS(wsHandlers))

	// Pre-mint the session so chromedp can inject the cookie.
	sess, err := sessions.Create(user.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := sessions.SetActiveWorkspace(sess.ID, ws.ID); err != nil {
		t.Fatalf("set active workspace: %v", err)
	}
	h.SessionCookieValue = sess.ID

	// Build the httptest.Server manually so we can replace the listener
	// with a tracking wrapper. ConnContext lets the wsGate map a request
	// back to its underlying net.Conn for forced-close on DisableWS.
	srv := &http.Server{
		Handler:           wrapped,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connContextKey{}, c)
		},
	}
	ts := httptest.NewUnstartedServer(wrapped)
	ts.Config = srv
	srv.Handler = wrapped
	ts.Start()

	h.Server = ts
	h.BaseURL = ts.URL
	t.Cleanup(func() {
		_ = h.Close()
	})
	return h
}

// Close terminates the test server. Safe to call more than once.
func (h *Harness) Close() error {
	if h == nil || h.Server == nil {
		return nil
	}
	h.Server.Close()
	h.Server = nil
	return nil
}

// DisableWS flips the /ws kill-switch and force-closes every tracked /ws
// connection. New /ws requests will receive 503 until EnableWS runs. The
// browser-side `SatellitesWS` reacts with the same path it would take on
// a server crash: onclose fires, the state machine flips to reconnecting,
// and after MAX_CAP_RETRIES at the cap it lands on disconnected.
func (h *Harness) DisableWS() {
	h.wsEnabled.Store(false)
	h.tracker.closeAll()
}

// EnableWS opens the kill-switch so future /ws upgrades succeed again.
// Existing client retries (or a manual `retry()` click) will reconnect.
func (h *Harness) EnableWS() {
	h.wsEnabled.Store(true)
}

// PublishEvent fans an event onto the workspace's wshandler topic so
// debug-panel + bridge tests can assert the indicator's recent-events
// buffer fills and page-side handlers observe the wire payload.
//
// The signature accepts `any` for source compatibility with the four
// existing callsites — each already passes a map[string]any literal.
// The harness source's project_id-narrowed subscribers honour
// data["project_id"] (sty_fbcde932) so callers that carry it scope to
// project-bridged subscribers; others fan to every workspace subscriber.
func (h *Harness) PublishEvent(kind string, data any) {
	payload, _ := data.(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}
	projectID, _ := payload["project_id"].(string)
	h.Source.Publish(h.WorkspaceID, projectID, kind, payload)
}

// UpdateStoryStatus performs the in-process status mutation and fans
// out the wire-translated story.<target> event over the harness
// EventSource so the chromedp bridge sees the same payload shape
// internal/wshandler/translate.go::translateStory produces in
// production. The MemoryStore does not couple to surreallive, so the
// test goroutine must publish the wire event explicitly (sty_f52d540e,
// helper rewired onto Source in sty_fa0cc6f3).
func (h *Harness) UpdateStoryStatus(t *testing.T, storyID, target string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	memberships := []string{h.WorkspaceID}
	updated, err := h.Stories.UpdateStatus(ctx, storyID, target, h.UserID, now, memberships)
	if err != nil {
		t.Fatalf("UpdateStoryStatus(%s → %s): %v", storyID, target, err)
	}
	tags := updated.Tags
	if tags == nil {
		tags = []string{}
	}
	payload := map[string]any{
		"workspace_id": updated.WorkspaceID,
		"project_id":   updated.ProjectID,
		"story_id":     updated.ID,
		"title":        updated.Title,
		"status":       updated.Status,
		"priority":     updated.Priority,
		"category":     updated.Category,
		"tags":         tags,
		"updated_at":   updated.UpdatedAt.Format(time.RFC3339),
	}
	h.Source.Publish(updated.WorkspaceID, updated.ProjectID, "story."+target, payload)
}

// gateWS wraps the wshandler with the kill-switch + tracker. The handler
// is registered at GET /ws above; we always pass GETs through here.
func (h *Harness) gateWS(next *wshandler.Handler) http.Handler {
	// Build a sub-mux so the wshandler can register its own GET /ws route
	// against the inner mux, and the outer mux routes /ws through this
	// gate.
	inner := http.NewServeMux()
	next.Register(inner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.wsEnabled.Load() {
			http.Error(w, "ws disabled (test)", http.StatusServiceUnavailable)
			return
		}
		if c, ok := r.Context().Value(connContextKey{}).(net.Conn); ok {
			h.tracker.add(c)
		}
		inner.ServeHTTP(w, r)
	})
}

// connContextKey carries the underlying net.Conn from net/http's
// ConnContext hook to the wsGate handler.
type connContextKey struct{}

// connTracker records the net.Conns that handled /ws requests so the
// harness can close them on DisableWS without taking the whole server
// down.
type connTracker struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{conns: make(map[net.Conn]struct{})}
}

func (t *connTracker) add(c net.Conn) {
	t.mu.Lock()
	t.conns[c] = struct{}{}
	t.mu.Unlock()
}

func (t *connTracker) closeAll() {
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.conns = make(map[net.Conn]struct{})
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Logger returns the harness's arbor logger; helper for tests that want
// to scope additional log output.
func (h *Harness) Logger() arbor.ILogger { return satarbor.New("warn") }

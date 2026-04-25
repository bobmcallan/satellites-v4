//go:build portalui

// Package portalui hosts the chromedp-driven E2E suite for the portal's
// websocket connection indicator widget (story_0e5328cd, follow-up to the
// 10.4 widget shipped in story_ac3e4057).
//
// Tests in this package boot an in-process satellites server using the
// production constructors (auth.Handlers, portal.New, wshandler.New,
// hub.NewAuthHub) wired against the package-internal memory stores, then
// drive a headless Chromium via github.com/chromedp/chromedp to assert
// the widget's state transitions.
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
	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/hub"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/portal"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
	"github.com/bobmcallan/satellites/internal/wshandler"
)

// Harness owns the in-process satellites server plus the test-only knobs
// (DisableWS / EnableWS, PublishEvent) the chromedp tests use to simulate
// outage and inject observable hub events.
type Harness struct {
	Server      *httptest.Server
	BaseURL     string
	AuthHub     *hub.AuthHub
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
	Contracts *contract.MemoryStore
	Tasks     *task.MemoryStore
	Documents *document.MemoryStore
	Repos     *repo.MemoryStore
	Grants    *rolegrant.MemoryStore

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
	docStore := document.NewMemoryStore()
	contractStore := contract.NewMemoryStore(docStore, storyStore)
	taskStore := task.NewMemoryStore()
	repoStore := repo.NewMemoryStore()
	grantStore := rolegrant.NewMemoryStore(docStore)

	portalHandlers, err := portal.New(cfg, logger, sessions, users, projectStore, ledgerStore, storyStore, contractStore, taskStore, docStore, repoStore, codeindex.NewStub(), grantStore, wsStore, startedAt)
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

	sharedHub := hub.New()
	authHub := hub.NewAuthHub(sharedHub, wsStore, &noopMismatchAudit{})

	wsHandlers := wshandler.New(wshandler.Deps{
		AuthHub: authHub,
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

	h := &Harness{
		AuthHub:      authHub,
		AuthHandlers: authHandlers,
		UserID:       user.ID,
		WorkspaceID:  ws.ID,
		Projects:     projectStore,
		Stories:      storyStore,
		Ledger:       ledgerStore,
		Contracts:    contractStore,
		Tasks:        taskStore,
		Documents:    docStore,
		Repos:        repoStore,
		Grants:       grantStore,
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
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connContextKey{}, c)
		},
	}
	ts := httptest.NewUnstartedServer(mux)
	ts.Config = srv
	srv.Handler = mux
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

// PublishEvent fans an event onto the workspace's hub topic so debug-panel
// tests can assert the indicator's recent-events buffer fills.
func (h *Harness) PublishEvent(kind string, data any) {
	topic := "ws:" + h.WorkspaceID
	h.AuthHub.Publish(context.Background(), topic, hub.Event{
		Kind:        kind,
		WorkspaceID: h.WorkspaceID,
		Data:        data,
	})
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

// noopMismatchAudit satisfies hub.MismatchAudit for tests that don't care
// about workspace-mismatch evidence.
type noopMismatchAudit struct{}

func (noopMismatchAudit) HubMismatch(_ context.Context, _ hub.Event, _ string) {}

// Logger returns the harness's arbor logger; helper for tests that want
// to scope additional log output.
func (h *Harness) Logger() arbor.ILogger { return satarbor.New("warn") }

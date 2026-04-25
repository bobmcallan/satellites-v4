package portal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestPortal_ConnectionIndicator_Rendered covers AC1 + AC5: the widget
// markup is present on authed pages and the retry button is gated by
// the disconnected state.
func TestPortal_ConnectionIndicator_Rendered(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(alice)
	_, _ = ws.Create(testCtx(), alice.ID, "alpha", time.Now().UTC())

	mux := http.NewServeMux()
	p.Register(mux)

	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	// Widget container + Alpine binding.
	assert.Contains(t, body, `class="ws-indicator"`, "widget container present")
	assert.Contains(t, body, `x-data="wsIndicator()"`, "Alpine component bound")
	assert.Contains(t, body, `:class="'ws-indicator-' + status"`, "status class bound")
	// Retry button: gated by status==='disconnected'.
	assert.Contains(t, body, `x-show="status === 'disconnected'"`, "retry visibility bound")
	assert.Contains(t, body, `@click="retry()"`, "retry click handler bound")
	// Three status CSS classes referenced in stylesheet — widget depends on them.
	// We don't load portal.css here (go test doesn't serve static), but the
	// class names must appear in the rendered template for Alpine to bind.
	// The class binding emits e.g. ws-indicator-connecting at runtime.
}

// TestPortal_ConnectionIndicator_Bootstrap covers AC1 config payload.
func TestPortal_ConnectionIndicator_Bootstrap(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(alice)
	wspace, _ := ws.Create(testCtx(), alice.ID, "alpha", time.Now().UTC())

	mux := http.NewServeMux()
	p.Register(mux)

	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, "window.SATELLITES_WS", "bootstrap script emitted")
	// Workspace id appears in the bootstrap JSON literal.
	assert.Contains(t, body, wspace.ID, "workspace id embedded in bootstrap")
	// ws.js loaded.
	assert.Contains(t, body, `/static/ws.js?v=`, "ws.js script tag present")
}

// TestPortal_ConnectionIndicator_DebugFlag covers AC6: ?debug=true
// flips the bootstrap payload to debug:true.
func TestPortal_ConnectionIndicator_DebugFlag(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(alice)
	_, _ = ws.Create(testCtx(), alice.ID, "alpha", time.Now().UTC())

	mux := http.NewServeMux()
	p.Register(mux)

	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)

	// No debug param → debug:false. html/template renders bool with a
	// leading space in JS context, so match the payload's substring.
	reqOff := httptest.NewRequest(http.MethodGet, "/", nil)
	reqOff.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	recOff := httptest.NewRecorder()
	mux.ServeHTTP(recOff, reqOff)
	bodyOff := recOff.Body.String()
	assert.Regexp(t, `debug:\s*false`, bodyOff, "debug bootstrap off by default")
	assert.NotRegexp(t, `debug:\s*true`, bodyOff, "no debug:true leak without the param")

	// With debug=true → debug:true.
	reqOn := httptest.NewRequest(http.MethodGet, "/?debug=true", nil)
	reqOn.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	recOn := httptest.NewRecorder()
	mux.ServeHTTP(recOn, reqOn)
	bodyOn := recOn.Body.String()
	assert.Regexp(t, `debug:\s*true`, bodyOn, "debug bootstrap on with the query param")
}

// TestPortal_WSJSAsset covers AC2+AC3+AC4+AC7: the static JS asset is
// served and contains the key state-machine symbols. Lightweight
// substitute for a browser-runtime test (see plan.md deferral note).
func TestPortal_WSJSAsset(t *testing.T) {
	t.Parallel()

	// Read the shipped file directly (the portal static handler serves
	// it from the embed.FS; here we assert the source artefact carries
	// the contract the state-table review criteria demand).
	src, err := os.ReadFile("../../pages/static/ws.js")
	require.NoError(t, err)
	body := string(src)

	// AC4 — all five state names present.
	for _, state := range []string{"idle", "connecting", "live", "reconnecting", "disconnected"} {
		assert.Contains(t, body, `'`+state+`'`, "state constant %q present", state)
	}
	// AC2 — backoff constants. The production values appear after the
	// `__SATELLITES_WS_FAST` test gate (story_0e5328cd) — the regex pins
	// the production fallback value, ignoring the test-mode override.
	assert.Regexp(t, regexp.MustCompile(`BACKOFF_BASE_MS\s*=\s*(?:__FAST\s*\?\s*\d+\s*:\s*)?1000\b`), body,
		"base backoff constant (production value)")
	assert.Regexp(t, regexp.MustCompile(`BACKOFF_MAX_MS\s*=\s*(?:__FAST\s*\?\s*\d+\s*:\s*)?30000\b`), body,
		"max backoff cap (production value)")
	// AC3 — since_id wired into reconnect subscribe payload.
	assert.Regexp(t, regexp.MustCompile(`since_id\s*=\s*this\.lastEventID`), body,
		"since_id set from lastEventID on reconnect")
	// AC7 — zero-flicker guard constant (production fallback after the
	// __SATELLITES_WS_FAST gate).
	assert.Regexp(t, regexp.MustCompile(`ZERO_FLICKER_MS\s*=\s*(?:__FAST\s*\?\s*\d+\s*:\s*)?500\b`), body,
		"zero-flicker threshold constant (production value)")
	// transition dispatcher exists.
	assert.Contains(t, body, "transition(next)", "central transition dispatcher")
	// Alpine component factory exposed.
	assert.Contains(t, body, "window.wsIndicator", "Alpine factory globally exposed")
	// Class exposed for external use.
	assert.Contains(t, body, "window.SatellitesWS", "SatellitesWS globally exposed")
}

// TestPortal_Landing_NoWidget confirms the widget is absent on the
// unauthenticated landing page (no workspace context).
func TestPortal_Landing_NoWidget(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newPortalWithWorkspace(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.NotContains(t, body, "window.SATELLITES_WS", "no bootstrap on landing page")
	assert.NotContains(t, body, `class="ws-indicator"`, "no widget on landing page")
}

// Sanity: ensure the embed.FS exposes ws.js — the static handler mount
// uses `/static/` with `http.FileServer(http.FS(static))` where static
// is derived from pages.Static().
func TestPortal_WSJSAsset_Embedded(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newPortalWithWorkspace(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/static/ws.js", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "static/ws.js must be served")
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(body), "class SatellitesWS"),
		"static handler serves the real ws.js contents")
}

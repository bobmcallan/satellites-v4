package portal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestNav_HasFlexContainer asserts the nav children sit inside a
// `.nav-inner` wrapper so the existing flex CSS (display: flex,
// align-items: center) renders the bar as a single horizontal row.
// Regression guard for story_31d43312 — story_e7e8b455 dropped the
// wrapper and the dashboard nav stacked vertically.
func TestNav_HasFlexContainer(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	headerOpen := strings.Index(body, `<header class="portal-nav nav"`)
	innerOpen := strings.Index(body, `<div class="nav-inner">`)
	brand := strings.Index(body, `class="nav-brand"`)
	if headerOpen < 0 {
		t.Fatalf("header missing")
	}
	if innerOpen < 0 {
		t.Fatalf(`<div class="nav-inner"> wrapper missing — nav will stack vertically without flex container`)
	}
	if !(headerOpen < innerOpen && innerOpen < brand) {
		t.Errorf("nav-inner must sit between <header> and the first nav child; got header=%d inner=%d brand=%d", headerOpen, innerOpen, brand)
	}
}

// TestNav_DOMOrder asserts the v3 nav layout: brand → optional DEV chip
// → optional active-WS chip → primary links → spacer → indicators →
// version chip → hamburger. Renders an authed page for a user with one
// workspace and DevMode on, then asserts substring-position ordering.
func TestNav_DOMOrder(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	order := []string{
		`class="nav-brand"`,
		`data-testid="dev-chip"`,
		`<nav class="nav-links">`,
		`href="/projects"`,
		`href="/tasks"`,
		`class="nav-spacer"`,
		`data-testid="nav-hamburger"`,
	}
	prev := -1
	for _, marker := range order {
		idx := strings.Index(body, marker)
		if idx < 0 {
			t.Errorf("nav body missing %q\nbody=%s", marker, body)
			continue
		}
		if idx < prev {
			t.Errorf("nav DOM order violation: %q (offset %d) appears before previous marker (offset %d)", marker, idx, prev)
		}
		prev = idx
	}
}

// TestNav_HamburgerDropdown asserts the dropdown wrapper exists and
// contains the logout form + theme picker form.
func TestNav_HamburgerDropdown(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	dropStart := strings.Index(body, `data-testid="nav-dropdown"`)
	if dropStart < 0 {
		t.Fatalf("nav-dropdown missing")
	}
	// The dropdown wrapper has a closing </div> but tracking nesting is
	// tricky in raw text; assert substring positions instead.
	for _, want := range []string{
		`form action="/auth/logout"`,
		`form class="theme-picker"`,
		`alice@local`,
	} {
		idx := strings.Index(body, want)
		if idx < dropStart {
			t.Errorf("expected %q to appear after dropdown opener (got idx=%d, drop=%d)", want, idx, dropStart)
		}
	}
}

// TestNav_NoUnimplementedRoutes verifies the rendered HTML does NOT link
// to any route that doesn't have a handler in this build (Skills,
// MCP-info, Help, Settings, Profile, Changelog, Feedback, admin).
func TestNav_NoUnimplementedRoutes(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, banned := range []string{
		`href="/skills"`,
		`href="/mcp-info"`,
		`href="/help"`,
		`href="/settings"`,
		`href="/profile"`,
		`href="/changelog"`,
		`href="/feedback"`,
		`href="/admin/`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("nav must NOT link to unimplemented route: %q", banned)
		}
	}
}

// TestNav_NoActiveWSChip asserts the duplicated active-workspace chip
// next to the brand is no longer rendered. story_4d1ef14f.
func TestNav_NoActiveWSChip(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, `data-testid="active-ws-chip"`) {
		t.Errorf("nav still renders the duplicated active-ws-chip — should be removed (story_4d1ef14f)")
	}
}

// TestNav_WorkspaceNameSingle asserts the active workspace name appears
// exactly once in the rendered nav. The switcher button is the single
// source of truth; the previous duplicate chip has been removed.
// story_4d1ef14f.
func TestNav_WorkspaceNameSingle(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	const wsName = "TestaroniWorkspaceXYZ"
	if _, err := ws.Create(testCtx(), user.ID, wsName, time.Now().UTC()); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	headerStart := strings.Index(body, `<header class="portal-nav nav"`)
	headerEnd := strings.Index(body, `</header>`)
	if headerStart < 0 || headerEnd < 0 {
		t.Fatalf("nav header bounds missing")
	}
	navOnly := body[headerStart:headerEnd]
	got := strings.Count(navOnly, wsName)
	// Switcher button + one menu entry per workspace; with one workspace
	// the menu lists it as a single <a> as well. Two occurrences are
	// expected (button label + menu list item). The bug was THREE (chip +
	// button + menu).
	if got != 2 {
		t.Errorf("active workspace name occurrences in nav = %d, want 2 (switcher button + menu item)", got)
	}
}

// TestNav_WorkspaceMenuAbsolute asserts the .nav-workspace-menu CSS rule
// is `position: absolute` so opening the dropdown cannot push the nav
// row taller. Reads the shipped portal.css directly. story_4d1ef14f.
func TestNav_WorkspaceMenuAbsolute(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("../../pages/static/css/portal.css")
	if err != nil {
		t.Fatalf("read portal.css: %v", err)
	}
	css := string(src)
	idx := strings.Index(css, ".nav-workspace-menu {")
	if idx < 0 {
		t.Fatalf(".nav-workspace-menu rule missing from portal.css")
	}
	end := strings.Index(css[idx:], "}")
	if end < 0 {
		t.Fatalf(".nav-workspace-menu rule has no closing brace")
	}
	rule := css[idx : idx+end]
	if !strings.Contains(rule, "position: absolute") {
		t.Errorf(".nav-workspace-menu rule must contain `position: absolute` so opening the menu does not reflow the nav row; got:\n%s", rule)
	}
}

// TestNav_NoVersionChip asserts the legacy `class="version-chip"` span
// is no longer rendered in the nav. story_1340913b moved version + commit
// metadata into the footer partial.
func TestNav_NoVersionChip(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, `class="version-chip"`) {
		t.Errorf("rendered nav still contains class=\"version-chip\" — should be removed (story_1340913b)")
	}
}

// TestNav_LogoutNotInline asserts the logout form is inside the
// hamburger dropdown wrapper, not at the top level of the nav header.
func TestNav_LogoutInDropdownNotInline(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, _, _, _ := newTestPortal(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	logoutForm := strings.Index(body, `form action="/auth/logout"`)
	dropdown := strings.Index(body, `data-testid="nav-dropdown"`)
	if logoutForm < 0 {
		t.Fatalf("logout form missing")
	}
	if dropdown < 0 {
		t.Fatalf("dropdown missing")
	}
	if logoutForm < dropdown {
		t.Errorf("logout form (offset %d) appears BEFORE dropdown opener (offset %d) — should be inside the dropdown", logoutForm, dropdown)
	}
	// Old inline class should NOT appear.
	if strings.Contains(body, `class="nav-logout"`) {
		t.Errorf("legacy inline nav-logout class still present — should be removed")
	}
}

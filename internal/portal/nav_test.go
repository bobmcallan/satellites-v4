package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		`class="version-chip"`,
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

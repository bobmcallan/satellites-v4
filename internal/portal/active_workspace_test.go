package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
)

// Sty_a21f61a6 — these tests pin the activeWorkspace contract introduced
// by the workspace-fallback bug fix. The previous fallback returned
// chips[0] as the visible "active" while leaving scope wide; the nav
// template then hid the fake active from the dropdown.

func TestDisambiguateChipNames_CaseCollisionAppendsID(t *testing.T) {
	t.Parallel()
	in := []wsChip{
		{ID: "wksp_aaaa1111", Name: "personal"},
		{ID: "wksp_bbbb2222", Name: "Personal"},
		{ID: "wksp_cccc3333", Name: "other"},
	}
	out := disambiguateChipNames(in)
	if got := out[0].Name; got != "personal (wksp_aaaa1111)" {
		t.Errorf("chip[0].Name = %q; want disambiguated form with id suffix", got)
	}
	if got := out[1].Name; got != "Personal (wksp_bbbb2222)" {
		t.Errorf("chip[1].Name = %q; want disambiguated form with id suffix", got)
	}
	if got := out[2].Name; got != "other" {
		t.Errorf("chip[2].Name = %q; want unchanged (no collision)", got)
	}
}

func TestDisambiguateChipNames_NoCollisionPassthrough(t *testing.T) {
	t.Parallel()
	in := []wsChip{
		{ID: "wksp_a", Name: "alpha"},
		{ID: "wksp_b", Name: "beta"},
	}
	out := disambiguateChipNames(in)
	if out[0].Name != "alpha" || out[1].Name != "beta" {
		t.Errorf("expected names unchanged; got %+v", out)
	}
}

func TestDisambiguateChipNames_SingleChipPassthrough(t *testing.T) {
	t.Parallel()
	in := []wsChip{{ID: "wksp_x", Name: "only"}}
	out := disambiguateChipNames(in)
	if len(out) != 1 || out[0].Name != "only" {
		t.Errorf("expected single chip unchanged; got %+v", out)
	}
}

func TestDisambiguateChipNames_TripleCollisionAllSuffixed(t *testing.T) {
	t.Parallel()
	in := []wsChip{
		{ID: "wksp_1", Name: "main"},
		{ID: "wksp_2", Name: "MAIN"},
		{ID: "wksp_3", Name: "Main"},
	}
	out := disambiguateChipNames(in)
	for i, c := range out {
		if !strings.HasSuffix(c.Name, "("+in[i].ID+")") {
			t.Errorf("chip[%d].Name = %q; want suffix `(%s)`", i, c.Name, in[i].ID)
		}
	}
}

// TestNav_NoStickyMultiMembership_HeaderEmDashAndAllChipsInDropdown
// covers sty_a21f61a6 AC1 + AC2 end-to-end: a user with two workspaces
// and no sticky session sees the em-dash placeholder in the header AND
// both workspaces appear in the dropdown.
func TestNav_NoStickyMultiMembership_HeaderEmDashAndAllChipsInDropdown(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_multi", Email: "multi@local"}
	users.Add(user)
	if _, err := ws.Create(testCtx(), user.ID, "alpha-ws", time.Now().UTC()); err != nil {
		t.Fatalf("create alpha-ws: %v", err)
	}
	if _, err := ws.Create(testCtx(), user.ID, "beta-ws", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create beta-ws: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	// Note: no SetActiveWorkspace — exercises the no-sticky path.

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
	nav := body[headerStart:headerEnd]

	// AC1: header label is the em-dash placeholder, not chips[0].Name.
	if !strings.Contains(nav, "WORKSPACE / —") {
		t.Errorf("nav header must read `WORKSPACE / —` under no-sticky+multi-membership; nav=%s", nav)
	}
	for _, chip := range []string{"alpha-ws", "beta-ws"} {
		if strings.Contains(nav, "WORKSPACE / "+chip) {
			t.Errorf("nav header must NOT use %q as a stand-in for the active workspace; nav=%s", chip, nav)
		}
	}

	// AC2: every membership chip appears in the dropdown.
	menuStart := strings.Index(nav, `data-testid="nav-workspace-menu"`)
	if menuStart < 0 {
		t.Fatalf("workspace dropdown menu missing")
	}
	menu := nav[menuStart:]
	for _, want := range []string{"alpha-ws", "beta-ws"} {
		if !strings.Contains(menu, want) {
			t.Errorf("dropdown must contain %q chip under no-sticky path; menu=%s", want, menu)
		}
	}
	if strings.Contains(menu, `data-testid="nav-workspace-empty"`) {
		t.Errorf("no-sticky multi-membership dropdown must NOT render the empty-state placeholder; menu=%s", menu)
	}
}

// TestNav_NoStickySingleMembership_HeaderShowsThatWorkspace covers AC1's
// single-membership branch: the only workspace IS unambiguously active
// even without a sticky session selection, so the header renders that
// name (not the em-dash placeholder).
func TestNav_NoStickySingleMembership_HeaderShowsThatWorkspace(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_solo", Email: "solo@local"}
	users.Add(user)
	if _, err := ws.Create(testCtx(), user.ID, "only-ws", time.Now().UTC()); err != nil {
		t.Fatalf("create only-ws: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	// No SetActiveWorkspace.

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "WORKSPACE / only-ws") {
		t.Errorf("single-membership nav must render the workspace as active even without sticky session; body=%s", body)
	}
}

// TestNav_StaleStickyMultiMembership_FallsBackToEmDash covers the stale-
// sticky branch: a saved ActiveWorkspaceID that no longer matches any
// current membership must fall back to the multi-membership empty-active
// presentation rather than silently masquerading as one of the chips.
func TestNav_StaleStickyMultiMembership_FallsBackToEmDash(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_stale", Email: "stale@local"}
	users.Add(user)
	if _, err := ws.Create(testCtx(), user.ID, "alpha-ws", time.Now().UTC()); err != nil {
		t.Fatalf("create alpha-ws: %v", err)
	}
	if _, err := ws.Create(testCtx(), user.ID, "beta-ws", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create beta-ws: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	if err := sessions.SetActiveWorkspace(sess.ID, "wksp_doesnotexist"); err != nil {
		t.Fatalf("set stale active workspace: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "WORKSPACE / —") {
		t.Errorf("stale-sticky multi-membership must render em-dash placeholder; body=%s", body)
	}
}

// TestProjectsList_NoStickyMultiMembership_SpansAllWorkspaces covers
// AC3's data-path side: a multi-membership user with no sticky still
// sees projects from every workspace they belong to (the presentation
// fix is empty-active header, not narrowed data).
func TestProjectsList_NoStickyMultiMembership_SpansAllWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := &config.Config{Env: "dev"}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	wsA, err := ws.Create(testCtx(), user.ID, "alpha-ws", time.Now().UTC())
	if err != nil {
		t.Fatalf("create alpha-ws: %v", err)
	}
	wsB, err := ws.Create(testCtx(), user.ID, "beta-ws", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("create beta-ws: %v", err)
	}
	if _, err := p.projects.Create(ctx, user.ID, wsA.ID, "alpha-project", time.Now().UTC()); err != nil {
		t.Fatalf("create alpha-project: %v", err)
	}
	if _, err := p.projects.Create(ctx, user.ID, wsB.ID, "beta-project", time.Now().UTC()); err != nil {
		t.Fatalf("create beta-project: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	// No sticky.

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"alpha-project", "beta-project"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects list missing %q under no-sticky multi-membership path", want)
		}
	}
}

// TestProjectsList_StickyMultiMembership_NarrowsToActiveWorkspace covers
// AC3's narrowed branch: when the user has set a sticky workspace, the
// data path only returns projects from that workspace.
func TestProjectsList_StickyMultiMembership_NarrowsToActiveWorkspace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := &config.Config{Env: "dev"}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	wsA, _ := ws.Create(testCtx(), user.ID, "alpha-ws", time.Now().UTC())
	wsB, _ := ws.Create(testCtx(), user.ID, "beta-ws", time.Now().UTC().Add(time.Hour))
	_, _ = p.projects.Create(ctx, user.ID, wsA.ID, "alpha-project", time.Now().UTC())
	_, _ = p.projects.Create(ctx, user.ID, wsB.ID, "beta-project", time.Now().UTC())

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	if err := sessions.SetActiveWorkspace(sess.ID, wsA.ID); err != nil {
		t.Fatalf("set sticky to alpha: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "alpha-project") {
		t.Errorf("projects list must include alpha-project under sticky=alpha; body=%s", body)
	}
	if strings.Contains(body, "beta-project") {
		t.Errorf("projects list must NOT include beta-project when sticky scope = alpha; body=%s", body)
	}
}

// TestNav_CaseCollisionDisambiguates covers AC4 at the render surface:
// two workspaces whose trimmed display names differ only in case render
// with the `(wksp_<id>)` suffix appended.
func TestNav_CaseCollisionDisambiguates(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, ws := newPortalWithWorkspace(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_dup", Email: "dup@local"}
	users.Add(user)
	wsLower, err := ws.Create(testCtx(), user.ID, "personal", time.Now().UTC())
	if err != nil {
		t.Fatalf("create lowercase: %v", err)
	}
	wsUpper, err := ws.Create(testCtx(), user.ID, "Personal", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("create uppercase: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	wantLower := "personal (" + wsLower.ID + ")"
	wantUpper := "Personal (" + wsUpper.ID + ")"
	if !strings.Contains(body, wantLower) {
		t.Errorf("nav must disambiguate the lowercase chip as %q; body=%s", wantLower, body)
	}
	if !strings.Contains(body, wantUpper) {
		t.Errorf("nav must disambiguate the uppercase chip as %q; body=%s", wantUpper, body)
	}
}

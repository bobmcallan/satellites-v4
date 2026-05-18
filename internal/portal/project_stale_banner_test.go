// sty_7bc94f80 — render-side test for the project-page stale-data
// banner. The banner mounts inside <main> on /projects/{id} only;
// /projects/{id}/ledger and /projects/{id}/stories/{story_id} must NOT
// carry it (the rubric calls for "exactly one match" of
// `class="project-stale-banner"` across pages/templates/*.html).
package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/story"
)

func TestProjectDetail_StaleBannerRendered(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, projects, _, stories, _, workspaces := newTestPortalWithContracts(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	now := time.Now().UTC()
	var wsID string
	if workspaces != nil {
		ws, _ := workspaces.Create(t.Context(), user.ID, "alpha", now)
		_ = workspaces.AddMember(t.Context(), ws.ID, user.ID, "admin", user.ID, now)
		wsID = ws.ID
	}
	proj, _ := projects.Create(t.Context(), user.ID, wsID, "alpha-1", now)
	seedStory, err := stories.Create(context.Background(), story.Story{
		ProjectID:          proj.ID,
		WorkspaceID:        wsID,
		Title:              "fixture",
		Description:        "stale-banner fixture",
		AcceptanceCriteria: "rendered",
		Status:             "backlog",
		Priority:           "medium",
		Category:           "feature",
		CreatedBy:          user.ID,
	}, now)
	if err != nil {
		t.Fatalf("seed story: %v", err)
	}
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	// /projects/{id} — banner MUST be present with the AC-mandated markers.
	body := fetchBody(t, mux, "/projects/"+proj.ID, sess.ID)
	mustContain(t, body, `class="project-stale-banner is-hidden"`,
		"banner must ship with is-hidden hardcoded to avoid first-paint flash (plan §6)")
	mustContain(t, body, `x-data="projectStaleBanner"`,
		"banner must mount the projectStaleBanner Alpine factory")
	mustContain(t, body, `data-testid="project-stale-banner"`,
		"banner must carry the data-testid the e2e suite asserts on")
	mustContain(t, body, `role="status"`,
		"banner must expose role=status for screen readers")
	mustContain(t, body, `aria-live="polite"`,
		"banner must expose aria-live=polite so live-region announcements fire")
	mustContain(t, body, `data-testid="project-stale-banner-refresh"`,
		"refresh button must carry the data-testid the e2e suite asserts on")
	mustContain(t, body, `:class="bannerClasses"`,
		"banner must bind :class to the Alpine state derivation")
	mustContain(t, body, `:class="hiddenWhenNotDisconnected"`,
		"refresh button must hide outside the disconnected state")

	// Banner must sit INSIDE <main> and BEFORE the {{range .Panels}} loop —
	// i.e. before the first .panel-headed section.
	mainIdx := strings.Index(body, `<main class="portal-main"`)
	bannerIdx := strings.Index(body, `data-testid="project-stale-banner"`)
	firstPanelIdx := strings.Index(body, `class="panel-headed"`)
	if mainIdx < 0 || bannerIdx < 0 || firstPanelIdx < 0 {
		t.Fatalf("missing required anchors: main=%d banner=%d firstPanel=%d", mainIdx, bannerIdx, firstPanelIdx)
	}
	if bannerIdx < mainIdx {
		t.Errorf("banner rendered before <main>; expected inside main element")
	}
	if bannerIdx > firstPanelIdx {
		t.Errorf("banner rendered after first panel; expected before {{range .Panels}}")
	}

	// /projects/{id}/ledger — banner MUST NOT appear (project-page-only).
	ledgerBody := fetchBody(t, mux, "/projects/"+proj.ID+"/ledger", sess.ID)
	if strings.Contains(ledgerBody, `class="project-stale-banner`) {
		t.Errorf("/projects/{id}/ledger must NOT render the project-stale-banner; it has its own wsStatus dot")
	}

	// /projects/{id}/stories/{story_id} — banner MUST NOT appear.
	storyBody := fetchBody(t, mux, "/projects/"+proj.ID+"/stories/"+seedStory.ID, sess.ID)
	if strings.Contains(storyBody, `class="project-stale-banner`) {
		t.Errorf("/projects/{id}/stories/{story_id} must NOT render the project-stale-banner")
	}

	// Exactly one template file emits the banner class — the project page.
	matches := grepTemplatesForBanner(t)
	if len(matches) != 1 {
		t.Errorf("expected exactly one pages/templates/*.html to render class=\"project-stale-banner\"; got %v", matches)
	} else if !strings.HasSuffix(matches[0], "project_detail.html") {
		t.Errorf("banner rendered by unexpected template %q; expected project_detail.html", matches[0])
	}

	// CSS gate: the .is-hidden rule must zero the display so the
	// is-hidden-on-server class genuinely hides the element.
	css := readPortalCSS(t)
	if !strings.Contains(css, `.project-stale-banner.is-hidden { display: none; }`) {
		t.Errorf("portal.css missing `.project-stale-banner.is-hidden { display: none; }` rule")
	}
	if !strings.Contains(css, `.project-stale-banner-reconnecting`) {
		t.Errorf("portal.css missing .project-stale-banner-reconnecting state-coloured variant")
	}
	if !strings.Contains(css, `.project-stale-banner-disconnected`) {
		t.Errorf("portal.css missing .project-stale-banner-disconnected state-coloured variant")
	}
	if !strings.Contains(css, `.project-stale-banner-refresh`) {
		t.Errorf("portal.css missing .project-stale-banner-refresh rule")
	}

	// JS gate: the projectStaleBanner factory must carry the literal copy
	// strings from plan §2.4 (U+2026 ellipsis, not ASCII "...").
	jsSource := readCommonJS(t)
	if !strings.Contains(jsSource, `Live updates paused — reconnecting…`) {
		t.Errorf("common.js missing exact reconnecting copy `Live updates paused — reconnecting…` (note U+2026 ellipsis)")
	}
	if !strings.Contains(jsSource, `Live updates unavailable — refresh to reload.`) {
		t.Errorf("common.js missing exact disconnected copy `Live updates unavailable — refresh to reload.`")
	}
	if !strings.Contains(jsSource, `Alpine.data('projectStaleBanner', projectStaleBanner)`) {
		t.Errorf("common.js missing Alpine.data registration for projectStaleBanner")
	}
	if !strings.Contains(jsSource, `addEventListener('satellites:realtime:status'`) {
		t.Errorf("common.js missing satellites:realtime:status listener; banner cannot react to status flips")
	}
	if strings.Contains(jsSource, `new SatellitesWS(`) || strings.Contains(jsSource, `new window.SatellitesWS(`) {
		t.Errorf("common.js must NOT construct SatellitesWS — the banner consumes the bridge's CustomEvent (rubric item 20)")
	}

	// Bridge gate: realtime_bridge.js must wire the onStatusChange callback
	// that fires the CustomEvent.
	bridge := readRealtimeBridgeJS(t)
	if !strings.Contains(bridge, `onStatusChange`) {
		t.Errorf("realtime_bridge.js missing onStatusChange callback")
	}
	if !strings.Contains(bridge, `satellites:realtime:status`) {
		t.Errorf("realtime_bridge.js missing satellites:realtime:status CustomEvent dispatch")
	}
}

// fetchBody hits the mux as the authenticated session user and returns
// the response body string. Fatals on non-200.
func fetchBody(t *testing.T, mux *http.ServeMux, path, sessionID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func mustContain(t *testing.T, body, needle, reason string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Errorf("body missing %q — %s", needle, reason)
	}
}

func readPortalCSS(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	path := filepath.Join(root, "pages", "static", "css", "portal.css")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read portal.css (%s): %v", path, err)
	}
	return string(raw)
}

func grepTemplatesForBanner(t *testing.T) []string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	tmplDir := filepath.Join(root, "pages", "templates")
	entries, err := os.ReadDir(tmplDir)
	if err != nil {
		t.Fatalf("read templates dir (%s): %v", tmplDir, err)
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(tmplDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(body), `class="project-stale-banner`) {
			matches = append(matches, filepath.Join("pages", "templates", e.Name()))
		}
	}
	return matches
}

//go:build portalui

package portalui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// TestPortal_AuthedWalk drives a real user's authed flow end-to-end so
// regressions like the empty workspace switcher (story_690b8f5c) and
// the empty /projects panel (story_0f415ab3) fail loudly. Walk:
// dev-signin → dashboard → /projects → /tasks → hamburger dropdown →
// open workspace switcher.
//
// Each assertion looks for both presence of the expected element AND
// the absence of the regression-adjacent failure mode (empty <ul> with
// no children, empty-state copy where rows should appear, etc.).
func TestPortal_AuthedWalk(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	// dev-signin starts at /. Clear cookies so we drive the real signin
	// path rather than the pre-baked harness cookie.
	if err := chromedp.Run(browserCtx, network.ClearBrowserCookies()); err != nil {
		t.Fatalf("clear cookies: %v", err)
	}

	// Step 1 — dev-signin click → dashboard.
	var dashBody string
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`form[data-testid="dev-signin"] button`, chromedp.ByQuery),
		chromedp.Click(`form[data-testid="dev-signin"] button`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.WaitVisible(`footer.footer`, chromedp.ByQuery),
		chromedp.Text("body", &dashBody, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("dev signin: %v", err)
	}
	if !strings.Contains(dashBody, "Signed in as") {
		t.Errorf("dashboard missing 'Signed in as' marker; bodyText=%s", dashBody)
	}
	if !strings.Contains(dashBody, "DEV") {
		t.Errorf("DEV chip missing on dashboard")
	}

	// Step 2 — open workspace switcher; menu must have ≥1 child <li>
	// (covers story_690b8f5c regression).
	var menuChildren int
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`.nav-workspace .btn-link`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(
			`document.querySelectorAll('[data-testid="nav-workspace-menu"] > li').length`,
			&menuChildren),
	); err != nil {
		t.Fatalf("workspace switcher open: %v", err)
	}
	if menuChildren < 1 {
		t.Errorf("workspace switcher menu has %d children; expected ≥1 (placeholder OR switch target) — story_690b8f5c regression guard", menuChildren)
	}

	// Step 3 — click PROJECTS link; assert ≥1 row in the data table
	// (covers story_0f415ab3 regression).
	var projectRows int
	var projectsBody string
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`a[href="/projects"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`section.panel-headed`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('table.data-table tbody tr').length`, &projectRows),
		chromedp.Text(`section.panel-headed`, &projectsBody, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("/projects navigate: %v", err)
	}
	if projectRows < 1 {
		t.Errorf("/projects rendered %d project rows; expected ≥1 (per-user default seed) — story_0f415ab3 regression guard\nbody=%s", projectRows, projectsBody)
	}
	if strings.Contains(projectsBody, "You don't own any projects yet") {
		t.Errorf("/projects shows empty-state copy after dev signin — bodyText=%s", projectsBody)
	}

	// Step 4 — click TASKS link; the panel must render. Empty-state copy
	// is acceptable here because no tasks are seeded; we just want a
	// rendered shell.
	var tasksHTML string
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`a[href="/tasks"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`section.panel-headed`, chromedp.ByQuery),
		chromedp.OuterHTML(`section.panel-headed`, &tasksHTML, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("/tasks navigate: %v", err)
	}
	if !strings.Contains(tasksHTML, "panel-headed") {
		t.Errorf("/tasks panel missing — html=%s", tasksHTML)
	}

	// Step 5 — open hamburger dropdown; theme picker must be present.
	var dropdownVisible bool
	var themePickerVisible bool
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(
			`(() => { const el = document.querySelector('[data-testid="nav-dropdown"]'); return !!el && el.offsetParent !== null; })()`,
			&dropdownVisible),
		chromedp.Evaluate(
			`(() => { const el = document.querySelector('[data-testid="nav-dropdown"] form.theme-picker'); return !!el && el.offsetParent !== null; })()`,
			&themePickerVisible),
	); err != nil {
		t.Fatalf("hamburger open: %v", err)
	}
	if !dropdownVisible {
		t.Errorf("hamburger dropdown did not open")
	}
	if !themePickerVisible {
		t.Errorf("theme picker missing inside hamburger dropdown")
	}
}

//go:build portalui

package portalui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestSettings_ViaNav covers story_ccee859d: opening the hamburger
// dropdown and clicking the settings link lands on /settings, the theme
// picker is rendered there, and submitting a theme button keeps the user
// on /settings (the picker's `next` defaults to /settings when rendered
// from the settings page).
func TestSettings_ViaNav(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install session cookie: %v", err)
	}

	// Step 1 — visit dashboard.
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate /: %v", err)
	}

	// Step 2 — open hamburger, click settings link.
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Click(`[data-testid="nav-settings-link"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`section[data-testid="settings-panel"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("nav -> settings: %v", err)
	}

	// Step 3 — assert URL and that the theme picker is rendered on the page.
	var path string
	var pickerCount int
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`window.location.pathname`, &path),
		chromedp.Evaluate(`document.querySelectorAll('section[data-testid="settings-panel"] form.theme-picker').length`, &pickerCount),
	); err != nil {
		t.Fatalf("settings render assertions: %v", err)
	}
	if path != "/settings" {
		t.Errorf("expected to be on /settings, got %q", path)
	}
	if pickerCount != 1 {
		t.Errorf("expected one theme-picker form on /settings, got %d", pickerCount)
	}

	// Step 4 — pick a theme; verify we stay on /settings.
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`form.theme-picker button[value="dark"]`, chromedp.ByQuery),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.WaitVisible(`section[data-testid="settings-panel"]`, chromedp.ByQuery),
		chromedp.Evaluate(`window.location.pathname`, &path),
	); err != nil {
		t.Fatalf("theme submit: %v", err)
	}
	if path != "/settings" {
		t.Errorf("after theme submit expected /settings, got %q", path)
	}
}

// TestSettings_RequiresAuth covers AC3: GET /settings without a session
// redirects to /login (matching the rest of the auth-gated portal pages).
func TestSettings_RequiresAuth(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	// No session cookie installed.
	var path string
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/settings"),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`window.location.pathname`, &path),
	); err != nil {
		t.Fatalf("navigate /settings unauth: %v", err)
	}
	// Auth-gated: handler does redirectToLogin → /login → handleLogin
	// further redirects to /. Land on either /login or / (landing page).
	if !strings.HasPrefix(path, "/login") && path != "/" {
		t.Errorf("unauthenticated /settings should redirect to /login or /, got %q", path)
	}
}

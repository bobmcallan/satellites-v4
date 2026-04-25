//go:build portalui

package portalui

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// TestLanding_RendersV3Layout (story_92210e4a AC1) — visiting / unauth
// renders the SATELLITES wordmark, "Agentic software engineering via
// MCP." subhead, and 01/02/03 panels.
func TestLanding_RendersV3Layout(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	var bodyText string
	if err := chromedp.Run(browserCtx,
		network.ClearBrowserCookies(),
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`.landing-wordmark`, chromedp.ByQuery),
		chromedp.Text("body", &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate /: %v", err)
	}

	for _, want := range []string{
		"SATELLITES",
		"Agentic software engineering via MCP.",
		"[01] CONFIGURE",
		"[02] MCP SERVER",
		"[03] EXECUTE",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("landing body missing %q\nbody=%s", want, bodyText)
		}
	}
}

// TestLanding_DefaultDarkTheme (AC2) — first paint with no cookies sets
// data-theme="dark" via the inline first-paint script.
func TestLanding_DefaultDarkTheme(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	var theme string
	if err := chromedp.Run(browserCtx,
		network.ClearBrowserCookies(),
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`.landing-wordmark`, chromedp.ByQuery),
		chromedp.AttributeValue("html", "data-theme", &theme, nil),
	); err != nil {
		t.Fatalf("navigate /: %v", err)
	}
	if theme != "dark" {
		t.Errorf("data-theme = %q, want \"dark\"", theme)
	}
}

// TestLanding_PortalMainCentered (AC6) — the landing main element is
// horizontally centered.
func TestLanding_PortalMainCentered(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	var marginsEqual bool
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`main.portal-main`, chromedp.ByQuery),
		chromedp.Evaluate(`(function () {
			var el = document.querySelector('main.portal-main');
			var r = el.getBoundingClientRect();
			return Math.abs(r.left - (window.innerWidth - r.right)) <= 2;
		})()`, &marginsEqual),
	); err != nil {
		t.Fatalf("centered check: %v", err)
	}
	if !marginsEqual {
		t.Errorf(".portal-main is not horizontally centered")
	}
}

// TestLanding_AuthRoutesToIndex (AC7) — signed-in users at / still get
// the dashboard, not the landing.
func TestLanding_AuthRoutesToIndex(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install session: %v", err)
	}

	var bodyText string
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`.version-chip`, chromedp.ByQuery),
		chromedp.Text("body", &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate /: %v", err)
	}
	// index.html displays "Signed in as <email>" — landing.html does not.
	if !strings.Contains(bodyText, "Signed in as") {
		t.Errorf("expected dashboard for authed user; body=%s", bodyText)
	}
	if strings.Contains(bodyText, "[01] CONFIGURE") {
		t.Errorf("authed user should NOT see landing 01/02/03 panels; body=%s", bodyText)
	}
}

//go:build portalui

package portalui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestPortalCSP_HeaderEmittedByHarness covers AC1: the harness response
// must carry the production Content-Security-Policy header. Without
// this, every chromedp test runs against a CSP-free server while pprod
// runs under strict CSP — the gap that masked story_a7297367's bug
// from story_6b1ef853's "strengthened" tests.
func TestPortalCSP_HeaderEmittedByHarness(t *testing.T) {
	h := StartHarness(t)

	resp, err := http.Get(h.BaseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatalf("harness did not emit Content-Security-Policy header")
	}
	for _, want := range []string{
		"default-src 'self'",
		"script-src",
		"https://cdn.jsdelivr.net",
		"'unsafe-eval'", // story_a7297367
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
}

// TestPortalCSP_AlpineWorks covers AC2: under the production CSP,
// Alpine.js can still evaluate its directives (x-data, x-show,
// @click) — clicking the hamburger toggles the dropdown's
// computed display.
//
// Pre-fix (without 'unsafe-eval' in script-src) Alpine cannot
// instantiate any directive, so the dropdown's `x-show="open"` never
// runs; combined with `[x-cloak]` being silently removed by Alpine's
// pre-eval init, the dropdown ends up rendering with the CSS default
// `.nav-dropdown { display: flex }` — permanently visible. This test
// exercises the click toggle, which Alpine MUST evaluate to flip
// state.
func TestPortalCSP_AlpineWorks(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install session cookie: %v", err)
	}

	const disp = `(() => {
		const el = document.querySelector('[data-testid="nav-dropdown"]');
		return el ? getComputedStyle(el).display : 'NOT-FOUND';
	})()`

	var dispOnLoad, dispAfterClick, dispAfterEsc string
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(disp, &dispOnLoad),
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(disp, &dispAfterClick),
		chromedp.KeyEvent(kb.Escape),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(disp, &dispAfterEsc),
	); err != nil {
		t.Fatalf("under-CSP toggle: %v", err)
	}

	if dispOnLoad != "none" {
		t.Errorf("under CSP: nav-dropdown computed display on load = %q, want 'none' (Alpine x-show should evaluate to false)", dispOnLoad)
	}
	if dispAfterClick == "none" || dispAfterClick == "NOT-FOUND" {
		t.Errorf("under CSP: nav-dropdown computed display after hamburger click = %q, want non-none (Alpine @click MUST evaluate)", dispAfterClick)
	}
	if dispAfterEsc != "none" {
		t.Errorf("under CSP: nav-dropdown computed display after Escape = %q, want 'none' (Alpine @keydown MUST evaluate)", dispAfterEsc)
	}
}

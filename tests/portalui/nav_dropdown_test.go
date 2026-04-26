//go:build portalui

package portalui

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestNavDropdown_OpenClose verifies story_7c2c0f2e: the hamburger dropdown
// is closed on first paint, toggles on hamburger click, closes when the
// user clicks outside, and closes on Escape. The bug was that the
// surrounding @click.outside lived on <header> rather than on the
// dropdown's own visibility surface, so within-header clicks couldn't
// close the menu and there was no Escape handler.
func TestNavDropdown_OpenClose(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install session cookie: %v", err)
	}

	const isVisible = `(() => { const el = document.querySelector('[data-testid="nav-dropdown"]'); return !!el && el.offsetParent !== null; })()`

	// AC1: closed on first paint.
	var firstPaintVisible bool
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		// Give Alpine a tick to initialise; x-cloak should keep the dropdown
		// hidden either way.
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(isVisible, &firstPaintVisible),
	); err != nil {
		t.Fatalf("first paint check: %v", err)
	}
	if firstPaintVisible {
		t.Errorf("AC1: nav-dropdown visible on first paint; expected hidden")
	}

	// AC2: hamburger toggles open then closed.
	var afterFirstClick bool
	var afterSecondClick bool
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(isVisible, &afterFirstClick),
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(isVisible, &afterSecondClick),
	); err != nil {
		t.Fatalf("toggle check: %v", err)
	}
	if !afterFirstClick {
		t.Errorf("AC2: dropdown not visible after first hamburger click")
	}
	if afterSecondClick {
		t.Errorf("AC2: dropdown still visible after second hamburger click; expected hidden")
	}

	// AC3: clicking outside the dropdown closes it.
	var afterOpenForOutside bool
	var afterOutsideClick bool
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(isVisible, &afterOpenForOutside),
		// Click in the page body well clear of the dropdown.
		chromedp.Evaluate(`(() => {
			const evt = new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window, clientX: 50, clientY: 400});
			document.body.dispatchEvent(evt);
			const evt2 = new MouseEvent('click', {bubbles: true, cancelable: true, view: window, clientX: 50, clientY: 400});
			document.body.dispatchEvent(evt2);
		})()`, nil),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(isVisible, &afterOutsideClick),
	); err != nil {
		t.Fatalf("outside-click check: %v", err)
	}
	if !afterOpenForOutside {
		t.Fatalf("AC3 setup: dropdown failed to open before outside-click step")
	}
	if afterOutsideClick {
		t.Errorf("AC3: dropdown still visible after click outside; expected hidden")
	}

	// AC4: Escape closes the dropdown.
	var afterOpenForEsc bool
	var afterEsc bool
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(isVisible, &afterOpenForEsc),
		chromedp.KeyEvent(kb.Escape),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(isVisible, &afterEsc),
	); err != nil {
		t.Fatalf("escape check: %v", err)
	}
	if !afterOpenForEsc {
		t.Fatalf("AC4 setup: dropdown failed to open before escape step")
	}
	if afterEsc {
		t.Errorf("AC4: dropdown still visible after Escape; expected hidden")
	}
}

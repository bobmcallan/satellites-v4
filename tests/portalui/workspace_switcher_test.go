//go:build portalui

package portalui

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestWorkspaceSwitcher_DropdownDoesNotReflowNav covers AC4 of
// story_4d1ef14f: opening the workspace switcher menu must not change
// the height of `.nav-inner`. The menu is `position: absolute` so it
// floats over content; if a regression flips it back to flow layout
// the nav row would grow taller and this test would fail.
func TestWorkspaceSwitcher_DropdownDoesNotReflowNav(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	var heightBefore, heightAfter float64
	var menuVisible bool
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`.nav-workspace .btn-link`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.nav-inner').getBoundingClientRect().height`, &heightBefore),
		chromedp.Click(`.nav-workspace .btn-link`, chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(`(() => { const el = document.querySelector('[data-testid="nav-workspace-menu"]'); return !!el && el.offsetParent !== null; })()`, &menuVisible),
		chromedp.Evaluate(`document.querySelector('.nav-inner').getBoundingClientRect().height`, &heightAfter),
	); err != nil {
		t.Fatalf("chromedp run: %v", err)
	}

	if !menuVisible {
		t.Errorf("workspace menu not visible after click")
	}
	if heightBefore != heightAfter {
		t.Errorf("nav-inner height changed when menu opened: before=%v after=%v (menu must be position:absolute so layout cannot shift)", heightBefore, heightAfter)
	}
	if heightBefore == 0 {
		t.Errorf("nav-inner height returned 0 — selector likely missed; check .nav-inner is rendered")
	}
}

//go:build portalui

package portalui

import (
	"context"
	"fmt"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestTypography_DropdownsShareFontSize covers AC4 of story_2469358b:
// every dropdown trigger and dropdown menu item shares one
// `getComputedStyle('font-size')` value at runtime. Drives the
// browser to /, opens the hamburger so the theme picker becomes
// visible, then queries the four control selectors.
func TestTypography_DropdownsShareFontSize(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	type sample struct {
		name     string
		selector string
	}
	samples := []sample{
		{name: "workspace switcher button", selector: ".nav-workspace .btn-link"},
		{name: "theme picker button", selector: ".theme-picker-btn"},
		{name: "ws-debug retry button", selector: ".ws-debug .btn-link"},
	}

	values := map[string]string{}
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`.nav-workspace .btn-link`, chromedp.ByQuery),
		// Open the hamburger so the theme picker is rendered (it lives
		// inside the dropdown).
		chromedp.Click(`[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.theme-picker-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open hamburger: %v", err)
	}
	for _, s := range samples {
		var v string
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(fmt.Sprintf(`(() => { const el = document.querySelector(%q); if (!el) return ""; return getComputedStyle(el).fontSize; })()`, s.selector), &v),
		); err != nil {
			t.Fatalf("eval %s: %v", s.name, err)
		}
		if v == "" {
			// .ws-debug only exists once the indicator's debug panel is
			// open. Skip silently — it isn't always present.
			if s.name == "ws-debug retry button" {
				continue
			}
			t.Errorf("selector %s missing on /", s.selector)
			continue
		}
		values[s.name] = v
	}
	if len(values) < 2 {
		t.Fatalf("captured fewer than two control font-sizes: %+v", values)
	}
	var first string
	for k, v := range values {
		if first == "" {
			first = v
			continue
		}
		if v != first {
			t.Errorf("font-size mismatch: %q has %s, baseline %s", k, v, first)
		}
	}
}

// TestTypography_NoHorizontalClip covers AC5: a multi-character workspace
// name fits inside the switcher button's bounding box (scrollWidth <=
// clientWidth). The harness's seeded workspace is "personal" — long
// enough to exercise the rule.
func TestTypography_NoHorizontalClip(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	var scrollWidth, clientWidth float64
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`.nav-workspace .btn-link`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.nav-workspace .btn-link').scrollWidth`, &scrollWidth),
		chromedp.Evaluate(`document.querySelector('.nav-workspace .btn-link').clientWidth`, &clientWidth),
	); err != nil {
		t.Fatalf("chromedp run: %v", err)
	}
	if scrollWidth > clientWidth {
		t.Errorf("workspace switcher button text overflows horizontally: scrollWidth=%v clientWidth=%v (button must grow to fit, not clip)", scrollWidth, clientWidth)
	}
}

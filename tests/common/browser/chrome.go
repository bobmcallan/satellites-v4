// Package browser provides chromedp boot + cookie helpers for
// integration tests that drive the portal SSR/JS surface against a
// real satellites container (see tests/common/containers).
//
// Tests with no local chromium binary t.Skip cleanly so the suite
// still passes on chromium-less environments.
package browser

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdpnet "github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// DefaultDeadline is the per-test cap on chromedp work. Tests still
// wrap individual flows in shorter context.WithTimeout windows.
const DefaultDeadline = 90 * time.Second

// New spins up a headless chromium and probes it. When no chromium
// binary is reachable the call falls back to t.Skipf so contributors
// without chromium installed don't see a hard fail.
//
// Returned cancel tears down both the chromedp context and the
// underlying ExecAllocator.
func New(t *testing.T, parent context.Context) (context.Context, context.CancelFunc) {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parent, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)

	// Force the browser to launch so chromium-missing failures surface
	// here as a skip rather than later as an action error.
	if err := chromedp.Run(browserCtx); err != nil {
		cancelBrowser()
		cancelAlloc()
		t.Skipf("chromium unavailable: %v", err)
	}
	cancel := func() {
		cancelBrowser()
		cancelAlloc()
	}
	return browserCtx, cancel
}

// InstallCookie writes a cookie under baseURL's origin so the next
// navigation lands authenticated. Pass auth.CookieName as name.
func InstallCookie(ctx context.Context, baseURL, name, value string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base url: %w", err)
	}
	expires := cdp.TimeSinceEpoch(time.Now().Add(time.Hour))
	return chromedp.Run(ctx,
		cdpnet.SetCookie(name, value).
			WithDomain(u.Hostname()).
			WithPath("/").
			WithHTTPOnly(true).
			WithExpires(&expires),
	)
}

// JSClick fires a synthetic click on the first element matching the
// CSS selector via HTMLElement.click(). Alpine.js (under the
// @alpinejs/csp build the portal ships) does not invoke `@click`
// handlers for chromedp's CDP-injected Input.dispatchMouseEvent —
// only JS-level click events do. Real user clicks in production are
// unaffected; this is a chromedp ↔ @alpinejs/csp interaction quirk.
func JSClick(selector string) chromedp.Action {
	expr := `(() => {
		const el = document.querySelector(` + jsString(selector) + `);
		if (!el) { return false; }
		el.click();
		return true;
	})()`
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("JSClick: no element matched selector %q", selector)
		}
		return nil
	})
}

// jsString returns a JS string literal safely encoding s.
func jsString(s string) string {
	var b []byte
	b = append(b, '\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '\'':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, c)
		}
	}
	b = append(b, '\'')
	return string(b)
}

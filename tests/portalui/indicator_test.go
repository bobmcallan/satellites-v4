//go:build portalui

package portalui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestIndicator_LiveOnConnect is the happy-path sanity. The widget
// connects, the AuthHub accepts the subscribe, the dot transitions
// idle → connecting → live within the chromedp deadline.
func TestIndicator_LiveOnConnect(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installFastFlag(browserCtx); err != nil {
		t.Fatalf("install fast flag: %v", err)
	}
	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(".ws-indicator", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("wait live: %v", err)
	}
}

// TestIndicator_DropToReconnecting_RecoverGreen covers AC4: server
// outage → indicator goes amber → green when service returns. The fast
// flag compresses backoff so the reconnecting label is visible within
// a couple of polling cycles.
func TestIndicator_DropToReconnecting_RecoverGreen(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installFastFlag(browserCtx); err != nil {
		t.Fatalf("install fast flag: %v", err)
	}
	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(".ws-indicator", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("initial live: %v", err)
	}

	// Simulate server outage: drop the kill-switch and force-close active
	// /ws conns. The browser sees onclose → reconnecting.
	h.DisableWS()
	if err := waitForIndicatorState(browserCtx, "reconnecting", 5*time.Second); err != nil {
		t.Fatalf("wait reconnecting: %v", err)
	}

	// Bring service back; an in-flight backoff tick reconnects.
	h.EnableWS()
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("recover live: %v", err)
	}
}

// TestIndicator_ProlongedOutage_TurnsRed covers AC5: when the service
// stays down through the cap retries the indicator must flip to
// disconnected and the retry button must be visible.
func TestIndicator_ProlongedOutage_TurnsRed(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installFastFlag(browserCtx); err != nil {
		t.Fatalf("install fast flag: %v", err)
	}
	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/?debug=true"),
		chromedp.WaitVisible(".ws-indicator", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("initial live: %v", err)
	}

	// Disable WS and keep it down — the client must exhaust MAX_CAP_RETRIES
	// and land on disconnected.
	h.DisableWS()
	if err := waitForIndicatorState(browserCtx, "disconnected", 15*time.Second); err != nil {
		t.Fatalf("wait disconnected: %v", err)
	}

	// Retry button is rendered behind x-show="status === 'disconnected'".
	// Open the debug panel (debug=true is set) so the retry button is
	// reachable, then assert visibility.
	var retryHTML string
	if err := chromedp.Run(browserCtx,
		chromedp.Click(".ws-indicator .ws-indicator-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".ws-debug button.btn-link", chromedp.ByQuery),
		chromedp.OuterHTML(".ws-debug button.btn-link", &retryHTML, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("retry button check: %v", err)
	}
	if !strings.Contains(retryHTML, "retry") {
		t.Errorf("retry button HTML missing label; got %s", retryHTML)
	}
}

// TestIndicator_RetryButton_Recovers covers AC6: clicking retry from the
// disconnected state recovers to live once the server is back.
func TestIndicator_RetryButton_Recovers(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installFastFlag(browserCtx); err != nil {
		t.Fatalf("install fast flag: %v", err)
	}
	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/?debug=true"),
		chromedp.WaitVisible(".ws-indicator", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("initial live: %v", err)
	}
	h.DisableWS()
	if err := waitForIndicatorState(browserCtx, "disconnected", 15*time.Second); err != nil {
		t.Fatalf("wait disconnected: %v", err)
	}

	// Open the debug panel + click retry. The harness keeps WS off so we
	// need to re-enable BEFORE the click; otherwise the retry triggers
	// another reconnecting cycle and we just bounce.
	h.EnableWS()
	if err := chromedp.Run(browserCtx,
		chromedp.Click(".ws-indicator .ws-indicator-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".ws-debug button.btn-link", chromedp.ByQuery),
		chromedp.Click(".ws-debug button.btn-link", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("retry click: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("recover after retry: %v", err)
	}
}

// TestIndicator_DebugPanel_RendersEvents covers AC7: with debug=true the
// indicator's recent-events buffer fills as hub events arrive, and the
// panel renders them.
func TestIndicator_DebugPanel_RendersEvents(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installFastFlag(browserCtx); err != nil {
		t.Fatalf("install fast flag: %v", err)
	}
	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install cookie: %v", err)
	}
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/?debug=true"),
		chromedp.WaitVisible(".ws-indicator", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("initial live: %v", err)
	}

	// Publish three events on the workspace topic. The wshandler streams
	// them to the browser; the SatellitesWS client appends to recentEvents
	// (capped at DEBUG_BUFFER_CAP=10).
	for i := 0; i < 3; i++ {
		h.PublishEvent("test.event", map[string]any{"i": i})
	}

	// Open the debug panel and poll the rendered <li> entries until at
	// least three "kind" code blocks appear, meaning the streamed events
	// reached recentEvents and Alpine has rendered the x-for. Polling
	// avoids any reliance on Alpine internals (Alpine 3 doesn't expose
	// `el.__x`); we trust the DOM.
	if err := chromedp.Run(browserCtx,
		chromedp.Click(".ws-indicator .ws-indicator-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".ws-debug", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open debug panel: %v", err)
	}
	if err := waitForDebugEntries(browserCtx, 3, 5*time.Second); err != nil {
		t.Fatalf("wait debug entries: %v", err)
	}
}

// waitForDebugEntries polls the rendered debug panel for at least n
// non-empty <li> rows containing a `<code>` (kind) child — the empty
// "no events yet" placeholder uses class "ws-debug-empty" and lacks a
// <code>, so it's excluded.
func waitForDebugEntries(ctx context.Context, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var html string
		err := chromedp.Run(ctx,
			chromedp.OuterHTML(".ws-debug ul.ws-debug-events", &html, chromedp.ByQuery),
		)
		if err == nil {
			if got := strings.Count(html, "<code"); got >= n {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return context.DeadlineExceeded
}

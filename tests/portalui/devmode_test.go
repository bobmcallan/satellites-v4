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

// TestDevMode_QuickSignin verifies AC1: clicking the dev signin button
// completes auth in one click, lands on / with the dashboard rendered.
func TestDevMode_QuickSignin(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	var bodyText string
	if err := chromedp.Run(browserCtx,
		network.ClearBrowserCookies(),
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`form[data-testid="dev-signin"] button`, chromedp.ByQuery),
		chromedp.Click(`form[data-testid="dev-signin"] button`, chromedp.ByQuery),
		// Form POST → 303 → GET /. Wait briefly for the redirect to land.
		chromedp.Sleep(400*time.Millisecond),
		chromedp.WaitVisible(`.version-chip`, chromedp.ByQuery),
		chromedp.Text("body", &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("dev signin: %v", err)
	}

	// index.html for authed users renders "Signed in as" (story_92210e4a
	// AC7). We're a dev user now, so the dashboard should render.
	if !strings.Contains(bodyText, "Signed in as") {
		t.Errorf("expected dashboard after dev signin, got body=%s", bodyText)
	}
	// DEV chip present (AC3).
	if !strings.Contains(bodyText, "DEV") {
		t.Errorf("expected DEV chip after dev signin, got body=%s", bodyText)
	}
}

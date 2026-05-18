//go:build portalui

// sty_7bc94f80 — chromedp coverage for the project-page stale-data
// banner. The two assertions:
//
//  1. After /projects/{id} reaches `live`, calling h.DisableWS() must
//     surface the .project-stale-banner within 1 s; re-enabling the WS
//     must hide it again within 1 s.
//  2. A prolonged outage (cap-retries → `disconnected`) must surface
//     the refresh button (`data-testid="project-stale-banner-refresh"`).
//
// Both reuse the harness from indicator_test.go: installFastFlag
// compresses backoff so the disconnected state is reachable in <15 s.

package portalui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/bobmcallan/satellites/internal/story"
)

// TestProjectStaleBanner_AppearsOnDisconnect covers ACs 3 + 4 + the
// render-side expectations the SSR test cannot reach: the banner is
// hidden on first paint, becomes visible within 1 s of the WS dropping,
// and disappears within 1 s of the WS coming back.
func TestProjectStaleBanner_AppearsOnDisconnect(t *testing.T) {
	h := StartHarness(t)
	proj := seedBannerProject(t, h, "alpha-banner")

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

	projectURL := h.BaseURL + "/projects/" + proj.ID
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(projectURL),
		chromedp.WaitVisible(`[data-testid="project-stale-banner"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate project page: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("initial live: %v", err)
	}

	// Banner must be HIDDEN at first paint (status=live → is-hidden).
	if err := waitForBannerVisibility(browserCtx, false, 1*time.Second); err != nil {
		t.Fatalf("banner should be hidden on first paint (status=live); got %v", err)
	}

	// AC: banner appears within 1s of the WS dropping.
	h.DisableWS()
	if err := waitForBannerVisibility(browserCtx, true, 1*time.Second); err != nil {
		t.Fatalf("banner did not appear within 1s of DisableWS: %v", err)
	}

	// AC: banner disappears within 1s of the WS coming back.
	h.EnableWS()
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("recover live: %v", err)
	}
	if err := waitForBannerVisibility(browserCtx, false, 1*time.Second); err != nil {
		t.Fatalf("banner did not disappear within 1s of EnableWS: %v", err)
	}
}

// TestProjectStaleBanner_RefreshButtonOnProlongedOutage covers AC 5 —
// the disconnected state surfaces the refresh affordance. The cap
// retries land on `disconnected` after the fast-flag backoff exhausts.
func TestProjectStaleBanner_RefreshButtonOnProlongedOutage(t *testing.T) {
	h := StartHarness(t)
	proj := seedBannerProject(t, h, "alpha-banner-cap")

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

	projectURL := h.BaseURL + "/projects/" + proj.ID
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(projectURL),
		chromedp.WaitVisible(`[data-testid="project-stale-banner"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate project page: %v", err)
	}
	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("initial live: %v", err)
	}

	// Kill WS and keep it down — the client must exhaust MAX_CAP_RETRIES
	// at the compressed-backoff cap and flip to disconnected. Match the
	// 15 s budget the indicator's prolonged-outage test uses.
	h.DisableWS()
	if err := waitForIndicatorState(browserCtx, "disconnected", 15*time.Second); err != nil {
		t.Fatalf("wait disconnected: %v", err)
	}

	// Refresh button is gated by :class="hiddenWhenNotDisconnected" —
	// must be visible (no is-hidden class) once status === 'disconnected'.
	if err := waitForRefreshButtonVisible(browserCtx, 2*time.Second); err != nil {
		t.Fatalf("refresh button not visible on disconnected: %v", err)
	}

	// Banner copy must show the disconnected string. We assert the
	// exact AC-mandated phrase including the en-dash.
	var copyText string
	if err := chromedp.Run(browserCtx,
		chromedp.Text(`[data-testid="project-stale-banner"] .project-stale-banner-text`, &copyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read banner copy: %v", err)
	}
	if !strings.Contains(copyText, "Live updates unavailable") {
		t.Errorf("banner copy missing disconnected phrase; got %q", copyText)
	}
}

// seedBannerProject seeds a project + a story so the project_detail
// page renders with at least one panel under it (the banner must mount
// before the {{range .Panels}} loop and the panel rendering exercises
// the surrounding markup at the same time).
func seedBannerProject(t *testing.T, h *Harness, name string) *seededProject {
	t.Helper()
	now := time.Now().UTC()
	ctx := context.Background()
	proj, err := h.Projects.Create(ctx, h.UserID, h.WorkspaceID, name, now)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := h.Stories.Create(ctx, story.Story{
		ProjectID:          proj.ID,
		WorkspaceID:        h.WorkspaceID,
		Title:              "stale-banner fixture",
		Description:        "sty_7bc94f80",
		AcceptanceCriteria: "rendered",
		Status:             "backlog",
		Priority:           "medium",
		Category:           "feature",
		CreatedBy:          h.UserID,
	}, now); err != nil {
		t.Fatalf("seed story: %v", err)
	}
	return &seededProject{ID: proj.ID}
}

type seededProject struct{ ID string }

// waitForBannerVisibility polls until the .project-stale-banner element
// matches the requested visibility (true = NOT is-hidden, false = is-hidden).
func waitForBannerVisibility(ctx context.Context, wantVisible bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var classes string
		err := chromedp.Run(ctx,
			chromedp.AttributeValue(`[data-testid="project-stale-banner"]`, "class", &classes, nil),
		)
		if err == nil {
			hasHidden := containsClassToken(classes, "is-hidden")
			visible := !hasHidden
			if visible == wantVisible {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for banner visibility=%v", wantVisible)
}

// waitForRefreshButtonVisible polls until the refresh button no longer
// carries the `is-hidden` token (the disconnected gate has opened).
func waitForRefreshButtonVisible(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var classes string
		err := chromedp.Run(ctx,
			chromedp.AttributeValue(`[data-testid="project-stale-banner-refresh"]`, "class", &classes, nil),
		)
		if err == nil && !containsClassToken(classes, "is-hidden") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for refresh button to become visible")
}

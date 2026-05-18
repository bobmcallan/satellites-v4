//go:build portalui

// sty_7667c9bc — integration-boundary slice for the shared client-side
// realtime bridge. Two browser drives:
//
//  1. /projects/{id}: publish a `story.in_progress` hub event, assert
//     the matching <tr.story-row data-id=…> patches to
//     data-status="in_progress" within 2 s.
//  2. /projects/{id}/ledger: publish a `ledger.created` hub event,
//     assert the new-rows pill appears within 2 s.
//
// Both runs install a single-WS spy that wraps the SatellitesWS
// constructor with a counter so we can confirm exactly one
// SatellitesWS is constructed by the bridge per page (the wsIndicator
// inside ws.js uses the unqualified `SatellitesWS` form and is
// intentionally excluded from the count — its collapse into the
// bridge is the next story, sty_7667c9bc plan §2.7).

package portalui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/bobmcallan/satellites/internal/story"
)

// installBridgeWSSpy installs a wrapper around window.SatellitesWS BEFORE
// the page scripts run so the bridge's `new window.SatellitesWS(…)`
// call site is counted on `window.__bridgeWSCount`. The wsIndicator
// uses the unqualified `SatellitesWS` global from inside ws.js and is
// not affected.
func installBridgeWSSpy(ctx context.Context) error {
	js := `
		(function () {
			window.__bridgeWSCount = 0;
			Object.defineProperty(window, 'SatellitesWS', {
				configurable: true,
				get: function () { return window.__realSatellitesWS; },
				set: function (cls) {
					var Real = cls;
					var Wrapped = function (opts) {
						window.__bridgeWSCount += 1;
						return new Real(opts);
					};
					Wrapped.STATES = Real.STATES;
					window.__realSatellitesWS = Wrapped;
				}
			});
		})();
	`
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(js).Do(ctx)
			return err
		}),
	)
}

// TestRealtimeBridge_E2E_StoryAndLedger drives the bridge end-to-end on
// the two pages it must serve. Each subtest seeds its own fixtures so
// the harness instance state stays minimal.
func TestRealtimeBridge_E2E_StoryAndLedger(t *testing.T) {
	h := StartHarness(t)

	now := time.Now().UTC()
	ctx := context.Background()
	proj, err := h.Projects.Create(ctx, h.UserID, h.WorkspaceID, "alpha", now)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seedStory, err := h.Stories.Create(ctx, story.Story{
		ProjectID:          proj.ID,
		WorkspaceID:        h.WorkspaceID,
		Title:              "fixture story for realtime bridge",
		Description:        "sty_7667c9bc fixture",
		AcceptanceCriteria: "covered by bridge dispatch",
		Status:             "backlog",
		Priority:           "medium",
		Category:           "feature",
		CreatedBy:          h.UserID,
	}, now)
	if err != nil {
		t.Fatalf("seed story: %v", err)
	}

	t.Run("project_page_patches_on_story_event", func(t *testing.T) {
		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installFastFlag(browserCtx); err != nil {
			t.Fatalf("install fast flag: %v", err)
		}
		if err := installBridgeWSSpy(browserCtx); err != nil {
			t.Fatalf("install ws spy: %v", err)
		}
		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install cookie: %v", err)
		}

		projectURL := h.BaseURL + "/projects/" + proj.ID + "?expand=stories"
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(projectURL),
			chromedp.WaitVisible(`[data-testid="realtime-bridge"]`, chromedp.ByQuery),
			chromedp.WaitVisible(`[data-testid="panel-stories-table"] tr.story-row`, chromedp.ByQuery),
		); err != nil {
			t.Fatalf("navigate project page: %v", err)
		}
		if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
			t.Fatalf("wait initial live: %v", err)
		}

		h.PublishEvent("story.in_progress", map[string]any{
			"workspace_id": h.WorkspaceID,
			"project_id":   proj.ID,
			"story_id":     seedStory.ID,
		})

		if err := chromedp.Run(browserCtx,
			chromedp.Poll(
				`(() => {
					const r = document.querySelector('tr.story-row[data-id="`+seedStory.ID+`"]');
					return !!(r && r.dataset.status === 'in_progress');
				})()`,
				nil,
				chromedp.WithPollingTimeout(2*time.Second),
			),
		); err != nil {
			t.Fatalf("story row did not patch to in_progress within 2s: %v", err)
		}

		// Bridge owns exactly one SatellitesWS construction on this page.
		var bridgeWSCount int
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(`window.__bridgeWSCount || 0`, &bridgeWSCount),
		); err != nil {
			t.Fatalf("read bridge ws count: %v", err)
		}
		if bridgeWSCount != 1 {
			t.Errorf("bridge SatellitesWS construction count = %d, want 1 (wsIndicator's unqualified ctor is excluded)", bridgeWSCount)
		}
	})

	t.Run("ledger_page_patches_on_ledger_event", func(t *testing.T) {
		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installFastFlag(browserCtx); err != nil {
			t.Fatalf("install fast flag: %v", err)
		}
		if err := installBridgeWSSpy(browserCtx); err != nil {
			t.Fatalf("install ws spy: %v", err)
		}
		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install cookie: %v", err)
		}

		ledgerURL := h.BaseURL + "/projects/" + proj.ID + "/ledger"
		var html string
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(ledgerURL),
			chromedp.WaitVisible(`[data-testid="realtime-bridge"]`, chromedp.ByQuery),
			chromedp.WaitVisible(`[data-testid="ledger-header"]`, chromedp.ByQuery),
			chromedp.OuterHTML("html", &html, chromedp.ByQuery),
		); err != nil {
			t.Fatalf("navigate ledger page: %v", err)
		}
		if !strings.Contains(html, `data-project-id="`+proj.ID+`"`) {
			t.Fatalf("ledger main missing data-project-id host attribute; bridge cannot scope")
		}
		if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
			t.Fatalf("wait initial live: %v", err)
		}

		h.PublishEvent("ledger.created", map[string]any{
			"id":          "ldg_e2e_bridge",
			"project_id":  proj.ID,
			"type":        "decision",
			"tags":        []string{"kind:test-bridge"},
			"content":     "bridge-dispatched-row",
			"durability":  "durable",
			"source_type": "system",
			"status":      "active",
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		})

		if err := chromedp.Run(browserCtx,
			chromedp.Poll(
				`document.querySelector('[data-testid="new-rows-pill"]') !== null`,
				nil,
				chromedp.WithPollingTimeout(2*time.Second),
			),
		); err != nil {
			t.Fatalf("new-rows pill did not appear within 2s: %v", err)
		}

		var bridgeWSCount int
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(`window.__bridgeWSCount || 0`, &bridgeWSCount),
		); err != nil {
			t.Fatalf("read bridge ws count: %v", err)
		}
		if bridgeWSCount != 1 {
			t.Errorf("bridge SatellitesWS construction count = %d, want 1 on ledger page", bridgeWSCount)
		}
	})
}

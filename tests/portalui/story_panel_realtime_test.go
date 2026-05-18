//go:build portalui

package portalui

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/bobmcallan/satellites/internal/story"
)

// TestStoryPanel_StatusPropagation_Under500ms (sty_f52d540e) — drives
// the storyPanel realtime path end-to-end: seed a story in `backlog`,
// open the project detail with `?expand=<storyID>` so the row renders
// SSR, wait for the ws indicator to flip to `live`, then call the
// harness helper that performs the in-process status mutation AND
// publishes the wire-translated `story.<status>` event. The chromedp
// poll asserts BOTH `row.dataset.status` AND the `.col-status
// .status-pill` text flip to `in_progress` within a 500 ms client-side
// deadline.
//
// Latency budget: 500 ms from helper invocation → both DOM signals
// visible. t.Logf records the measured elapsed for postmortem signal.
func TestStoryPanel_StatusPropagation_Under500ms(t *testing.T) {
	h := StartHarness(t)

	now := time.Now().UTC()
	proj, err := h.Projects.Create(context.Background(), h.UserID, h.WorkspaceID, "realtime-alpha", now)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	s, err := h.Stories.Create(context.Background(), story.Story{
		ProjectID:   proj.ID,
		WorkspaceID: h.WorkspaceID,
		Title:       "realtime status fixture",
		Status:      story.StatusBacklog,
		Priority:    "medium",
		Category:    "feature",
		CreatedBy:   h.UserID,
	}, now)
	if err != nil {
		t.Fatalf("seed story: %v", err)
	}

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

	rowSel := `tr.story-row[data-id="` + s.ID + `"]`
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/projects/"+proj.ID+"?expand="+s.ID),
		chromedp.WaitVisible(rowSel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate / wait row: %v", err)
	}

	if err := waitForIndicatorState(browserCtx, "live", 10*time.Second); err != nil {
		t.Fatalf("wait ws live: %v", err)
	}

	// The JS predicate returns the in-page data-realtime-updated-at
	// timestamp ONLY when BOTH the data-status attribute AND the
	// .col-status .status-pill text have flipped to `in_progress`. Any
	// half-flipped state yields null and chromedp re-polls until the
	// 500 ms cap.
	pollExpr := `(() => {
		const row = document.querySelector('` + rowSel + `');
		if (!row) return null;
		const pill = row.querySelector('.col-status .status-pill');
		if (!pill) return null;
		if (row.dataset.status === 'in_progress' && pill.textContent === 'in_progress') {
			const seen = Number(row.getAttribute('data-realtime-updated-at') || '0');
			return seen;
		}
		return null;
	})()`

	t0 := time.Now()
	h.UpdateStoryStatus(t, s.ID, story.StatusInProgress)

	var seenAt float64
	if err := chromedp.Run(browserCtx,
		chromedp.Poll(pollExpr, &seenAt,
			chromedp.WithPollingTimeout(500*time.Millisecond),
			chromedp.WithPollingInterval(10*time.Millisecond),
		),
	); err != nil {
		t.Fatalf("status did not propagate within 500ms: %v", err)
	}

	elapsed := time.Since(t0)
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("propagation budget exceeded: elapsed=%s, want < 500ms (data-realtime-updated-at=%.0f)", elapsed, seenAt)
	}
	t.Logf("propagation latency: %s (data-realtime-updated-at=%.0f)", elapsed, seenAt)
}

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/bobmcallan/satellites/tests/common/browser"
	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestPortalStoryStatusPill_FullStack_PillStableUnderActivityAppend is
// the regression test for sty_ec83484f. It drives a real
// satellites + surrealdb stack end-to-end and asserts the stories
// panel does NOT corrupt a story row's .status-pill text or
// data-status when the substrate emits a `story.activity.append`
// WireEvent (introduced by sty_e55f335e).
//
// Before the fix, _applyEvent routed any kind whose prefix was
// `story.` into _applyStoryEvent, which then stripped the prefix to
// derive the new status — so `story.activity.append` wrote
// `activity.append` into the pill and into row.dataset.status. The
// CSS then uppercased the broken text to ACTIVITY.APPEND.
//
// The test asserts three invariants under a real substrate-emitted
// story.activity.append (pill text, row data-status, row visibility
// under the default status:open chip), then a positive case proving
// the whitelist correctly routes a synthetic story.<status> frame
// into _applyStoryEvent and patches the pill in place.
//
// The positive case dispatches the story.<status> frame
// synthetically rather than via story_update + WS fan-out: the
// translator at internal/wshandler/translate.go:127 reads the row id
// via pickStringID(row, "id", "story_id"), but stories-table rows
// surface their id as a structured RecordID rather than a plain
// string on live mutations, so the live story_id field is empty in
// the WS payload. That substrate-side asymmetry is out of scope for
// this story (the unit under test here is the JS dispatcher
// whitelist); the synthetic positive case proves the whitelist
// routes valid story.<status> kinds without depending on the
// translator surface.
func TestPortalStoryStatusPill_FullStack_PillStableUnderActivityAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv: map[string]string{
			"SATELLITES_DEV_USERNAME": "dev@local",
			"SATELLITES_DEV_PASSWORD": "letmein",
		},
	})
	defer stack.Stop()

	cookie := devLogin(t, ctx, stack.BaseURL, "dev@local", "letmein")
	mcpURL := stack.BaseURL + "/mcp"

	mint := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "portal-story-status-pill",
	})
	bearer, _ := mint["key"].(string)
	if bearer == "" {
		t.Fatalf("agent_apikey_create returned no key: %+v", mint)
	}

	proj := callAPIv1(t, ctx, stack.BaseURL, bearer, "project_add", map[string]any{
		"name": "story-status-pill",
	})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", proj)
	}

	story := callAPIv1(t, ctx, stack.BaseURL, bearer, "story_add", map[string]any{
		"project_id": projectID,
		"title":      "pill-stability-under-activity-append",
	})
	storyID, _ := story["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add: %+v", story)
	}

	// Populate user_story so non-backlog transitions clear the
	// bug-template gate, then bake a definite non-default status
	// into the row so the assertion has something concrete to
	// anchor on.
	_ = callAPIv1(t, ctx, stack.BaseURL, bearer, "story_update", map[string]any{
		"id": storyID,
		"fields": map[string]any{
			"user_story": "as an operator, I want the status pill to stay correct under activity-append fan-out.",
		},
	})
	for _, next := range []string{"ready", "in_progress"} {
		_ = callAPIv1(t, ctx, stack.BaseURL, bearer, "story_update", map[string]any{
			"id":     storyID,
			"status": next,
		})
	}

	// ----- Browser drive -----
	browserCtx, cancelBrowser := browser.New(t, ctx)
	defer cancelBrowser()

	if err := browser.InstallCookie(browserCtx, stack.BaseURL, cookie.Name, cookie.Value); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	projURL := stack.BaseURL + "/projects/" + projectID
	storyRowSel := fmt.Sprintf(`tr.story-row[data-id="%s"]`, storyID)
	pillSel := fmt.Sprintf(`tr.story-row[data-id="%s"] .col-status .status-pill`, storyID)

	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(projURL),
		chromedp.WaitVisible(storyRowSel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate + wait for story row: %v", err)
	}

	// Pin $el + force-run init() on the storyPanel Alpine scope.
	// Under @alpinejs/csp, bare-function x-data factories
	// (x-data="storyPanel") do not always have their init() invoked
	// automatically and don't always carry the $el reference on the
	// raw data scope — pinning both is what lets _applyStoryEvent's
	// `this.$el.querySelector(...)` find the row when a WS frame
	// arrives. Also wraps onEvent so the test can audit which kinds
	// the dispatcher saw.
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(
		`(() => {
            const el = document.querySelector('[x-data="storyPanel"]');
            const d = el && el._x_dataStack && el._x_dataStack[0];
            if (!d) { return false; }
            if (!d.$el) { d.$el = el; }
            if (!d._ws) { d._attachRealtimeBridge(); }
            window.__capturedKinds = [];
            window.__applyStoryEventCalls = 0;
            if (d._ws) {
                const orig = d._ws.onEvent.bind(d._ws);
                d._ws.onEvent = function (ev) {
                    try { window.__capturedKinds.push(ev && ev.Kind); } catch (_) {}
                    return orig(ev);
                };
            }
            const origApplyStory = d._applyStoryEvent.bind(d);
            d._applyStoryEvent = function (ev, projectID) {
                window.__applyStoryEventCalls++;
                return origApplyStory(ev, projectID);
            };
            return !!d._ws;
        })()`,
		nil)); err != nil {
		t.Fatalf("attach realtime bridge: %v", err)
	}
	if err := pollEqual(browserCtx, 10*time.Second,
		`(() => { const el = document.querySelector('[x-data="storyPanel"]'); const d = el && el._x_dataStack && el._x_dataStack[0]; return (d && d._ws && d._ws.status) || ''; })()`,
		"live"); err != nil {
		t.Fatalf("realtime WS bridge did not reach live state: %v", err)
	}

	// SSR rendered the row with the in_progress status we baked in
	// above. Confirm before firing the activity-append at it.
	if err := pollEqual(browserCtx, 5*time.Second,
		fmt.Sprintf(`document.querySelector(%q)?.textContent || ''`, pillSel),
		"in_progress"); err != nil {
		t.Fatalf("pill never settled to in_progress on load: %v", err)
	}
	if err := pollEqual(browserCtx, 5*time.Second,
		fmt.Sprintf(`document.querySelector(%q)?.dataset?.status || ''`, storyRowSel),
		"in_progress"); err != nil {
		t.Fatalf("row data-status never settled to in_progress on load: %v", err)
	}

	// Append a ledger row scoped to the story whose tag set hits
	// activityKindSet (internal/wshandler/translate.go:21). This
	// fires a `story.activity.append` WireEvent — the exact frame
	// that used to corrupt the pill before sty_ec83484f.
	_ = callAPIv1(t, ctx, stack.BaseURL, bearer, "ledger_append", map[string]any{
		"project_id": projectID,
		"story_id":   storyID,
		"type":       "evidence",
		"content":    "regression seed — must NOT touch the status pill",
		"tags":       []string{"kind:evidence"},
	})

	// Wait for the substrate to deliver the activity-append frame
	// (ledger.append + story.activity.append fan-out). Without
	// waiting for delivery, the subsequent stability assertions
	// would race the WS.
	if err := pollEqual(browserCtx, 5*time.Second,
		`((window.__capturedKinds || []).includes('story.activity.append') ? 'yes' : 'no')`,
		"yes"); err != nil {
		t.Fatalf("substrate did not deliver story.activity.append WireEvent: %v", err)
	}

	// Bounded poll: assert the pill text + data-status stay at
	// in_progress for the full 2 s budget. Any flip during this
	// window is the pre-fix regression.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var pillText, rowStatus string
		var visible bool
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(
				fmt.Sprintf(`document.querySelector(%q)?.textContent || ''`, pillSel),
				&pillText),
			chromedp.Evaluate(
				fmt.Sprintf(`document.querySelector(%q)?.dataset?.status || ''`, storyRowSel),
				&rowStatus),
			// matchesRow honours the default status:open chip; a row
			// whose data-status was corrupted to "activity.append"
			// would no longer match status:open and would disappear.
			chromedp.Evaluate(
				fmt.Sprintf(`(() => { const r = document.querySelector(%q); if (!r) { return false; } const s = getComputedStyle(r); return s.display !== 'none' && s.visibility !== 'hidden'; })()`, storyRowSel),
				&visible),
		); err != nil {
			t.Fatalf("evaluate pill/data-status/visibility: %v", err)
		}
		if pillText != "in_progress" {
			t.Fatalf("pill text mutated under story.activity.append: got %q want %q (sty_ec83484f regression)", pillText, "in_progress")
		}
		if rowStatus != "in_progress" {
			t.Fatalf("row data-status mutated under story.activity.append: got %q want %q (sty_ec83484f regression)", rowStatus, "in_progress")
		}
		if !visible {
			t.Fatalf("row dropped out of status:open chip filter under story.activity.append (data-status corruption surrogate; sty_ec83484f regression)")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Cross-check: _applyStoryEvent must NOT have been invoked by
	// the activity.append fan-out — that's exactly the whitelist
	// invariant. (It WAS the bug for events on the story.* prefix
	// before sty_ec83484f.)
	var applyCount int
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`window.__applyStoryEventCalls || 0`, &applyCount)); err != nil {
		t.Fatalf("read apply count: %v", err)
	}
	if applyCount != 0 {
		t.Fatalf("_applyStoryEvent invoked %d time(s) for non-status story.* event (whitelist breached): %d", applyCount, applyCount)
	}

	// Positive case: dispatch a synthetic story.<status> frame
	// (story.blocked is the only non-terminal status the storyPanel
	// must keep responsive). Bypasses the substrate-side translator
	// asymmetry around stories-table id encoding; exercises only
	// the JS dispatcher's whitelist routing.
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(
		fmt.Sprintf(`(() => {
            const el = document.querySelector('[x-data="storyPanel"]');
            const d = el && el._x_dataStack && el._x_dataStack[0];
            d._applyEvent({
                Kind: 'story.blocked',
                Data: { project_id: %q, story_id: %q, status: 'blocked', title: 'pill-stability-under-activity-append', priority: '', category: '', tags: [] }
            }, %q);
        })()`, projectID, storyID, projectID),
		nil)); err != nil {
		t.Fatalf("dispatch synthetic story.blocked: %v", err)
	}
	if err := pollEqual(browserCtx, 2*time.Second,
		fmt.Sprintf(`document.querySelector(%q)?.textContent || ''`, pillSel),
		"blocked"); err != nil {
		t.Fatalf("pill did not update to blocked after synthetic story.blocked (whitelist regressed status path): %v", err)
	}
	if err := pollEqual(browserCtx, 2*time.Second,
		fmt.Sprintf(`document.querySelector(%q)?.dataset?.status || ''`, storyRowSel),
		"blocked"); err != nil {
		t.Fatalf("row data-status did not update to blocked after synthetic story.blocked: %v", err)
	}
}

// pollEqual reads a JS string expression in the page on a 100 ms
// cadence and returns nil as soon as it equals want. Times out per
// the deadline budget.
func pollEqual(ctx context.Context, budget time.Duration, expr, want string) error {
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		var got string
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &got)); err != nil {
			return fmt.Errorf("evaluate %s: %w", expr, err)
		}
		if got == want {
			return nil
		}
		last = got
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to equal %q (last=%q)", expr, want, last)
}

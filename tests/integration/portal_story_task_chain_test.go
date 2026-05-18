package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/bobmcallan/satellites/tests/common/browser"
	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestPortalStoryTaskChain_FullStack_RowShapeAndExpansion drives the
// in-story task chain redesign end-to-end against a real
// satellites + surrealdb stack. Asserts:
//   - the per-story task table renders the id / title / duration /
//     status / updated column shape (matches the story-row pattern);
//   - tasks have no native title — the title cell carries tag chips
//     (kind:work, action:plan, iter:#N when >1, outcome:success when
//     closed) per buildTaskChainTags;
//   - clicking a task row expands its detail sub-row.
func TestPortalStoryTaskChain_FullStack_RowShapeAndExpansion(t *testing.T) {
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
		"name": "portal-story-task-chain",
	})
	bearer, _ := mint["key"].(string)
	if bearer == "" {
		t.Fatalf("agent_apikey_create returned no key: %+v", mint)
	}

	proj := callAPIv1(t, ctx, stack.BaseURL, bearer, "project_add", map[string]any{
		"name": "in-story-chain",
	})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", proj)
	}

	agent := callAPIv1(t, ctx, stack.BaseURL, bearer, "document_get", map[string]any{
		"type": "agent", "name": "substrate_auditor",
	})
	agentID, _ := agent["id"].(string)
	if agentID == "" {
		t.Fatalf("document_get(agent substrate_auditor): %+v", agent)
	}

	story := callAPIv1(t, ctx, stack.BaseURL, bearer, "story_add", map[string]any{
		"project_id": projectID,
		"title":      "in-story-task-chain",
	})
	storyID, _ := story["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add: %+v", story)
	}

	seedTask := func(prompt string) string {
		resp := callAPIv1(t, ctx, stack.BaseURL, bearer, "task_add", map[string]any{
			"agent_id": agentID,
			"prompt":   prompt,
			"story_id": storyID,
			"kind":     "work",
			"action":   "contract:substrate_audit",
		})
		id, _ := resp["task_id"].(string)
		if id == "" {
			t.Fatalf("task_add %q: %+v", prompt, resp)
		}
		return id
	}
	seedTask("task A — to be closed (success)")
	seedTask("task B — to be claimed/in-flight")
	seedTask("task C — remains enqueued")

	closeClaim := callAPIv1(t, ctx, stack.BaseURL, bearer, "task_claim", map[string]any{
		"worker_id": "chain-worker-close",
	})
	closedID, _ := closeClaim["id"].(string)
	if closedID == "" {
		t.Fatalf("first claim: %+v", closeClaim)
	}
	_ = callAPIv1(t, ctx, stack.BaseURL, bearer, "task_update", map[string]any{
		"id":      closedID,
		"status":  "closed",
		"outcome": "success",
	})

	inflightClaim := callAPIv1(t, ctx, stack.BaseURL, bearer, "task_claim", map[string]any{
		"worker_id": "chain-worker-inflight",
	})
	inflightID, _ := inflightClaim["id"].(string)
	if inflightID == "" {
		t.Fatalf("second claim: %+v", inflightClaim)
	}

	// ----- Browser drive -----
	browserCtx, cancelBrowser := browser.New(t, ctx)
	defer cancelBrowser()

	if err := browser.InstallCookie(browserCtx, stack.BaseURL, cookie.Name, cookie.Value); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	projURL := stack.BaseURL + "/projects/" + projectID
	storyRowSel := fmt.Sprintf(`[data-testid="story-row-%s"]`, storyID)
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(projURL),
		chromedp.WaitVisible(storyRowSel, chromedp.ByQuery),
		browser.JSClick(storyRowSel),
		chromedp.WaitVisible(fmt.Sprintf(`[data-testid="story-tasks-%s"]`, storyID), chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate + expand story: %v", err)
	}

	var html string
	if err := chromedp.Run(browserCtx,
		chromedp.OuterHTML(fmt.Sprintf(`[data-testid="story-tasks-%s"]`, storyID),
			&html, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read task table html: %v", err)
	}

	for _, want := range []string{
		// New column headers — story-row-parity layout.
		`>id<`,
		`>title<`,
		`>duration<`,
		`>status<`,
		`>updated<`,
		// Old column headers must NOT appear (regression guard).
		// Note: ">#<" would match a literal column header from the old
		// "#" col-seq; check the col-seq class instead since "#" alone
		// also appears in iter values.
		// Tag-chip-driven title cell (no separate title text).
		`class="task-row-tags"`,
		`class="tag-chip is-clickable"`,
		`data-tag="kind:work"`,
		`data-tag="action:substrate_audit"`,
		`data-tag="outcome:success"`,
		// Duration pill on the closed row.
		`class="duration-pill"`,
		// Detail row stub present (collapsed by default).
		`class="story-task-detail`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("task chain html missing %q", want)
		}
	}
	for _, regression := range []string{
		`class="col-seq"`,
		`class="col-kind"`,
		`class="col-iter"`,
		`class="col-outcome"`,
	} {
		if strings.Contains(html, regression) {
			t.Errorf("task chain html still has legacy column class %q", regression)
		}
	}

	// Click the closed task row → its detail sub-row becomes visible.
	closedRowSel := fmt.Sprintf(`[data-testid="story-task-row-%s"]`, closedID)
	closedDetailSel := fmt.Sprintf(`[data-testid="story-task-detail-%s"]`, closedID)
	if err := chromedp.Run(browserCtx,
		browser.JSClick(closedRowSel),
		chromedp.WaitVisible(closedDetailSel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("expand closed task row %s: %v", closedID, err)
	}

	_ = inflightID
}

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

// TestPortalProjectTasks_FullStack_RowShapeAndExpansion drives the
// redesigned /projects/{id}/tasks page end-to-end against a real
// satellites + surrealdb stack. Verifies:
//   - the task table renders the new id/title/duration/status/updated
//     column shape (story-row parity);
//   - tag chips for kind, iteration, outcome, and story render in the
//     title cell;
//   - clicking a task row expands its detail row (Alpine handler);
//   - clicking a tag chip appends the token to the filter input and
//     auto-submits, reloading the page with ?q=… in the URL.
//
// Seeded shape: 3 work tasks against one story, one each in the
// enqueued / in-flight (claimed) / closed (outcome:success) states.
func TestPortalProjectTasks_FullStack_RowShapeAndExpansion(t *testing.T) {
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

	// Dev login carries the owner identity that subsequent project +
	// task creates inherit (via an API key minted under the session).
	cookie := devLogin(t, ctx, stack.BaseURL, "dev@local", "letmein")

	mcpURL := stack.BaseURL + "/mcp"
	mint := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "portal-project-tasks-fullstack",
	})
	bearer, _ := mint["key"].(string)
	if bearer == "" {
		t.Fatalf("agent_apikey_create returned no key: %+v", mint)
	}

	proj := callAPIv1(t, ctx, stack.BaseURL, bearer, "project_add", map[string]any{
		"name": "fullstack-project-tasks",
	})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", proj)
	}

	// substrate_auditor is system-scope-seeded on every boot and
	// advertises delivers: contract:substrate_audit — passes the
	// capability check in task_add.
	agent := callAPIv1(t, ctx, stack.BaseURL, bearer, "document_get", map[string]any{
		"type": "agent", "name": "substrate_auditor",
	})
	agentID, _ := agent["id"].(string)
	if agentID == "" {
		t.Fatalf("document_get(agent substrate_auditor): %+v", agent)
	}

	story := callAPIv1(t, ctx, stack.BaseURL, bearer, "story_add", map[string]any{
		"project_id": projectID,
		"title":      "fullstack-task-shape",
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
	seedTask("enqueued candidate 1")
	seedTask("enqueued candidate 2")
	seedTask("enqueued candidate 3")

	// First claim → close it (outcome:success).
	firstClaim := callAPIv1(t, ctx, stack.BaseURL, bearer, "task_claim", map[string]any{
		"worker_id": "fullstack-worker-close",
	})
	closedID, _ := firstClaim["id"].(string)
	if closedID == "" {
		t.Fatalf("first claim: %+v", firstClaim)
	}
	closed := callAPIv1(t, ctx, stack.BaseURL, bearer, "task_update", map[string]any{
		"id":      closedID,
		"status":  "closed",
		"outcome": "success",
	})
	if got, _ := closed["status"].(string); got != "closed" {
		t.Fatalf("task_update did not close %s: %+v", closedID, closed)
	}

	// Second claim → leave as in-flight.
	secondClaim := callAPIv1(t, ctx, stack.BaseURL, bearer, "task_claim", map[string]any{
		"worker_id": "fullstack-worker-inflight",
	})
	inflightID, _ := secondClaim["id"].(string)
	if inflightID == "" {
		t.Fatalf("second claim: %+v", secondClaim)
	}

	// Third task remains enqueued.

	// ----- Browser drive -----
	browserCtx, cancelBrowser := browser.New(t, ctx)
	defer cancelBrowser()

	if err := browser.InstallCookie(browserCtx, stack.BaseURL, cookie.Name, cookie.Value); err != nil {
		t.Fatalf("install cookie: %v", err)
	}

	tasksURL := stack.BaseURL + "/projects/" + projectID + "/tasks"
	var html string
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(tasksURL),
		chromedp.WaitVisible(`[data-testid="project-tasks-page"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`[data-testid="project-task-row"]`, chromedp.ByQuery),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate %s: %v", tasksURL, err)
	}

	for _, want := range []string{
		// Layout + Alpine scope.
		`x-data="taskPanel"`,
		`data-testid="pane-enqueued"`,
		`data-testid="pane-in-flight"`,
		`data-testid="pane-closed"`,
		// Story-row-parity column headers.
		`>id<`,
		`>title<`,
		`>duration<`,
		`>status<`,
		`>updated<`,
		// Tag chips replacing the old role/iter/outcome columns.
		`class="task-row-tags"`,
		`class="tag-chip is-clickable"`,
		`data-tag="kind:work"`,
		`data-tag="outcome:success"`,
		// Duration cell.
		`class="duration-pill"`,
		// Detail row stub (collapsed by default, present in markup).
		`class="task-detail`,
		`>timing<`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("SSR body missing %q", want)
		}
	}

	// Click the closed task row → detail row should become visible.
	rowSel := fmt.Sprintf(`[data-testid="project-task-row"][data-task-id=%q]`, closedID)
	detailSel := fmt.Sprintf(`[data-testid="task-detail-%s"]`, closedID)
	if err := chromedp.Run(browserCtx,
		chromedp.WaitVisible(rowSel, chromedp.ByQuery),
		browser.JSClick(rowSel),
		chromedp.WaitVisible(detailSel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("row click + detail wait for %s: %v", closedID, err)
	}

	// Click the outcome:success tag chip → page navigates with the
	// token in ?q=. Wait for an artifact that only appears AFTER the
	// new page renders: the q=outcome:success filter matches no rows
	// (free-text against id+title+story+claimed-by haystack), so the
	// closed pane's empty-state marker shows up. WaitVisible on
	// pane-empty also avoids chromedp's "Cannot find context"
	// race when polling JS during a navigation tear-down.
	tagSel := `[data-tag="outcome:success"]`
	if err := chromedp.Run(browserCtx,
		browser.JSClick(tagSel),
		chromedp.WaitVisible(`[data-testid="pane-closed"] [data-testid="pane-empty"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("tag click + post-nav wait: %v", err)
	}
	var landedURL string
	if err := chromedp.Run(browserCtx, chromedp.Location(&landedURL)); err != nil {
		t.Fatalf("read location: %v", err)
	}
	if !strings.Contains(landedURL, "outcome") {
		t.Errorf("URL after tag click missing outcome token: %s", landedURL)
	}

	_ = inflightID
}

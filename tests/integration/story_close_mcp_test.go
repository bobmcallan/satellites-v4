package integration

// story_close_mcp_test.go — sty_b97dda00 slice 1.
//
// Drives the new mechanical story_close MCP verb end-to-end against a
// testcontainers Surreal + satellites server. Asserts:
//
//  - PASS: a chain with closed work + closed contract:story_review +
//    verdict:pass ledger row + all infrastructure-template fields
//    filled walks the story to status=done and appends a
//    kind:close-evidence ledger row.
//  - FAIL paths (AC3): story_review:absent, story_review:open,
//    story_review:fail, chain:open_work, template:<field>:missing.
//    Each path asserts NO mutation — story status unchanged + no
//    close-evidence row appended.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// storyCloseFixtures collects the per-test stable identity rows the
// chain-mint helpers need.
type storyCloseFixtures struct {
	baseURL          string
	mcpURL           string
	bearer           string
	projectID        string
	storyCloseAgent  string
	storyReviewAgent string
}

// TestStoryCloseMCP boots the full container stack once and runs the
// PASS + 5 FAIL subtests against fresh stories minted per case.
func TestStoryCloseMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers story_close test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:          "surrealdb/surrealdb:v3.0.0",
			ExposedPorts:   []string{"8000/tcp"},
			Cmd:            []string{"start", "--user", "root", "--pass", "root"},
			Networks:       []string{net.Name},
			NetworkAliases: map[string][]string{net.Name: {"surrealdb"}},
			WaitingFor:     wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("surreal: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	// SATELLITES_API_KEYS bearer flow mirrors TestStoryMCPRoundTrip.
	// The cookie-auth + agent_apikey_create flow used by
	// agent_apikey_test / api_parity_test currently surfaces a
	// pre-existing "tool 'agent_apikey_create' not found" error
	// against the testcontainers boot (independently reproducible on
	// origin/main); the API-key flow avoids that path entirely.
	const bearer = "key_story_close"
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"SATELLITES_DB_DSN":   "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": bearer,
			"SATELLITES_DOCS_DIR": "/app/docs",
		},
	})
	defer stop()

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, bearer)

	createResp := callAPIv1(t, ctx, baseURL, bearer, "project_add", map[string]any{
		"name": "story-close-test-project",
	})
	projectID, _ := createResp["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", createResp)
	}

	agentArr := callToolArray(t, ctx, mcpURL, bearer, "document_list", map[string]any{
		"type":  "agent",
		"limit": 50,
	})
	closeAgent := agentIDFromList(agentArr, "story_close_agent")
	reviewAgent := agentIDFromList(agentArr, "story_reviewer")
	if closeAgent == "" || reviewAgent == "" {
		t.Fatalf("agents not resolvable: close=%q review=%q (agents=%+v)", closeAgent, reviewAgent, agentArr)
	}

	ff := &storyCloseFixtures{
		baseURL:          baseURL,
		mcpURL:           mcpURL,
		bearer:           bearer,
		projectID:        projectID,
		storyCloseAgent:  closeAgent,
		storyReviewAgent: reviewAgent,
	}

	t.Run("PassPath", func(t *testing.T) { assertStoryClosePassPath(t, ctx, ff) })
	t.Run("FailStoryReviewAbsent", func(t *testing.T) { assertStoryCloseFailReviewAbsent(t, ctx, ff) })
	t.Run("FailStoryReviewOpen", func(t *testing.T) { assertStoryCloseFailReviewOpen(t, ctx, ff) })
	t.Run("FailStoryReviewFail", func(t *testing.T) { assertStoryCloseFailVerdictFail(t, ctx, ff) })
	t.Run("FailChainOpenWork", func(t *testing.T) { assertStoryCloseFailOpenWork(t, ctx, ff) })
	t.Run("FailTemplateFieldMissing", func(t *testing.T) { assertStoryCloseFailTemplateField(t, ctx, ff) })
}

// mintStoryForClose creates an infrastructure-category story and (when
// fillFields=true) writes the four template-required `done` fields.
// Returns the freshly-minted story id.
func mintStoryForClose(t *testing.T, ctx context.Context, ff *storyCloseFixtures, title string, fillFields bool) string {
	t.Helper()
	resp := callAPIv1(t, ctx, ff.baseURL, ff.bearer, "story_add", map[string]any{
		"project_id":          ff.projectID,
		"title":               title,
		"description":         "story_close integration fixture",
		"acceptance_criteria": "see plan",
		"category":            "infrastructure",
	})
	storyID, _ := resp["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add returned no id: %+v", resp)
	}
	if fillFields {
		fillTemplateFields(t, ctx, ff, storyID)
	}
	return storyID
}

// fillTemplateFields populates the four `done`-gating fields on the
// infrastructure template — scope is gated at in_progress but the
// other four (rollout_plan, fix_commit, regression_test_path,
// post_deploy_check) are gated at done. story_close's gate runs
// EvaluateTransition for done so all four must be present on PASS.
func fillTemplateFields(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) {
	t.Helper()
	_ = callTool(t, ctx, ff.mcpURL, ff.bearer, "story_update", map[string]any{
		"id": storyID,
		"fields": map[string]any{
			"scope":                "story_close integration test scope",
			"rollout_plan":         "additive primitive only; no migration",
			"fix_commit":           "tests/integration/story_close_mcp_test.go",
			"regression_test_path": "tests/integration/story_close_mcp_test.go",
			"post_deploy_check":    "Re-run TestStoryCloseMCP against the live instance.",
		},
	})
}

// addAndCloseWorkTask mints a published work task on the story and
// closes it. Returns the task id.
func addAndCloseWorkTask(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID, prompt string) string {
	t.Helper()
	resp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_add", map[string]any{
		"agent_id": ff.storyCloseAgent,
		"story_id": storyID,
		"prompt":   prompt,
		"action":   "contract:story_close",
	})
	tid, _ := resp["task_id"].(string)
	if tid == "" {
		t.Fatalf("task_add returned no task_id: %+v", resp)
	}
	closeResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_update", map[string]any{
		"id":      tid,
		"status":  "closed",
		"outcome": "success",
	})
	if got, _ := closeResp["status"].(string); got != "closed" {
		t.Fatalf("task_update did not close work task: %+v", closeResp)
	}
	return tid
}

// addPublishedWorkTask mints a published work task and returns its id
// WITHOUT closing it — used to drive the chain:open_work FAIL path.
func addPublishedWorkTask(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) string {
	t.Helper()
	resp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_add", map[string]any{
		"agent_id": ff.storyCloseAgent,
		"story_id": storyID,
		"prompt":   "open work for FAIL path",
		"action":   "contract:story_close",
	})
	tid, _ := resp["task_id"].(string)
	if tid == "" {
		t.Fatalf("task_add (open work) returned no task_id: %+v", resp)
	}
	return tid
}

// addReviewTask mints the kind=review task tagged contract:story_review
// and closes it when closeIt=true.
func addReviewTask(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string, closeIt bool) string {
	t.Helper()
	resp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_add", map[string]any{
		"agent_id": ff.storyReviewAgent,
		"story_id": storyID,
		"kind":     "review",
		"prompt":   "story_review for story_close integration test",
		"action":   "contract:story_review",
	})
	tid, _ := resp["task_id"].(string)
	if tid == "" {
		t.Fatalf("task_add (review) returned no task_id: %+v", resp)
	}
	if closeIt {
		closeResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_update", map[string]any{
			"id":      tid,
			"status":  "closed",
			"outcome": "success",
		})
		if got, _ := closeResp["status"].(string); got != "closed" {
			t.Fatalf("task_update did not close review task: %+v", closeResp)
		}
	}
	return tid
}

// appendVerdictRow appends a kind:verdict ledger row tagged with the
// review task id + the requested verdict (pass|fail).
func appendVerdictRow(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID, reviewTaskID, verdict string) {
	t.Helper()
	tags := []string{"kind:verdict", "task_id:" + reviewTaskID, "verdict:" + verdict}
	_ = callTool(t, ctx, ff.mcpURL, ff.bearer, "ledger_append", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"type":       "decision",
		"content":    `{"rationale":"integration-test verdict"}`,
		"tags":       tags,
	})
}

// callStoryClose invokes the new MCP verb and returns the decoded
// response body.
func callStoryClose(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) map[string]any {
	t.Helper()
	return callTool(t, ctx, ff.mcpURL, ff.bearer, "story_close", map[string]any{
		"story_id": storyID,
	})
}

// gapCodes extracts the gap.code values from a story_close response.
func gapCodes(resp map[string]any) []string {
	raw, _ := resp["gaps"].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := m["code"].(string); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// gapMatchPrefix returns true when any gap code in resp starts with
// prefix. Used for `template:<field>:missing` matching where the field
// name is template-dependent.
func gapMatchPrefix(resp map[string]any, prefix string) bool {
	for _, c := range gapCodes(resp) {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// assertStoryNotClosed verifies no mutation: status NOT done AND no
// kind:close-evidence ledger row exists on the story.
func assertStoryNotClosed(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) {
	t.Helper()
	getResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "story_get", map[string]any{"id": storyID})
	st, _ := getResp["story"].(map[string]any)
	if status, _ := st["status"].(string); status == "done" {
		t.Errorf("story walked to done on FAIL: status=%q", status)
	}
	rawResp := callToolRaw(t, ctx, ff.mcpURL, ff.bearer, "ledger_list", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"tags":       []any{"kind:close-evidence"},
		"limit":      10,
	})
	if isToolError(rawResp) {
		t.Fatalf("ledger_list isError: %+v", rawResp)
	}
	text := extractToolText(t, rawResp)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return
	}
	for _, r := range rows {
		tags, _ := r["tags"].([]any)
		for _, tag := range tags {
			if s, _ := tag.(string); s == "kind:close-evidence" {
				t.Errorf("close-evidence row appended on FAIL path: %+v", r)
			}
		}
	}
}

func assertStoryClosePassPath(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	storyID := mintStoryForClose(t, ctx, ff, "pass-path", true)
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop work for PASS")
	reviewID := addReviewTask(t, ctx, ff, storyID, true)
	appendVerdictRow(t, ctx, ff, storyID, reviewID, "pass")

	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "pass" {
		t.Fatalf("status = %v, want pass; resp=%+v", resp["status"], resp)
	}
	if id, _ := resp["evidence_id"].(string); id == "" {
		t.Errorf("evidence_id missing on pass: %+v", resp)
	}
	if got, _ := resp["story_status"].(string); got != "done" {
		t.Errorf("story_status = %q, want done", got)
	}
	getResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "story_get", map[string]any{"id": storyID})
	st, _ := getResp["story"].(map[string]any)
	if got, _ := st["status"].(string); got != "done" {
		t.Errorf("story_get status = %q, want done after PASS", got)
	}
	rawResp := callToolRaw(t, ctx, ff.mcpURL, ff.bearer, "ledger_list", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"tags":       []any{"kind:close-evidence"},
		"limit":      10,
	})
	text := extractToolText(t, rawResp)
	var rows []map[string]any
	_ = json.Unmarshal([]byte(text), &rows)
	if len(rows) == 0 {
		t.Errorf("expected at least one kind:close-evidence row, got none")
	}
}

func assertStoryCloseFailReviewAbsent(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	storyID := mintStoryForClose(t, ctx, ff, "fail-review-absent", true)
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop work; no review task")

	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "fail" {
		t.Fatalf("status = %v, want fail; resp=%+v", resp["status"], resp)
	}
	codes := gapCodes(resp)
	found := false
	for _, c := range codes {
		if c == "story_review:absent" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected story_review:absent in gaps, got %+v", codes)
	}
	assertStoryNotClosed(t, ctx, ff, storyID)
}

func assertStoryCloseFailReviewOpen(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	storyID := mintStoryForClose(t, ctx, ff, "fail-review-open", true)
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop work; review left open")
	_ = addReviewTask(t, ctx, ff, storyID, false) // leave OPEN

	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "fail" {
		t.Fatalf("status = %v, want fail; resp=%+v", resp["status"], resp)
	}
	found := false
	for _, c := range gapCodes(resp) {
		if c == "story_review:open" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected story_review:open in gaps, got %+v", gapCodes(resp))
	}
	assertStoryNotClosed(t, ctx, ff, storyID)
}

func assertStoryCloseFailVerdictFail(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	storyID := mintStoryForClose(t, ctx, ff, "fail-verdict-fail", true)
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop work; review verdict fail")
	reviewID := addReviewTask(t, ctx, ff, storyID, true)
	appendVerdictRow(t, ctx, ff, storyID, reviewID, "fail")

	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "fail" {
		t.Fatalf("status = %v, want fail; resp=%+v", resp["status"], resp)
	}
	found := false
	for _, c := range gapCodes(resp) {
		if c == "story_review:fail" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected story_review:fail in gaps, got %+v", gapCodes(resp))
	}
	assertStoryNotClosed(t, ctx, ff, storyID)
}

func assertStoryCloseFailOpenWork(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	storyID := mintStoryForClose(t, ctx, ff, "fail-open-work", true)
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "first work task")
	_ = addPublishedWorkTask(t, ctx, ff, storyID) // left open
	reviewID := addReviewTask(t, ctx, ff, storyID, true)
	appendVerdictRow(t, ctx, ff, storyID, reviewID, "pass")

	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "fail" {
		t.Fatalf("status = %v, want fail; resp=%+v", resp["status"], resp)
	}
	found := false
	for _, c := range gapCodes(resp) {
		if c == "chain:open_work" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected chain:open_work in gaps, got %+v", gapCodes(resp))
	}
	assertStoryNotClosed(t, ctx, ff, storyID)
}

func assertStoryCloseFailTemplateField(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	storyID := mintStoryForClose(t, ctx, ff, "fail-template-field", false) // do NOT fill template fields
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop work; missing template fields")
	reviewID := addReviewTask(t, ctx, ff, storyID, true)
	appendVerdictRow(t, ctx, ff, storyID, reviewID, "pass")

	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "fail" {
		t.Fatalf("status = %v, want fail; resp=%+v", resp["status"], resp)
	}
	if !gapMatchPrefix(resp, "template:") {
		t.Errorf("expected template:<field>:missing in gaps, got %+v", gapCodes(resp))
	}
	assertStoryNotClosed(t, ctx, ff, storyID)
}

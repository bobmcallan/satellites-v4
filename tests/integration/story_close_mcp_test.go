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
	baseURL              string
	mcpURL               string
	bearer               string
	projectID            string
	developerAgent       string
	storyReviewAgent     string
	developmentReviewer  string
	releaserAgent        string
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

	// developer_agent + releaser_agent are workspace-tier (wksp_5b3257d1)
	// in the real seed. The api-key bearer here is admin only of the
	// auto-minted system workspace, so those rows aren't reachable via
	// document_list. Mint system-scope fixture agents that deliver
	// contract:develop / contract:commit / contract:merge_to_main so
	// the chain-mint helpers can author tasks against them.
	developerAgent := ensureSystemAgentFixture(t, ctx, baseURL, bearer, "developer_agent_fixture",
		[]string{"contract:plan", "contract:develop"}, nil)
	releaserAgent := ensureSystemAgentFixture(t, ctx, baseURL, bearer, "releaser_agent_fixture",
		[]string{"contract:commit", "contract:merge_to_main"}, nil)

	agentArr := callToolArray(t, ctx, mcpURL, bearer, "document_list", map[string]any{
		"type":  "agent",
		"limit": 50,
	})
	reviewAgent := agentIDFromList(agentArr, "story_reviewer")
	devReviewer := agentIDFromList(agentArr, "development_reviewer")
	if developerAgent == "" || reviewAgent == "" {
		t.Fatalf("agents not resolvable: developer=%q review=%q (agents=%+v)", developerAgent, reviewAgent, agentArr)
	}

	ff := &storyCloseFixtures{
		baseURL:             baseURL,
		mcpURL:              mcpURL,
		bearer:              bearer,
		projectID:           projectID,
		developerAgent:      developerAgent,
		storyReviewAgent:    reviewAgent,
		developmentReviewer: devReviewer,
		releaserAgent:       releaserAgent,
	}

	t.Run("PassPath", func(t *testing.T) { assertStoryClosePassPath(t, ctx, ff) })
	t.Run("FailStoryReviewAbsent", func(t *testing.T) { assertStoryCloseFailReviewAbsent(t, ctx, ff) })
	t.Run("FailStoryReviewOpen", func(t *testing.T) { assertStoryCloseFailReviewOpen(t, ctx, ff) })
	t.Run("FailStoryReviewFail", func(t *testing.T) { assertStoryCloseFailVerdictFail(t, ctx, ff) })
	t.Run("FailChainOpenWork", func(t *testing.T) { assertStoryCloseFailOpenWork(t, ctx, ff) })
	t.Run("FailTemplateFieldMissing", func(t *testing.T) { assertStoryCloseFailTemplateField(t, ctx, ff) })
	t.Run("LifecycleE2E", func(t *testing.T) { assertStoryCloseLifecycleE2E(t, ctx, ff) })
}

// TestStoryCloseLifecycleE2E (AC6) is a separate top-level entry point
// that exercises the same new-chain E2E flow against a fresh
// testcontainers stack. Implementation lives in
// assertStoryCloseLifecycleE2E (shared with the LifecycleE2E subtest of
// TestStoryCloseMCP so the heavy container boot amortises in normal
// CI). The standalone form lets `go test -run TestStoryCloseLifecycleE2E`
// drive the new-chain assertion in isolation.
func TestStoryCloseLifecycleE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers TestStoryCloseLifecycleE2E in -short mode")
	}
	// Reuse the same boot path as TestStoryCloseMCP. Each call mints
	// its own network + surreal + server, so this test runs
	// stand-alone.
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

	const bearer = "key_lifecycle_e2e"
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
		"name": "story-close-lifecycle-e2e",
	})
	projectID, _ := createResp["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", createResp)
	}

	developerAgent := ensureSystemAgentFixture(t, ctx, baseURL, bearer, "developer_agent_fixture",
		[]string{"contract:plan", "contract:develop"}, nil)
	releaserAgent := ensureSystemAgentFixture(t, ctx, baseURL, bearer, "releaser_agent_fixture",
		[]string{"contract:commit", "contract:merge_to_main"}, nil)

	agentArr := callToolArray(t, ctx, mcpURL, bearer, "document_list", map[string]any{
		"type":  "agent",
		"limit": 50,
	})
	reviewAgent := agentIDFromList(agentArr, "story_reviewer")
	devReviewer := agentIDFromList(agentArr, "development_reviewer")
	if developerAgent == "" || reviewAgent == "" || releaserAgent == "" {
		t.Fatalf("agents not resolvable: developer=%q review=%q releaser=%q",
			developerAgent, reviewAgent, releaserAgent)
	}

	ff := &storyCloseFixtures{
		baseURL:             baseURL,
		mcpURL:              mcpURL,
		bearer:              bearer,
		projectID:           projectID,
		developerAgent:      developerAgent,
		storyReviewAgent:    reviewAgent,
		developmentReviewer: devReviewer,
		releaserAgent:       releaserAgent,
	}
	assertStoryCloseLifecycleE2E(t, ctx, ff)
}

// ensureSystemAgentFixture POSTs /api/v1/agent/add for a system-scope
// agent that delivers / reviews the supplied actions, and returns the
// created agent id. The api-key bearer is admin of the system
// workspace (see cmd/satellites-server/main.go), so the row is
// reachable through the same caller's subsequent document_list calls.
// The fixture name is used as the visible doc name; pass distinct
// names per test so the row is unambiguous.
func ensureSystemAgentFixture(t *testing.T, ctx context.Context, baseURL, bearer, name string, delivers, reviews []string) string {
	t.Helper()
	settings := map[string]any{
		"permission_patterns": []string{"Read:**"},
	}
	if len(delivers) > 0 {
		settings["delivers"] = delivers
	}
	if len(reviews) > 0 {
		settings["reviews"] = reviews
	}
	structured, _ := json.Marshal(settings)
	resp := callAPIv1(t, ctx, baseURL, bearer, "agent_add", map[string]any{
		"scope":      "system",
		"name":       name,
		"body":       "fixture agent for tests/integration/story_close_mcp_test.go",
		"structured": string(structured),
	})
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("agent_add(%q) returned no id: %+v", name, resp)
	}
	return id
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
		"agent_id": ff.developerAgent,
		"story_id": storyID,
		"prompt":   prompt,
		"action":   "contract:develop",
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
		"agent_id": ff.developerAgent,
		"story_id": storyID,
		"prompt":   "open work for FAIL path",
		"action":   "contract:develop",
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

// appendReleaseEvidenceRow appends a kind:release-evidence ledger row
// tagged with pushed_sha:<sha>. The mechanical story_close verb's
// deploy:behind gate consults this row's pushed_sha against pprod's
// satellites_info commit; the integration harness's pprod reports
// `unknown` (config.GitCommit default at test time), so the seeded
// sha matches.
func appendReleaseEvidenceRow(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID, pushedSHA string) {
	t.Helper()
	tags := []string{
		"kind:release-evidence",
		"phase:merge_to_main",
		"pushed_sha:" + pushedSHA,
	}
	_ = callTool(t, ctx, ff.mcpURL, ff.bearer, "ledger_append", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"type":       "evidence",
		"content":    "## release-evidence stub for integration test",
		"tags":       tags,
	})
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
// response body. Passes pprod_commit:"unknown" because the
// integration container reports config.GitCommit="unknown" and the
// release-evidence rows seeded above carry pushed_sha:unknown — this
// satisfies the multi-tenant deploy:behind resolver (sty_224774f0)
// without depending on SATELLITES_SELF_PROJECT_ID being set on the
// per-test container, since each subtest mints its own project_id.
func callStoryClose(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) map[string]any {
	t.Helper()
	return callTool(t, ctx, ff.mcpURL, ff.bearer, "story_close", map[string]any{
		"story_id":     storyID,
		"pprod_commit": "unknown",
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
	// AC5 — seed a kind:release-evidence row whose pushed_sha matches
	// the container's reported satellites_info commit so the
	// deploy:behind gate passes. config.GitCommit default at test
	// time is "unknown".
	appendReleaseEvidenceRow(t, ctx, ff, storyID, "unknown")

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

// closeWorkTask mints a work task and closes it outcome=success. The
// chain helpers below use this to walk each lifecycle phase.
func closeWorkTask(t *testing.T, ctx context.Context, ff *storyCloseFixtures, agentID, storyID, action, kind, prompt string) string {
	t.Helper()
	args := map[string]any{
		"agent_id": agentID,
		"story_id": storyID,
		"prompt":   prompt,
		"action":   action,
	}
	if kind != "" {
		args["kind"] = kind
	}
	resp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_add", args)
	tid, _ := resp["task_id"].(string)
	if tid == "" {
		t.Fatalf("task_add(%s/%s) returned no task_id: %+v", action, kind, resp)
	}
	closeResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "task_update", map[string]any{
		"id":      tid,
		"status":  "closed",
		"outcome": "success",
	})
	if got, _ := closeResp["status"].(string); got != "closed" {
		t.Fatalf("task_update(%s) did not close: %+v", tid, closeResp)
	}
	return tid
}

// appendVerdictRowForTask is closeWorkTask's verdict-shaped sibling
// (kind:verdict ledger row tagged with the just-closed review task).
// Distinct from `appendVerdictRow` (used by the FAIL-path subtests)
// because the lifecycle E2E path needs both the verdict:accepted shape
// (for develop_review) and verdict:pass (for story_review pre-ship).
func appendVerdictRowForTask(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID, taskID, verdict string) {
	t.Helper()
	_ = callTool(t, ctx, ff.mcpURL, ff.bearer, "ledger_append", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"type":       "decision",
		"content":    `{"rationale":"lifecycle-e2e verdict"}`,
		"tags":       []string{"kind:verdict", "task_id:" + taskID, "verdict:" + verdict},
	})
}

// assertStoryCloseLifecycleE2E (AC6) walks the full new chain end-to-
// end against the running testcontainers stack, then calls the
// mechanical `story_close` MCP verb and asserts the PASS contract:
// status=pass + non-empty evidence_id + story_get status=done +
// ledger contains a kind:close-evidence row.
//
// Chain phase ordering walked here mirrors the new workflow:
//
//	plan          (work, closed)
//	develop       (work, closed)
//	develop_review(review on contract:develop, closed + verdict:accepted)
//	story_review  (work on contract:story_review, closed + verdict:pass)
//	push          (work, closed)
//	merge_to_main (work, closed)
//	push (main)   (work, closed)
//
// Each step uses task_add with the matching agent + action +
// (where applicable) kind, then task_update(status=closed,
// outcome=success). The review steps additionally append a
// kind:verdict ledger row so the mechanical close verb's gate sees
// the verdict tag.
func assertStoryCloseLifecycleE2E(t *testing.T, ctx context.Context, ff *storyCloseFixtures) {
	t.Helper()
	storyID := mintStoryForClose(t, ctx, ff, "lifecycle-e2e", true)

	// plan
	_ = closeWorkTask(t, ctx, ff, ff.developerAgent, storyID, "contract:plan", "", "plan for lifecycle-e2e")

	// develop + develop_review (accepted).
	// development_reviewer reviews contract:develop (story_reviewer
	// does not, so use the matching reviewer to avoid
	// agent_cannot_review).
	devTaskID := closeWorkTask(t, ctx, ff, ff.developerAgent, storyID, "contract:develop", "", "develop for lifecycle-e2e")
	devReviewAgent := ff.developmentReviewer
	if devReviewAgent == "" {
		devReviewAgent = ff.storyReviewAgent
	}
	devReviewID := closeWorkTask(t, ctx, ff, devReviewAgent, storyID, "contract:develop", "review", "develop_review for lifecycle-e2e")
	appendVerdictRowForTask(t, ctx, ff, storyID, devReviewID, "accepted")
	_ = devTaskID

	// story_review (pre-ship gate, kind=review, verdict:pass — same
	// shape the slice-1 PASS subtest exercises so the mechanical
	// close verb's existing gate accepts the chain).
	srReviewID := closeWorkTask(t, ctx, ff, ff.storyReviewAgent, storyID, "contract:story_review", "review", "story_review for lifecycle-e2e")
	appendVerdictRowForTask(t, ctx, ff, storyID, srReviewID, "pass")

	// commit + merge_to_main (execution-shape, no reviewer). After
	// the lifecycle restructure (sty_ab0b47d6), push is renamed to
	// commit and the second push (push_main) folds into the atomic
	// merge_to_main release operation. Use releaser_agent when
	// available, else developer_agent for fixture-only chain shaping.
	releaser := ff.releaserAgent
	if releaser == "" {
		releaser = ff.developerAgent
	}
	_ = closeWorkTask(t, ctx, ff, releaser, storyID, "contract:commit", "", "commit for lifecycle-e2e")
	_ = closeWorkTask(t, ctx, ff, releaser, storyID, "contract:merge_to_main", "", "merge_to_main for lifecycle-e2e")
	// AC5 — release-evidence row authored by the merge_to_main
	// contract at release time. Seed with pushed_sha matching pprod's
	// reported commit (config.GitCommit default "unknown" in tests).
	appendReleaseEvidenceRow(t, ctx, ff, storyID, "unknown")

	// Mechanical close verb.
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
		t.Errorf("story_get status = %q, want done after lifecycle-e2e PASS", got)
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
		t.Errorf("lifecycle-e2e: expected at least one kind:close-evidence row, got none")
	}
}

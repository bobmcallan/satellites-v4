package integration

// story_close_template_field_test.go — sty_4885dc89 anchor.
//
// Locks the wire-level contract for the `kind:template-field:<name>`
// carrier mechanism: a develop slice that appends a carrier row
// against the parent story populates `story.fields.<name>` at
// `story_close` time, without any operator-side
// `story_update(fields=…)` call between the develop close and the
// story_close. The substrate's close-gate roll-up is the only writer.

import (
	"context"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestStoryCloseTemplateFieldCarrier boots the full container stack
// once, mints an improvement-category story whose template requires
// fix_commit / regression_test_path / before_after at the done
// transition, appends three carrier rows via `ledger_append`, calls
// `story_close`, and asserts:
//
//   - story_close returns status=pass with non-empty evidence_id.
//   - story_get(id).fields.fix_commit == carrier row's content.
//   - The same for regression_test_path and before_after.
//   - No story_update(fields=…) MCP call appears in the test path
//     between the develop close and story_close (the test does not
//     call it; the substrate is the sole writer).
func TestStoryCloseTemplateFieldCarrier(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers story_close template-field carrier test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()

	const bearer = "key_template_field_carrier"
	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv:     map[string]string{"SATELLITES_API_KEYS": bearer},
		SkipDocsMount: true,
	})
	defer stack.Stop()
	baseURL := stack.BaseURL

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, bearer)

	createResp := callAPIv1(t, ctx, baseURL, bearer, "project_add", map[string]any{
		"name": "story-close-template-field-carrier",
	})
	projectID, _ := createResp["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", createResp)
	}

	developerAgent := ensureSystemAgentFixture(t, ctx, baseURL, bearer, "developer_agent_carrier_fixture",
		[]string{"contract:plan", "contract:develop"}, nil)

	agentArr := callToolArray(t, ctx, mcpURL, bearer, "document_list", map[string]any{
		"type":  "agent",
		"limit": 50,
	})
	reviewAgent := agentIDFromList(agentArr, "story_reviewer")
	if developerAgent == "" || reviewAgent == "" {
		t.Fatalf("agents not resolvable: developer=%q review=%q", developerAgent, reviewAgent)
	}

	ff := &storyCloseFixtures{
		baseURL:          baseURL,
		mcpURL:           mcpURL,
		bearer:           bearer,
		projectID:        projectID,
		developerAgent:   developerAgent,
		storyReviewAgent: reviewAgent,
	}

	// Mint an improvement-category story carrying only the in-progress
	// hook field (motivation). The three done-gated fields
	// (before_after, fix_commit, regression_test_path) are left empty
	// — the carrier rows below must populate them via the substrate's
	// close-gate roll-up.
	storyResp := callAPIv1(t, ctx, ff.baseURL, ff.bearer, "story_add", map[string]any{
		"project_id":          ff.projectID,
		"title":               "template-field carrier anchor",
		"description":         "sty_4885dc89 anchor: develop close emits carrier rows; story_close rolls them up",
		"acceptance_criteria": "see plan ldg_cbca56f9",
		"category":            "improvement",
		"fields": map[string]any{
			"motivation": "lock the kind:template-field:<name> wire contract",
		},
	})
	storyID, _ := storyResp["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add returned no id: %+v", storyResp)
	}

	// Develop work + story_review chain.
	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop for template-field carrier anchor")
	reviewID := addReviewTask(t, ctx, ff, storyID, true)
	appendVerdictRow(t, ctx, ff, storyID, reviewID, "pass")
	appendReleaseEvidenceRow(t, ctx, ff, storyID, "unknown")

	// Carrier rows — the wire-level contract under test. Each row's
	// `content` is the value the substrate must project onto
	// story.fields.<name>.
	const fixCommitValue = "abc1234def567"
	const regressionTestPath = "tests/integration/story_close_template_field_test.go"
	const beforeAfterValue = "before: manual story_field_set; after: carrier roll-up"

	appendTemplateFieldRow(t, ctx, ff, storyID, "fix_commit", fixCommitValue)
	appendTemplateFieldRow(t, ctx, ff, storyID, "regression_test_path", regressionTestPath)
	appendTemplateFieldRow(t, ctx, ff, storyID, "before_after", beforeAfterValue)

	// story_close — the substrate's close-gate roll-up must populate
	// the three done-gated fields before the template gap-check runs,
	// so the close passes without any operator-side story_update call.
	resp := callStoryClose(t, ctx, ff, storyID)
	if status, _ := resp["status"].(string); status != "pass" {
		t.Fatalf("story_close status = %v, want pass; resp=%+v", resp["status"], resp)
	}
	if id, _ := resp["evidence_id"].(string); id == "" {
		t.Errorf("evidence_id missing on pass: %+v", resp)
	}

	// Verify fields landed on the story row — story_get is the
	// audit-of-record for substrate-written values.
	getResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "story_get", map[string]any{"id": storyID})
	st, _ := getResp["story"].(map[string]any)
	fields, _ := st["fields"].(map[string]any)
	if got, _ := fields["fix_commit"].(string); got != fixCommitValue {
		t.Errorf("fields.fix_commit = %q, want %q", got, fixCommitValue)
	}
	if got, _ := fields["regression_test_path"].(string); got != regressionTestPath {
		t.Errorf("fields.regression_test_path = %q, want %q", got, regressionTestPath)
	}
	if got, _ := fields["before_after"].(string); got != beforeAfterValue {
		t.Errorf("fields.before_after = %q, want %q", got, beforeAfterValue)
	}
}

// appendTemplateFieldRow appends one kind:template-field:<name>
// carrier row scoped to the named story. Mirrors the row shape the
// develop contract's "Template field carrier" section documents.
func appendTemplateFieldRow(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID, field, value string) {
	t.Helper()
	tags := []string{"kind:template-field:" + field, "story_id:" + storyID}
	_ = callTool(t, ctx, ff.mcpURL, ff.bearer, "ledger_append", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"type":       "evidence",
		"content":    value,
		"tags":       tags,
	})
}

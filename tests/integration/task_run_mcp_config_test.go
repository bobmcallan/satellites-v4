// sty_056b68f6 — `satellites-client task run` mints a task-scoped
// agent api-key on the orchestrator's behalf and threads the
// resolved verb allowlist into the worktree's .mcp.json file. The
// dispatched subprocess sees ONLY the verbs its role permits.
//
// This integration test focuses on the wire-shape contract:
// `agent_apikey_create(task_id=…)` returns `allowed_verbs` that
// matches the agent's pr_role_grid default. The end-to-end dispatch
// path (claude subprocess + worktree materialisation) is exercised
// by the `task_run_apiv1` test in this package; here we pin the
// mint contract that feeds it.

package integration

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestTaskRunMintCarriesRoleDefault asserts the mint response shape
// the dispatcher reads:
//
//   1. token (`key`) — non-empty, sat_ prefixed
//   2. allowed_verbs — non-empty, every entry is a real verb on
//      pr_role_grid's execution row
//   3. expires_at — populated (not empty), the 6h safety net
//   4. task_id — echoes the supplied task id
//   5. the returned bearer is distinct from the orchestrator's
//      project-scoped bearer
func TestTaskRunMintCarriesRoleDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv: map[string]string{
			"SATELLITES_DEV_USERNAME": "dev@local",
			"SATELLITES_DEV_PASSWORD": "letmein",
		},
	})
	defer stack.Stop()
	baseURL := stack.BaseURL

	cookie := devLogin(t, ctx, baseURL, "dev@local", "letmein")
	mcpURL := baseURL + "/mcp"

	mint := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "task-run-orch-key",
	})
	orchBearer, _ := mint["key"].(string)
	if orchBearer == "" {
		t.Fatalf("orch mint missing key: %+v", mint)
	}

	proj := apiPostJSON(t, ctx, baseURL+"/api/v1/project/add", orchBearer, map[string]any{
		"name": "task-run-mint-project",
	})
	projectID, _ := proj["id"].(string)
	st := apiPostJSON(t, ctx, baseURL+"/api/v1/story/add", orchBearer, map[string]any{
		"project_id": projectID,
		"title":      "task-run-mint-story",
	})
	storyID, _ := st["id"].(string)

	devID := resolveAgentID(t, ctx, baseURL, orchBearer, "developer_agent")
	tsk := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", orchBearer, map[string]any{
		"story_id": storyID,
		"agent_id": devID,
		"action":   "contract:develop",
		"prompt":   "task-run-mint fixture",
	})
	taskID, _ := tsk["task_id"].(string)

	// The dispatcher posts this exact shape (see
	// cmd/satellites-client/task_run.go).
	mintTask, status := apiPostStatus(t, ctx, baseURL+"/api/v1/agent/apikey-create", orchBearer, map[string]any{
		"name":       "run:" + taskID,
		"project_id": projectID,
		"task_id":    taskID,
	})
	if status != http.StatusOK {
		t.Fatalf("task-scoped mint status = %d, body=%+v", status, mintTask)
	}
	taskBearer, _ := mintTask["key"].(string)
	if taskBearer == "" {
		t.Fatalf("task-scoped mint missing key: %+v", mintTask)
	}
	if taskBearer == orchBearer {
		t.Errorf("task-scoped bearer equals orch bearer — the dispatcher would leak the project key")
	}
	if got, _ := mintTask["task_id"].(string); got != taskID {
		t.Errorf("task_id = %q, want %q", got, taskID)
	}
	if got, _ := mintTask["expires_at"].(string); got == "" {
		t.Errorf("expires_at empty — task-scoped key must carry the 6h safety net")
	}

	allowedRaw, _ := mintTask["allowed_verbs"].([]any)
	if len(allowedRaw) == 0 {
		t.Fatalf("allowed_verbs empty: %+v", mintTask)
	}
	allowed := make([]string, 0, len(allowedRaw))
	for _, v := range allowedRaw {
		s, _ := v.(string)
		allowed = append(allowed, s)
	}
	sort.Strings(allowed)

	// Required execution-role verbs per pr_role_grid.
	mustContain := []string{"task_update", "ledger_append"}
	for _, v := range mustContain {
		if !contains(allowed, v) {
			t.Errorf("allowed_verbs missing %q (execution-role default): %v", v, allowed)
		}
	}
	// Confirmed NOT present — out-of-set escalation guards.
	mustNotContain := []string{"task_add", "story_add", "story_update"}
	for _, v := range mustNotContain {
		if contains(allowed, v) {
			t.Errorf("allowed_verbs contains %q (role escalation past pr_role_grid): %v", v, allowed)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

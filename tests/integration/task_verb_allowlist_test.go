// sty_056b68f6 — per-task MCP verb allowlist enforced at the wire.
//
// The orchestrator's project-scoped apikey can call every verb. A
// task-scoped apikey minted with `agent_apikey_create(task_id=…)`
// is clamped to the task's agent role default per pr_role_grid.
// Out-of-set calls return 403 + the
// `{"error":"verb_not_in_allowlist","verb":"<v>","allowed":[...]}`
// wire envelope; in-set calls succeed normally.

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestTaskVerbAllowlist_ExecutionRow asserts the execution-role
// matrix:
//
//  - in-set: task_update(self → closed), ledger_append(own task tag)
//    → 200.
//  - out-of-set: task_add, story_add, story_update, principle_list,
//    agent_list, contract_list → 403 + envelope.
//
// (Subset of AC2 matrix per review-criteria.md.)
func TestTaskVerbAllowlist_ExecutionRow(t *testing.T) {
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

	// Orchestrator-scoped bearer (project key).
	mint := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "verb-gate-orchestrator",
	})
	orchBearer, _ := mint["key"].(string)
	if orchBearer == "" {
		t.Fatalf("orchestrator key not minted: %+v", mint)
	}

	// Bootstrap a project + story + task so the task-scoped mint has
	// a task to attach to. developer_agent's role: execution (per
	// pr_role_grid).
	proj := apiPostJSON(t, ctx, baseURL+"/api/v1/project/add", orchBearer, map[string]any{
		"name": "verb-gate-project",
	})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project/add: %+v", proj)
	}

	st := apiPostJSON(t, ctx, baseURL+"/api/v1/story/add", orchBearer, map[string]any{
		"project_id": projectID,
		"title":      "verb-gate-story",
	})
	storyID, _ := st["id"].(string)
	if storyID == "" {
		t.Fatalf("story/add: %+v", st)
	}

	// Resolve developer_agent.id via document_list.
	devID := resolveAgentID(t, ctx, baseURL, orchBearer, "developer_agent")

	tsk := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", orchBearer, map[string]any{
		"story_id": storyID,
		"agent_id": devID,
		"action":   "contract:develop",
		"prompt":   "verb-gate fixture task",
	})
	taskID, _ := tsk["task_id"].(string)
	if taskID == "" {
		t.Fatalf("task/add: %+v", tsk)
	}

	// Task-scoped mint. Role default for execution is taken from the
	// server-side map. Expect the response to carry allowed_verbs.
	mintTask := apiPostJSON(t, ctx, baseURL+"/api/v1/agent/apikey-create", orchBearer, map[string]any{
		"name":       "run:" + taskID,
		"project_id": projectID,
		"task_id":    taskID,
	})
	execBearer, _ := mintTask["key"].(string)
	if execBearer == "" {
		t.Fatalf("task-scoped mint missing key: %+v", mintTask)
	}
	if allowed, _ := mintTask["allowed_verbs"].([]any); len(allowed) == 0 {
		t.Fatalf("task-scoped mint missing allowed_verbs: %+v", mintTask)
	}

	// In-set: ledger_append (own task tag) → 200.
	allowedResp, status := apiPostStatus(t, ctx, baseURL+"/api/v1/ledger/append", execBearer, map[string]any{
		"project_id": projectID,
		"type":       "evidence",
		"content":    "execution-role in-set verb",
		"tags":       []string{"task_id:" + taskID, "kind:test"},
	})
	if status != http.StatusOK {
		t.Errorf("ledger/append (in-set) status = %d, body=%v", status, allowedResp)
	}

	// Out-of-set: task_add → 403 + envelope.
	deniedBody, status := apiPostStatus(t, ctx, baseURL+"/api/v1/task/add", execBearer, map[string]any{
		"story_id": storyID,
		"agent_id": devID,
		"action":   "contract:develop",
		"prompt":   "execution attempts to author a task",
	})
	if status != http.StatusForbidden {
		t.Errorf("task/add by execution role status = %d, want 403; body=%v", status, deniedBody)
	}
	assertVerbDeniedEnvelope(t, deniedBody, "task_add")

	// Out-of-set: story_add → 403.
	deniedBody, status = apiPostStatus(t, ctx, baseURL+"/api/v1/story/add", execBearer, map[string]any{
		"project_id": projectID,
		"title":      "execution attempts to author a story",
	})
	if status != http.StatusForbidden {
		t.Errorf("story/add by execution role status = %d, want 403; body=%v", status, deniedBody)
	}
	assertVerbDeniedEnvelope(t, deniedBody, "story_add")

	// Cross-role escalation: mint with allowed_verbs=[story_add] over
	// the task-scoped bearer — AC6 / pr_no_unrequested_compat. The
	// task-scoped bearer can't even call agent_apikey_create (not in
	// execution role), so the gate fires before subset validation.
	deniedBody, status = apiPostStatus(t, ctx, baseURL+"/api/v1/agent/apikey-create", execBearer, map[string]any{
		"name":          "escalate",
		"project_id":    projectID,
		"task_id":       taskID,
		"allowed_verbs": []string{"story_add"},
	})
	if status != http.StatusForbidden {
		t.Errorf("cross-role mint via execution bearer status = %d, want 403; body=%v", status, deniedBody)
	}
	assertVerbDeniedEnvelope(t, deniedBody, "agent_apikey_create")
}

// TestTaskVerbAllowlist_ReviewRow asserts the review-role matrix's
// in-set verbs (story_get, task_walk, ledger_list) succeed and an
// out-of-set verb (task_add) is rejected.
func TestTaskVerbAllowlist_ReviewRow(t *testing.T) {
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
		"name": "verb-gate-orch-review",
	})
	orchBearer, _ := mint["key"].(string)

	proj := apiPostJSON(t, ctx, baseURL+"/api/v1/project/add", orchBearer, map[string]any{
		"name": "verb-gate-review-project",
	})
	projectID, _ := proj["id"].(string)

	st := apiPostJSON(t, ctx, baseURL+"/api/v1/story/add", orchBearer, map[string]any{
		"project_id": projectID,
		"title":      "verb-gate-review-story",
	})
	storyID, _ := st["id"].(string)

	revID := resolveAgentID(t, ctx, baseURL, orchBearer, "development_reviewer")
	tsk := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", orchBearer, map[string]any{
		"story_id": storyID,
		"agent_id": revID,
		"kind":     "review",
		"action":   "contract:develop",
		"prompt":   "review-row fixture task",
	})
	taskID, _ := tsk["task_id"].(string)

	mintTask := apiPostJSON(t, ctx, baseURL+"/api/v1/agent/apikey-create", orchBearer, map[string]any{
		"name":       "run:" + taskID,
		"project_id": projectID,
		"task_id":    taskID,
	})
	reviewBearer, _ := mintTask["key"].(string)

	// In-set: story_get → 200.
	_, status := apiPostStatus(t, ctx, baseURL+"/api/v1/story/get", reviewBearer, map[string]any{
		"id": storyID,
	})
	if status != http.StatusOK {
		t.Errorf("story/get (review in-set) status = %d, want 200", status)
	}

	// In-set: task_walk → 200.
	_, status = apiPostStatus(t, ctx, baseURL+"/api/v1/task/walk", reviewBearer, map[string]any{
		"story_id": storyID,
	})
	if status != http.StatusOK {
		t.Errorf("task/walk (review in-set) status = %d, want 200", status)
	}

	// Out-of-set: task_add → 403.
	deniedBody, status := apiPostStatus(t, ctx, baseURL+"/api/v1/task/add", reviewBearer, map[string]any{
		"story_id": storyID,
		"agent_id": revID,
		"action":   "contract:develop",
		"prompt":   "review tries to author task",
	})
	if status != http.StatusForbidden {
		t.Errorf("task/add (review out-of-set) status = %d, want 403", status)
	}
	assertVerbDeniedEnvelope(t, deniedBody, "task_add")
}

// resolveAgentID looks up an agent doc by name via /api/v1/document/list
// and returns its id. The seeded role-bound agents are workspace-
// scoped post-sty_58c4839a.
func resolveAgentID(t *testing.T, ctx context.Context, baseURL, bearer, name string) string {
	t.Helper()
	out := apiPostJSON(t, ctx, baseURL+"/api/v1/document/list", bearer, map[string]any{
		"type":  "agent",
		"limit": 50,
	})
	items, _ := out["items"].([]any)
	if len(items) == 0 {
		items, _ = out["_array"].([]any)
	}
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		if got, _ := row["name"].(string); got == name {
			id, _ := row["id"].(string)
			return id
		}
	}
	t.Fatalf("resolveAgentID(%q): not found in document/list response", name)
	return ""
}

// apiPostStatus POSTs body to url with Authorization: Bearer bearer
// and returns the response body as a map plus the HTTP status code.
// Differs from apiPostJSON in that 4xx/5xx do NOT fail the test —
// the gate tests need to assert the status code itself.
func apiPostStatus(t *testing.T, ctx context.Context, url, bearer string, body map[string]any) (map[string]any, int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(buf)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out any
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&out)
	if m, ok := out.(map[string]any); ok {
		return m, resp.StatusCode
	}
	if a, ok := out.([]any); ok {
		return map[string]any{"_array": a}, resp.StatusCode
	}
	return map[string]any{}, resp.StatusCode
}

// assertVerbDeniedEnvelope checks the body carries the three
// required keys of the AC2 wire shape.
func assertVerbDeniedEnvelope(t *testing.T, body map[string]any, wantVerb string) {
	t.Helper()
	if got, _ := body["error"].(string); got != "verb_not_in_allowlist" {
		t.Errorf("envelope.error = %q, want verb_not_in_allowlist; body=%+v", got, body)
	}
	if got, _ := body["verb"].(string); got != wantVerb {
		t.Errorf("envelope.verb = %q, want %q; body=%+v", got, wantVerb, body)
	}
	if _, ok := body["allowed"]; !ok {
		t.Errorf("envelope missing allowed[]: %+v", body)
	}
}

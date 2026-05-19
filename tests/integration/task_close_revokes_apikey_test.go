// sty_056b68f6 — task_update(status=closed) revokes the task-scoped
// agent api-key. After close the dispatched subprocess's bearer
// fails AuthMiddleware (401). The row appears archived in
// agent_apikey_list(include_archived=true).

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

func TestTaskClose_RevokesTaskScopedAPIKey(t *testing.T) {
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
		"name": "close-revoke-orch",
	})
	orchBearer, _ := mint["key"].(string)

	proj := apiPostJSON(t, ctx, baseURL+"/api/v1/project/add", orchBearer, map[string]any{
		"name": "close-revoke-project",
	})
	projectID, _ := proj["id"].(string)
	st := apiPostJSON(t, ctx, baseURL+"/api/v1/story/add", orchBearer, map[string]any{
		"project_id": projectID,
		"title":      "close-revoke-story",
	})
	storyID, _ := st["id"].(string)

	devID := resolveAgentID(t, ctx, baseURL, orchBearer, "developer_agent")
	tsk := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", orchBearer, map[string]any{
		"story_id": storyID,
		"agent_id": devID,
		"action":   "contract:develop",
		"prompt":   "close-revoke fixture",
	})
	taskID, _ := tsk["task_id"].(string)

	mintTask := apiPostJSON(t, ctx, baseURL+"/api/v1/agent/apikey-create", orchBearer, map[string]any{
		"name":       "run:" + taskID,
		"project_id": projectID,
		"task_id":    taskID,
	})
	taskBearer, _ := mintTask["key"].(string)
	taskKeyID, _ := mintTask["id"].(string)

	// Pre-revoke: an in-set verb (satellites_info) succeeds.
	_, status := apiPostStatus(t, ctx, baseURL+"/api/v1/satellites/info", taskBearer, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("pre-revoke satellites/info status = %d, want 200", status)
	}

	// Close the task as the orchestrator. The substrate's
	// TaskUpdate → APIKeys.RevokeByTaskID hook flips the row.
	closeResp := apiPostJSON(t, ctx, baseURL+"/api/v1/task/update", orchBearer, map[string]any{
		"id":      taskID,
		"status":  "closed",
		"outcome": "success",
	})
	if got, _ := closeResp["status"].(string); got != "closed" {
		t.Fatalf("close response status = %q, want closed: %+v", got, closeResp)
	}

	// Post-revoke: same bearer must 401 — LookupByToken misses the
	// archived row and falls through.
	_, status = apiPostStatus(t, ctx, baseURL+"/api/v1/satellites/info", taskBearer, map[string]any{})
	if status != http.StatusUnauthorized {
		t.Errorf("post-revoke satellites/info status = %d, want 401", status)
	}

	// The row appears archived to the orchestrator under
	// include_archived=true.
	list := apiPostJSON(t, ctx, baseURL+"/api/v1/agent/apikey-list", orchBearer, map[string]any{
		"include_archived": true,
	})
	items, _ := list["items"].([]any)
	found := false
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		if id, _ := row["id"].(string); id == taskKeyID {
			found = true
			if got, _ := row["status"].(string); got != "archived" {
				t.Errorf("task key %s status = %q, want archived", taskKeyID, got)
			}
		}
	}
	if !found {
		t.Errorf("task-scoped key id %q not found in include_archived list", taskKeyID)
	}
}

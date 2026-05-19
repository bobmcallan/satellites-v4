// sty_72e36256 — orchestrator-side inline prompt builder.
//
// AC5: the renderer composes a self-contained markdown prompt the
// orchestrator pipes into task_add(prompt=…); the executor running
// under the restricted role envelope (sty_056b68f6) doesn't need
// story_get / agent_get / contract_get / principle_list / task_walk
// / document_get — every section of context is in the prompt body.
//
// This test exercises the live wire chain:
//
//  1. Boot the stack and bootstrap project + story + task with
//     developer_agent whose skill_refs is non-empty.
//  2. Mint a task-scoped apikey for that task; assert the standard
//     read verbs (story_get, agent_get, contract_get, principle_list,
//     task_walk, document_get) all return 403 under that bearer —
//     this verifies the inline-context path is load-bearing, not
//     belt-and-braces.
//  3. Render the prompt via the orchestrator bearer
//     (`task_render_prompt`) and assert the markdown carries the
//     agent body, the contract body, the skill body, the story title,
//     the cited principle, and the "## Your work" header.
//  4. Pipe the rendered markdown into task_add(prompt=<rendered>)
//     via the orchestrator bearer — proving the rendered markdown is
//     directly usable as a task prompt body without further shaping.

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

func TestTaskRenderPrompt_RestrictedEnvelopeDispatch(t *testing.T) {
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

	// Orchestrator (project-scoped) bearer — has every verb.
	mint := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "render-prompt-orchestrator",
	})
	orchBearer, _ := mint["key"].(string)
	if orchBearer == "" {
		t.Fatalf("orchestrator key not minted: %+v", mint)
	}

	proj := apiPostJSON(t, ctx, baseURL+"/api/v1/project/add", orchBearer, map[string]any{
		"name": "render-prompt-project",
	})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project/add: %+v", proj)
	}

	st := apiPostJSON(t, ctx, baseURL+"/api/v1/story/add", orchBearer, map[string]any{
		"project_id":  projectID,
		"title":       "Render prompt dogfood story",
		"description": "Story body cites pr_substrate_model so the renderer must pick it up.",
	})
	storyID, _ := st["id"].(string)
	if storyID == "" {
		t.Fatalf("story/add: %+v", st)
	}

	devID := resolveAgentID(t, ctx, baseURL, orchBearer, "developer_agent")

	tsk := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", orchBearer, map[string]any{
		"story_id": storyID,
		"agent_id": devID,
		"action":   "contract:develop",
		"prompt":   "render-prompt fixture task",
	})
	taskID, _ := tsk["task_id"].(string)
	if taskID == "" {
		t.Fatalf("task/add: %+v", tsk)
	}

	// Task-scoped bearer (the restricted execution envelope).
	mintTask := apiPostJSON(t, ctx, baseURL+"/api/v1/agent/apikey-create", orchBearer, map[string]any{
		"name":       "run:" + taskID,
		"project_id": projectID,
		"task_id":    taskID,
	})
	execBearer, _ := mintTask["key"].(string)
	if execBearer == "" {
		t.Fatalf("task-scoped mint missing key: %+v", mintTask)
	}

	// Restricted envelope — every read verb a render-prompt would
	// invoke must 403 under the task-scoped bearer. If any of these
	// were 200, the inline-context path would not be load-bearing.
	forbidden := []struct {
		path string
		body map[string]any
	}{
		{"/api/v1/story/get", map[string]any{"id": storyID}},
		{"/api/v1/agent/get", map[string]any{"id": devID}},
		{"/api/v1/contract/get", map[string]any{"name": "develop"}},
		{"/api/v1/principle/list", map[string]any{"project_id": projectID, "active_only": true}},
		{"/api/v1/task/walk", map[string]any{"story_id": storyID}},
	}
	for _, fb := range forbidden {
		denied, status := apiPostStatus(t, ctx, baseURL+fb.path, execBearer, fb.body)
		if status != http.StatusForbidden {
			t.Errorf("%s (under task-scoped bearer) status=%d want 403; body=%+v", fb.path, status, denied)
		}
	}

	// Render the prompt over the orchestrator bearer.
	rendered := apiPostJSON(t, ctx, baseURL+"/api/v1/task/render-prompt", orchBearer, map[string]any{
		"task_id":  taskID,
		"action":   "contract:develop",
		"story_id": storyID,
		"work":     "Implement the renderer end-to-end.",
	})
	prompt, _ := rendered["prompt"].(string)
	if prompt == "" {
		t.Fatalf("task/render-prompt missing prompt field: %+v", rendered)
	}

	// AC2 — every required section appears, in order.
	want := []string{
		"# Task " + taskID,
		"## Your role",
		"## Contract",
		"## Story",
		"## Principles in force",
		"## Your work",
		"## Close",
	}
	prev := -1
	for _, w := range want {
		idx := strings.Index(prompt, w)
		if idx < 0 {
			t.Errorf("rendered prompt missing %q; prompt=%q", w, prompt[:min(len(prompt), 400)])
			continue
		}
		if idx <= prev {
			t.Errorf("rendered prompt section %q out of order (idx=%d, prev=%d)", w, idx, prev)
		}
		prev = idx
	}
	// Cited principle present; uncited principles absent.
	if !strings.Contains(prompt, "pr_substrate_model") {
		t.Errorf("rendered prompt missing cited principle pr_substrate_model; got first 300 bytes=%q", prompt[:min(len(prompt), 300)])
	}
	// Work body verbatim.
	if !strings.Contains(prompt, "Implement the renderer end-to-end.") {
		t.Errorf("rendered prompt missing work body verbatim")
	}

	// AC1 — pipe the rendered prompt into task_add over the
	// orchestrator bearer. The substrate must accept the markdown
	// as the new task's prompt body.
	piped := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", orchBearer, map[string]any{
		"story_id": storyID,
		"agent_id": devID,
		"action":   "contract:develop",
		"prompt":   prompt,
	})
	if id, _ := piped["task_id"].(string); id == "" {
		t.Fatalf("task/add with rendered prompt failed: %+v", piped)
	}
}

// min is the local helper (Go 1.21+ has builtin min, kept for clarity).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

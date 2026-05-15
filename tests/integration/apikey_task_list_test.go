// sty_0be97c3e AC4 — `satellites-client task list` authenticates via
// an apikey minted at satellites_init. Asserts the apikey Bearer
// reaches /api/v1/task/list and the response is project-scoped.
//
// Test design: in-process flow that mints an apikey (cookie-auth path)
// and then exercises /api/v1/task/list with Authorization: Bearer
// <apikey>. The satellites-client CLI wraps the same POST shape via
// cliremote; this test asserts the wire-level header carries the key
// and the substrate response surfaces only rows for the apikey
// owner's projects. Per pr_local_iteration: full subprocess driving
// of the binary is in task_run_apiv1_test.go; this test focuses on
// the auth-resolution leg AC4 names.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestAPIKeyTaskList_ScopedToOwner — sty_0be97c3e AC4.
//
//  1. Boot Surreal + satellites in dev mode.
//  2. Cookie-login as dev@local.
//  3. Mint an apikey via agent_apikey_create.
//  4. Add a project + story + task under that owner.
//  5. POST /api/v1/task/list with Authorization: Bearer <apikey>.
//  6. Assert: 200, response carries at least one row, every row's
//     project_id matches the project we just created.
func TestAPIKeyTaskList_ScopedToOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {"surrealdb"},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	docsHost := filepath.Join(repoRoot(t), "docs")
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"SATELLITES_DB_DSN":       "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_DEV_USERNAME": "dev@local",
			"SATELLITES_DEV_PASSWORD": "letmein",
			"SATELLITES_DOCS_DIR":     "/app/docs",
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	defer stop()

	cookie := devLogin(t, ctx, baseURL, "dev@local", "letmein")
	mcpURL := baseURL + "/mcp"

	mint := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "task-list-apikey",
	})
	bearer, _ := mint["key"].(string)
	if bearer == "" {
		t.Fatalf("agent_apikey_create returned no key: %+v", mint)
	}

	// Bootstrap project + story + task scoped to the apikey owner.
	proj := apiPostJSON(t, ctx, baseURL+"/api/v1/project/add", bearer, map[string]any{
		"name": "apikey-tasklist-project",
	})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project/add returned no id: %+v", proj)
	}

	story := apiPostJSON(t, ctx, baseURL+"/api/v1/story/add", bearer, map[string]any{
		"project_id": projectID,
		"title":      "apikey-scope-story",
	})
	storyID, _ := story["id"].(string)
	if storyID == "" {
		t.Fatalf("story/add returned no id: %+v", story)
	}

	taskResp := apiPostJSON(t, ctx, baseURL+"/api/v1/task/add", bearer, map[string]any{
		"story_id":    storyID,
		"action":      "contract:plan",
		"description": "AC4 fixture task",
	})
	if id, _ := taskResp["id"].(string); id == "" {
		t.Fatalf("task/add returned no id: %+v", taskResp)
	}

	// AC4: /api/v1/task/list with Authorization: Bearer <apikey>.
	out := apiPostJSON(t, ctx, baseURL+"/api/v1/task/list", bearer, map[string]any{
		"story_id": storyID,
	})
	tasks, _ := out["tasks"].([]any)
	if len(tasks) == 0 {
		// task/list may return rows under the bare array key — accept
		// both shapes since the typed surface is the source of truth.
		raw, _ := out["_array"].([]any)
		tasks = raw
	}
	if len(tasks) == 0 {
		t.Fatalf("task/list returned no rows for project_id=%q: %+v", projectID, out)
	}
	for _, raw := range tasks {
		row, _ := raw.(map[string]any)
		if got, _ := row["project_id"].(string); got != projectID {
			t.Errorf("task row project_id=%q, want %q — apikey bearer is not enforcing project scope: %+v", got, projectID, row)
		}
	}
}

// apiPostJSON POSTs a JSON body to url with Authorization: Bearer
// bearer and decodes the response as a map. Used by AC4 fixtures
// without taking a dependency on the cliremote package.
func apiPostJSON(t *testing.T, ctx context.Context, url, bearer string, body map[string]any) map[string]any {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status=%d body=%s", url, resp.StatusCode, string(raw))
	}
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v; raw=%s", url, err, string(raw))
	}
	if m, ok := out.(map[string]any); ok {
		return m
	}
	if a, ok := out.([]any); ok {
		return map[string]any{"_array": a}
	}
	return map[string]any{}
}

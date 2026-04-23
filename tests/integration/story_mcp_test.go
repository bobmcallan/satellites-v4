package integration

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

// TestStoryMCPRoundTrip drives the full story primitive MCP surface:
// project_create → story_create → story_list → transitions
// (backlog→ready→in_progress→done) → assert 3 ledger rows of type
// story.status_change for the owning project.
func TestStoryMCPRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
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

	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"DB_DSN":              "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_story",
			"DOCS_DIR":            "/app/docs",
		},
	})
	defer stop()

	mcpURL := baseURL + "/mcp"

	rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	})

	// tools/list must advertise all story tools.
	listResp := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result, _ := listResp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	need := map[string]bool{"story_create": false, "story_get": false, "story_list": false, "story_update_status": false}
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if name, _ := tool["name"].(string); name != "" {
				if _, tracked := need[name]; tracked {
					need[name] = true
				}
			}
		}
	}
	for k, seen := range need {
		if !seen {
			t.Errorf("tools/list missing %q", k)
		}
	}

	// Create project.
	createProj := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_create",
			"arguments": map[string]any{"name": "story-smoke"},
		},
	})
	var proj map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, createProj)), &proj)
	projID, _ := proj["id"].(string)

	// Create story.
	createStory := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name": "story_create",
			"arguments": map[string]any{
				"project_id":          projID,
				"title":               "end-to-end",
				"description":         "transitions should write ledger rows",
				"acceptance_criteria": "3 ledger rows after full lifecycle",
				"priority":            "high",
				"category":            "feature",
				"tags":                []string{"epic:v4-stories"},
			},
		},
	})
	var st map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, createStory)), &st); err != nil {
		t.Fatalf("decode story_create: %v", err)
	}
	storyID, _ := st["id"].(string)
	if !strings.HasPrefix(storyID, "sty_") {
		t.Fatalf("story id = %q", storyID)
	}
	if status, _ := st["status"].(string); status != "backlog" {
		t.Errorf("initial status = %q, want backlog", status)
	}

	// story_list via tag filter must surface it.
	listByTag := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{
			"name": "story_list",
			"arguments": map[string]any{
				"project_id": projID,
				"tag":        "epic:v4-stories",
			},
		},
	})
	var listed []map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, listByTag)), &listed)
	found := false
	for _, l := range listed {
		if id, _ := l["id"].(string); id == storyID {
			found = true
		}
	}
	if !found {
		t.Errorf("story_list by tag missing created story")
	}

	// Transition lifecycle with a sleep between calls so ledger rows have
	// distinct created_at values (surrealdb time precision workaround).
	for i, next := range []string{"ready", "in_progress", "done"} {
		if i > 0 {
			time.Sleep(1200 * time.Millisecond)
		}
		resp := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
			"jsonrpc": "2.0", "id": 10 + i, "method": "tools/call",
			"params": map[string]any{
				"name":      "story_update_status",
				"arguments": map[string]any{"id": storyID, "status": next},
			},
		})
		var updated map[string]any
		if err := json.Unmarshal([]byte(extractToolText(t, resp)), &updated); err != nil {
			t.Fatalf("decode update_status %q: %v", next, err)
		}
		if gs, _ := updated["status"].(string); gs != next {
			t.Errorf("transition to %q: got status %q", next, gs)
		}
	}

	// Invalid transition from done → ready must error.
	bad := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 20, "method": "tools/call",
		"params": map[string]any{
			"name":      "story_update_status",
			"arguments": map[string]any{"id": storyID, "status": "ready"},
		},
	})
	badResult, _ := bad["result"].(map[string]any)
	if isErr, _ := badResult["isError"].(bool); !isErr {
		t.Errorf("terminal state should isError on further transition; got %+v", badResult)
	}

	// Ledger must contain exactly 3 story.status_change rows.
	ledgerResp := rpcCall(t, ctx, mcpURL, "key_story", map[string]any{
		"jsonrpc": "2.0", "id": 30, "method": "tools/call",
		"params": map[string]any{
			"name": "ledger_list",
			"arguments": map[string]any{
				"project_id": projID,
				"type":       "story.status_change",
			},
		},
	})
	var entries []map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, ledgerResp)), &entries); err != nil {
		t.Fatalf("decode ledger_list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("status_change rows = %d, want 3; entries=%+v", len(entries), entries)
	}
	for _, e := range entries {
		if actor, _ := e["actor"].(string); actor != "apikey" {
			t.Errorf("actor = %q, want apikey", actor)
		}
		if content, _ := e["content"].(string); !strings.Contains(content, storyID) {
			t.Errorf("entry content missing story id: %q", content)
		}
	}
}

package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestProjectMCPRoundTrip drives the new project MCP surface end-to-end:
// initialize → project_create → project_list → document_ingest_file with
// the new project's id → document_get with and without cross-project access.
func TestProjectMCPRoundTrip(t *testing.T) {
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
			"DB_DSN":              "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_proj",
			"DOCS_DIR":            "/app/docs",
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	defer stop()

	mcpURL := baseURL + "/mcp"

	// Initialize.
	init := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	})
	if init["error"] != nil {
		t.Fatalf("initialize: %v", init["error"])
	}

	// tools/list must include the new project tools.
	list := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result, _ := list["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	need := map[string]bool{
		"project_create": false,
		"project_get":    false,
		"project_list":   false,
	}
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if name, _ := tool["name"].(string); name != "" {
				if _, tracked := need[name]; tracked {
					need[name] = true
				}
			}
		}
	}
	for name, seen := range need {
		if !seen {
			t.Errorf("tools/list missing %q", name)
		}
	}

	// project_create returns an owned project.
	created := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_create",
			"arguments": map[string]any{"name": "alpha"},
		},
	})
	var proj map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, created)), &proj); err != nil {
		t.Fatalf("decode project_create: %v", err)
	}
	projID, _ := proj["id"].(string)
	if !strings.HasPrefix(projID, "proj_") {
		t.Errorf("project id = %q, want proj_ prefix", projID)
	}
	if owner, _ := proj["owner_user_id"].(string); owner != "apikey" {
		t.Errorf("owner_user_id = %q, want apikey", owner)
	}

	// project_list must include the newly created project.
	listResp := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_list",
			"arguments": map[string]any{},
		},
	})
	var listed []map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, listResp)), &listed); err != nil {
		t.Fatalf("decode project_list: %v", err)
	}
	var foundID bool
	for _, p := range listed {
		if id, _ := p["id"].(string); id == projID {
			foundID = true
		}
	}
	if !foundID {
		t.Errorf("project_list missing newly created project %q; got %+v", projID, listed)
	}

	// document_ingest_file with an explicit project_id writes to that scope.
	ingestResp := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_ingest_file",
			"arguments": map[string]any{"path": "architecture.md", "project_id": projID},
		},
	})
	var ingest map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, ingestResp)), &ingest); err != nil {
		t.Fatalf("decode document_ingest_file: %v", err)
	}
	if pid, _ := ingest["project_id"].(string); pid != projID {
		t.Errorf("ingested doc project_id = %q, want %q", pid, projID)
	}
	if ingest["created"] != true {
		t.Errorf("first ingest created = %v, want true", ingest["created"])
	}

	// document_get in the same project round-trips the body.
	getResp := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_get",
			"arguments": map[string]any{"filename": "architecture.md", "project_id": projID},
		},
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, getResp)), &doc); err != nil {
		t.Fatalf("decode document_get: %v", err)
	}
	if body, _ := doc["body"].(string); !strings.Contains(body, "Satellites v4 — Architecture") {
		t.Errorf("document_get returned unexpected body (%d bytes)", len(body))
	}

	// Cross-project access rejection: asking for an unknown project_id as
	// a non-owner must return isError (no leak).
	bogus := rpcCall(t, ctx, mcpURL, "key_proj", map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_get",
			"arguments": map[string]any{"filename": "architecture.md", "project_id": "proj_doesnotexist"},
		},
	})
	bogusResult, _ := bogus["result"].(map[string]any)
	if isErr, _ := bogusResult["isError"].(bool); !isErr {
		t.Errorf("cross-project access should set isError; got %+v", bogusResult)
	}
}

// Tests the project_* write-verb chain end-to-end over MCP against a
// real SurrealDB (sty_690b06ee, plan §3.9 AC5 dev-time evidence).
//
// The five-call chain mirrors the dogfood walk the merge_to_main task
// runs against pprod (AC6): project_add → project_set (resolved) →
// project_update → project_delete → project_set (no_project_for_remote
// because the archived project is filtered out by resolveProjectByRemote).
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

// TestProjectWriteVerbs_MCP_RoundTrip exercises the AC5 chain locally
// over MCP only. A real SurrealDB + server container booted via
// testcontainers backs the run. Asserts each verb's wire-shape
// invariants: project_add returns repo_url_canonical when repo_url is
// supplied, project_set resolves to that project, project_update
// patches description, project_delete reports cascade_summary, the
// follow-up project_set returns no_project_for_remote.
func TestProjectWriteVerbs_MCP_RoundTrip(t *testing.T) {
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
			"SATELLITES_DB_DSN":   "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_pwe2e",
			"SATELLITES_DOCS_DIR": "/app/docs",
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
	rpcInit(t, ctx, mcpURL, "key_pwe2e")

	// tools/list must advertise the three write verbs.
	listResp := rpcCall(t, ctx, mcpURL, "key_pwe2e", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	need := map[string]bool{
		"project_add":    false,
		"project_update": false,
		"project_delete": false,
		"project_set":    false,
	}
	if result, _ := listResp["result"].(map[string]any); result != nil {
		if tools, _ := result["tools"].([]any); tools != nil {
			for _, raw := range tools {
				if tool, ok := raw.(map[string]any); ok {
					if name, _ := tool["name"].(string); name != "" {
						if _, tracked := need[name]; tracked {
							need[name] = true
						}
					}
				}
			}
		}
	}
	for name, seen := range need {
		if !seen {
			t.Errorf("tools/list missing %q", name)
		}
	}

	repoURL := "https://example.invalid/e2e/sty_690b06ee"

	// 1. project_add with name+repo_url+description.
	added := rpcCall(t, ctx, mcpURL, "key_pwe2e", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "project_add",
			"arguments": map[string]any{
				"name":        "e2e-bootstrap",
				"repo_url":    repoURL,
				"description": "sty_690b06ee dev-time e2e evidence",
			},
		},
	})
	var addedPV map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, added)), &addedPV); err != nil {
		t.Fatalf("decode project_add: %v", err)
	}
	pid, _ := addedPV["id"].(string)
	if !strings.HasPrefix(pid, "proj_") {
		t.Fatalf("project_add returned no id; resp=%+v", addedPV)
	}
	if addedPV["status"] != "active" {
		t.Errorf("status = %v, want active", addedPV["status"])
	}
	if addedPV["description"] != "sty_690b06ee dev-time e2e evidence" {
		t.Errorf("description = %v, want sty_690b06ee dev-time e2e evidence", addedPV["description"])
	}
	if addedPV["repo_url_canonical"] == nil {
		t.Errorf("project_add must echo repo_url_canonical; resp=%+v", addedPV)
	}

	// 2. project_set against the same repo_url resolves.
	resolved := rpcCall(t, ctx, mcpURL, "key_pwe2e", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_set",
			"arguments": map[string]any{"repo_url": repoURL},
		},
	})
	var resolvedPV map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, resolved)), &resolvedPV); err != nil {
		t.Fatalf("decode project_set: %v", err)
	}
	if resolvedPV["status"] != "resolved" {
		t.Errorf("project_set status = %v, want resolved", resolvedPV["status"])
	}
	if resolvedPV["project_id"] != pid {
		t.Errorf("project_set project_id = %v, want %s", resolvedPV["project_id"], pid)
	}

	// 3. project_update flips description.
	updated := rpcCall(t, ctx, mcpURL, "key_pwe2e", map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{
			"name": "project_update",
			"arguments": map[string]any{
				"id":          pid,
				"description": "updated by e2e dogfood",
			},
		},
	})
	var updatedPV map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, updated)), &updatedPV)
	if updatedPV["description"] != "updated by e2e dogfood" {
		t.Errorf("project_update description = %v, want updated by e2e dogfood", updatedPV["description"])
	}

	// 4. project_delete archives + reports cascade_summary.
	deleted := rpcCall(t, ctx, mcpURL, "key_pwe2e", map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_delete",
			"arguments": map[string]any{"id": pid},
		},
	})
	var deletedPV map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, deleted)), &deletedPV); err != nil {
		t.Fatalf("decode project_delete: %v", err)
	}
	if deletedPV["status"] != "archived" {
		t.Errorf("project_delete status = %v, want archived", deletedPV["status"])
	}
	cs, _ := deletedPV["cascade_summary"].(map[string]any)
	if cs == nil {
		t.Errorf("project_delete missing cascade_summary: %+v", deletedPV)
	}

	// 5. project_set after archive returns no_project_for_remote.
	postArchive := rpcCall(t, ctx, mcpURL, "key_pwe2e", map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_set",
			"arguments": map[string]any{"repo_url": repoURL},
		},
	})
	var postPV map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, postArchive)), &postPV)
	if postPV["status"] != "no_project_for_remote" {
		t.Errorf("project_set after archive status = %v, want no_project_for_remote", postPV["status"])
	}
}

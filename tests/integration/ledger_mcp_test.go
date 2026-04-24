package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// TestLedgerMCPRoundTrip exercises ledger_append + ledger_list over the
// HTTP MCP surface: project_create → ledger_append × 3 (mixed types) →
// ledger_list (all → 3, filtered → matches) → cross-project ledger_list
// returns isError.
func TestLedgerMCPRoundTrip(t *testing.T) {
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
		t.Fatalf("surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"DB_DSN":              "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_ledger",
			"DOCS_DIR":            "/app/docs",
		},
	})
	defer stop()

	mcpURL := baseURL + "/mcp"

	rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	})

	// tools/list must include ledger_append + ledger_list.
	list := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result, _ := list["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	needLedger := map[string]bool{"ledger_append": false, "ledger_list": false}
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if name, _ := tool["name"].(string); name != "" {
				if _, tracked := needLedger[name]; tracked {
					needLedger[name] = true
				}
			}
		}
	}
	for k, seen := range needLedger {
		if !seen {
			t.Errorf("tools/list missing %q", k)
		}
	}

	// Create a project under the api-key owner.
	createResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_create",
			"arguments": map[string]any{"name": "ledger-smoke"},
		},
	})
	var proj map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, createResp)), &proj); err != nil {
		t.Fatalf("decode project_create: %v", err)
	}
	projID, _ := proj["id"].(string)
	if projID == "" {
		t.Fatal("project_create did not return an id")
	}

	// Append 3 entries: 2 of type A, 1 of type B. A small sleep between
	// calls ensures created_at differs enough for the DESC ORDER BY to be
	// deterministic across SurrealDB's time-precision floor.
	for i, spec := range []struct{ etype, content string }{
		{ledger.TypeDecision, "one"},
		{ledger.TypeDecision, "two"},
		{ledger.TypeArtifact, "three"},
	} {
		if i > 0 {
			time.Sleep(1100 * time.Millisecond)
		}
		appendResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
			"jsonrpc": "2.0", "id": 10 + i, "method": "tools/call",
			"params": map[string]any{
				"name": "ledger_append",
				"arguments": map[string]any{
					"project_id": projID,
					"type":       spec.etype,
					"content":    spec.content,
				},
			},
		})
		var entry map[string]any
		if err := json.Unmarshal([]byte(extractToolText(t, appendResp)), &entry); err != nil {
			t.Fatalf("decode ledger_append[%d]: %v", i, err)
		}
		if id, _ := entry["id"].(string); id == "" {
			t.Errorf("ledger_append[%d]: empty id", i)
		}
		if cb, _ := entry["created_by"].(string); cb != "apikey" {
			t.Errorf("ledger_append[%d]: created_by = %q, want apikey", i, cb)
		}
	}

	// List all (no filter) → 3 entries newest-first.
	allResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 20, "method": "tools/call",
		"params": map[string]any{
			"name": "ledger_list",
			"arguments": map[string]any{
				"project_id": projID,
			},
		},
	})
	var all []map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, allResp)), &all); err != nil {
		t.Fatalf("decode ledger_list: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ledger_list all: count = %d, want 3", len(all))
	}
	// Newest first means "three" is at [0].
	if c, _ := all[0]["content"].(string); c != "three" {
		t.Errorf("newest-first broken: got %q at index 0", c)
	}

	// Type filter.
	filterResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 21, "method": "tools/call",
		"params": map[string]any{
			"name": "ledger_list",
			"arguments": map[string]any{
				"project_id": projID,
				"type":       ledger.TypeDecision,
			},
		},
	})
	var filtered []map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, filterResp)), &filtered); err != nil {
		t.Fatalf("decode filtered ledger_list: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("type filter: count = %d, want 2", len(filtered))
	}
	for _, e := range filtered {
		if tt, _ := e["type"].(string); tt != ledger.TypeDecision {
			t.Errorf("filter leaked %q", tt)
		}
	}

	// Limit.
	limitResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 22, "method": "tools/call",
		"params": map[string]any{
			"name": "ledger_list",
			"arguments": map[string]any{
				"project_id": projID,
				"limit":      1,
			},
		},
	})
	var capped []map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, limitResp)), &capped)
	if len(capped) != 1 {
		t.Errorf("limit 1: count = %d, want 1", len(capped))
	}

	// Cross-project: ledger_list on a non-existent id → isError (no leak).
	bogus := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 23, "method": "tools/call",
		"params": map[string]any{
			"name": "ledger_list",
			"arguments": map[string]any{
				"project_id": "proj_doesnotexist",
			},
		},
	})
	bogusResult, _ := bogus["result"].(map[string]any)
	if isErr, _ := bogusResult["isError"].(bool); !isErr {
		t.Errorf("cross-project ledger_list should set isError; got %+v", bogusResult)
	}

	// === slice 7.2 verb extensions: append-with-rich-fields, get,
	// search, recall, dereference. Run after the legacy assertions so
	// they don't perturb the original counts. ===
	expires := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	richResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 30, "method": "tools/call",
		"params": map[string]any{
			"name": "ledger_append",
			"arguments": map[string]any{
				"project_id":  projID,
				"type":        ledger.TypeEvidence,
				"content":     "rich-row",
				"story_id":    "sty_synthetic",
				"contract_id": "ci_synthetic",
				"tags":        []any{"phase:plan", "kind:rich"},
				"structured":  `{"score":0.9}`,
				"durability":  "ephemeral",
				"expires_at":  expires,
				"source_type": "agent",
				"sensitive":   false,
			},
		},
	})
	var rich map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, richResp)), &rich); err != nil {
		t.Fatalf("decode rich append: %v", err)
	}
	richID, _ := rich["id"].(string)
	if richID == "" {
		t.Fatalf("rich append missing id: %+v", rich)
	}
	if got, _ := rich["story_id"].(string); got != "sty_synthetic" {
		t.Errorf("rich append story_id = %q, want sty_synthetic", got)
	}

	// ledger_get(id) round-trip.
	getResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 31, "method": "tools/call",
		"params": map[string]any{
			"name":      "ledger_get",
			"arguments": map[string]any{"id": richID},
		},
	})
	var getRow map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, getResp)), &getRow); err != nil {
		t.Fatalf("decode ledger_get: %v", err)
	}
	if getRow["id"] != richID {
		t.Errorf("ledger_get returned id %v, want %q", getRow["id"], richID)
	}

	// ledger_search by query substring on content.
	searchResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 32, "method": "tools/call",
		"params": map[string]any{
			"name":      "ledger_search",
			"arguments": map[string]any{"project_id": projID, "query": "rich-row"},
		},
	})
	var searchHits []map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, searchResp)), &searchHits); err != nil {
		t.Fatalf("decode ledger_search: %v", err)
	}
	if len(searchHits) == 0 {
		t.Errorf("ledger_search(query=rich-row) returned no hits")
	}

	// ledger_dereference flips the row's status; default ledger_list
	// excludes it; explicit status filter returns it.
	derefResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 33, "method": "tools/call",
		"params": map[string]any{
			"name":      "ledger_dereference",
			"arguments": map[string]any{"id": richID, "reason": "superseded"},
		},
	})
	var audit map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, derefResp)), &audit); err != nil {
		t.Fatalf("decode ledger_dereference: %v", err)
	}
	if audit["type"] != ledger.TypeDecision {
		t.Errorf("audit type = %v, want decision", audit["type"])
	}

	defaultListResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 34, "method": "tools/call",
		"params": map[string]any{
			"name":      "ledger_list",
			"arguments": map[string]any{"project_id": projID},
		},
	})
	var defaultList []map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, defaultListResp)), &defaultList)
	for _, r := range defaultList {
		if r["id"] == richID {
			t.Errorf("default list still includes dereferenced row %q", richID)
		}
	}

	derefdListResp := rpcCall(t, ctx, mcpURL, "key_ledger", map[string]any{
		"jsonrpc": "2.0", "id": 35, "method": "tools/call",
		"params": map[string]any{
			"name":      "ledger_list",
			"arguments": map[string]any{"project_id": projID, "status": "dereferenced"},
		},
	})
	var derefdList []map[string]any
	_ = json.Unmarshal([]byte(extractToolText(t, derefdListResp)), &derefdList)
	found := false
	for _, r := range derefdList {
		if r["id"] == richID {
			found = true
		}
	}
	if !found {
		t.Errorf("status=dereferenced filter did not return dereferenced row %q; got %+v", richID, derefdList)
	}
}

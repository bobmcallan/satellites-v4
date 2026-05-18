package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestDocumentCRUDVerbs_RoundTrip exercises the slice 6.2 generic
// document_add / document_list / document_get(id) / document_update /
// document_delete verbs against a real SurrealDB. The walk is:
//
//  1. document_add — happy path returns the new doc
//  2. document_list   — finds the new row by type filter
//  3. document_get    — id-keyed retrieval
//  4. document_update — partial update on body + tags; immutable rejection
//  5. document_delete — default archive hides from list; mode=hard removes
//
// FK rejection: a separate document_add with a dangling
// contract_binding must isError; binding to an existing type=contract row
// passes.
func TestDocumentCRUDVerbs_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv: map[string]string{"SATELLITES_API_KEYS": "key_crud"},
	})
	defer stack.Stop()
	baseURL := stack.BaseURL

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, "key_crud")

	// 1. tools/list must include the five new verbs.
	listResp := rpcCall(t, ctx, mcpURL, "key_crud", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	want := map[string]bool{
		"document_add": false, "document_update": false,
		"document_list": false, "document_delete": false,
		"document_get": false,
	}
	if result, _ := listResp["result"].(map[string]any); result != nil {
		if tools, _ := result["tools"].([]any); tools != nil {
			for _, raw := range tools {
				if tool, ok := raw.(map[string]any); ok {
					if name, _ := tool["name"].(string); name != "" {
						if _, tracked := want[name]; tracked {
							want[name] = true
						}
					}
				}
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("tools/list missing %q", k)
		}
	}

	// 2. document_add — system-scope principle (no project_id).
	created := callTool(t, ctx, mcpURL, "key_crud", "document_add", map[string]any{
		"type":  "principle",
		"scope": "system",
		"name":  "test-principle",
		"body":  "# Test\n",
		"tags":  []any{"v4", "test"},
	})
	docID, _ := created["id"].(string)
	if docID == "" {
		t.Fatalf("document_add returned no id: %+v", created)
	}
	if got, _ := created["scope"].(string); got != "system" {
		t.Errorf("created scope = %q, want system", got)
	}

	// 3. document_add — project=system mismatch → rejection.
	bogus := callToolRaw(t, ctx, mcpURL, "key_crud", "document_add", map[string]any{
		"type":       "principle",
		"scope":      "system",
		"name":       "bad",
		"project_id": "proj_should_be_rejected",
	})
	if !isToolError(bogus) {
		t.Errorf("scope=system + project_id should isError; got %+v", bogus)
	}

	// 4. document_add — dangling contract_binding rejected.
	dangling := callToolRaw(t, ctx, mcpURL, "key_crud", "document_add", map[string]any{
		"type":             "skill",
		"scope":            "system",
		"name":             "skill-dangling",
		"contract_binding": "doc_does_not_exist",
	})
	if !isToolError(dangling) {
		t.Errorf("dangling contract_binding should isError; got %+v", dangling)
	}

	// 5. document_add — bind a skill to a real contract.
	contract := callTool(t, ctx, mcpURL, "key_crud", "document_add", map[string]any{
		"type":  "contract",
		"scope": "system",
		"name":  "test-contract",
		"body":  "contract body",
	})
	contractID, _ := contract["id"].(string)
	skill := callTool(t, ctx, mcpURL, "key_crud", "document_add", map[string]any{
		"type":             "skill",
		"scope":            "system",
		"name":             "test-skill",
		"contract_binding": contractID,
	})
	if got, _ := skill["contract_binding"].(string); got != contractID {
		t.Errorf("skill contract_binding = %q, want %q", got, contractID)
	}

	// 6. document_list filtered by type=principle includes the
	// test-principle alongside the system-seeded principles
	// (story_ac3dc4d0 added config/seed/system/principles/ — 9 system rows
	// land at boot). Assert the test row is present rather than a
	// literal count.
	plistArr := callToolArray(t, ctx, mcpURL, "key_crud", "document_list", map[string]any{
		"type":  "principle",
		"scope": "system",
	})
	foundTestPrinciple := false
	for _, row := range plistArr {
		m, _ := row.(map[string]any)
		if m == nil {
			continue
		}
		if name, _ := m["name"].(string); name == "test-principle" {
			foundTestPrinciple = true
			break
		}
	}
	if !foundTestPrinciple {
		t.Errorf("document_list(type=principle, scope=system) did not include test-principle; got %d rows", len(plistArr))
	}

	// 7. document_get by id round-trips.
	got := callTool(t, ctx, mcpURL, "key_crud", "document_get", map[string]any{"id": docID})
	if got["id"] != docID || got["name"] != "test-principle" {
		t.Errorf("document_get(id) = %+v", got)
	}

	// 8. document_update partial: body + tags. Immutable rejection.
	updated := callTool(t, ctx, mcpURL, "key_crud", "document_update", map[string]any{
		"id":   docID,
		"body": "# Updated\n",
		"tags": []any{"v4", "updated"},
	})
	if updated["body"] != "# Updated\n" {
		t.Errorf("update body = %v, want \"# Updated\\n\"", updated["body"])
	}
	if v, _ := updated["version"].(float64); v != 2 {
		t.Errorf("update version = %v, want 2", updated["version"])
	}
	for _, immutable := range []string{"workspace_id", "project_id", "type", "scope", "name"} {
		resp := callToolRaw(t, ctx, mcpURL, "key_crud", "document_update", map[string]any{
			"id":      docID,
			immutable: "tampered",
		})
		if !isToolError(resp) {
			t.Errorf("update with immutable %q should isError; got %+v", immutable, resp)
		}
	}

	// 9. document_delete archive (default).
	delResp := callTool(t, ctx, mcpURL, "key_crud", "document_delete", map[string]any{"id": docID})
	if delResp["deleted"] != true {
		t.Errorf("delete deleted = %v, want true", delResp["deleted"])
	}
	// After archive, document_get(id) still returns the row (status=archived);
	// document_list with type=principle filters by status=active in the
	// store path? — no, List composes structured filters but doesn't
	// auto-exclude archived. The archive call flips status to "archived".
	gotArchived := callTool(t, ctx, mcpURL, "key_crud", "document_get", map[string]any{"id": docID})
	if gotArchived["status"] != "archived" {
		t.Errorf("after archive status = %v, want archived", gotArchived["status"])
	}

	// 10. document_delete hard removes the row.
	hardDel := callTool(t, ctx, mcpURL, "key_crud", "document_delete", map[string]any{
		"id":   docID,
		"mode": "hard",
	})
	if hardDel["mode"] != "hard" {
		t.Errorf("hard delete mode = %v, want hard", hardDel["mode"])
	}
	gone := callToolRaw(t, ctx, mcpURL, "key_crud", "document_get", map[string]any{"id": docID})
	if !isToolError(gone) {
		t.Errorf("after hard delete document_get should isError; got %+v", gone)
	}

	// 11. document_search — query matches body of the contract row;
	// type=contract narrows; empty-query+tag returns updated_at DESC
	// list.
	searchHits := callToolArray(t, ctx, mcpURL, "key_crud", "document_search", map[string]any{
		"query": "contract body",
	})
	if len(searchHits) != 1 || searchHits[0].(map[string]any)["id"] != contractID {
		t.Errorf("search(query=contract body) = %+v, want only the contract row", searchHits)
	}

	combined := callToolArray(t, ctx, mcpURL, "key_crud", "document_search", map[string]any{
		"query": "contract body",
		"type":  "principle",
	})
	if len(combined) != 0 {
		t.Errorf("search(query=contract body, type=principle) = %d rows, want 0 (AND filter)", len(combined))
	}

	emptyQ := callToolArray(t, ctx, mcpURL, "key_crud", "document_search", map[string]any{
		"type": "skill",
	})
	if len(emptyQ) != 1 {
		t.Errorf("search(empty query, type=skill) = %d rows, want 1", len(emptyQ))
	}
}

func rpcInit(t *testing.T, ctx context.Context, mcpURL, apiKey string) {
	t.Helper()
	init := rpcCall(t, ctx, mcpURL, apiKey, map[string]any{
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
}

// callTool calls the named tool and returns the parsed JSON object the
// handler emitted; fails the test on isError or non-object.
func callTool(t *testing.T, ctx context.Context, mcpURL, apiKey, name string, args map[string]any) map[string]any {
	t.Helper()
	resp := callToolRaw(t, ctx, mcpURL, apiKey, name, args)
	if isToolError(resp) {
		t.Fatalf("%s isError: %+v", name, resp)
	}
	text := extractToolText(t, resp)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s decode: %v; raw=%s", name, err, text)
	}
	return out
}

// callToolArray is the array-result variant of callTool (e.g. document_list).
func callToolArray(t *testing.T, ctx context.Context, mcpURL, apiKey, name string, args map[string]any) []any {
	t.Helper()
	resp := callToolRaw(t, ctx, mcpURL, apiKey, name, args)
	if isToolError(resp) {
		t.Fatalf("%s isError: %+v", name, resp)
	}
	text := extractToolText(t, resp)
	if text == "null" {
		return nil
	}
	var out []any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s decode: %v; raw=%s", name, err, text)
	}
	return out
}

// callToolRaw posts a tools/call without asserting success; the caller
// inspects isError to test rejection paths.
func callToolRaw(t *testing.T, ctx context.Context, mcpURL, apiKey, name string, args map[string]any) map[string]any {
	t.Helper()
	return rpcCall(t, ctx, mcpURL, apiKey, map[string]any{
		"jsonrpc": "2.0", "id": time.Now().UnixNano(), "method": "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
}

func isToolError(resp map[string]any) bool {
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		return false
	}
	isErr, _ := result["isError"].(bool)
	return isErr
}

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestMCPInfoToolRespondsOverHTTP spins up the server in a container with a
// known API key, then exercises the Streamable HTTP MCP protocol:
// initialize → tools/list → tools/call satellites_info. Verifies auth
// enforcement (no header = 401) and the tool payload shape.
func TestMCPInfoToolRespondsOverHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	baseURL, stop := startServerContainerWithEnv(t, ctx, map[string]string{
		"SATELLITES_API_KEYS": "key_test",
	})
	defer stop()

	mcpURL := baseURL + "/mcp"

	// 1. Unauthenticated request must 401.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauth request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("unauth response missing WWW-Authenticate")
	}

	// 2. Initialize the MCP session.
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	}
	initResp := rpcCall(t, ctx, mcpURL, "key_test", initReq)
	if initResp["error"] != nil {
		t.Fatalf("initialize error: %v", initResp["error"])
	}

	// 3. tools/list must include satellites_info; the retired
	// story_*-prefixed verb names from story_775a7b49 must be absent
	// from whatever the server advertises.
	listReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	}
	listResp := rpcCall(t, ctx, mcpURL, "key_test", listReq)
	if listResp["error"] != nil {
		t.Fatalf("tools/list error: %v", listResp["error"])
	}
	result, _ := listResp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	advertised := make(map[string]bool, len(tools))
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if name, _ := tool["name"].(string); name != "" {
				advertised[name] = true
			}
		}
	}
	if !advertised["satellites_info"] {
		t.Fatalf("tools/list did not advertise satellites_info; got %v", advertised)
	}
	// story_775a7b49: ensure none of the retired prefixed names leaked
	// back into the registration. The bare DEV container does not wire
	// a DB so the contract-family verbs are not advertised here; the
	// new-names-present check lives in the contractFixture-backed unit
	// test TestRegisteredToolNames in internal/mcpserver.
	for _, gone := range []string{
		"story_workflow_claim",
		"story_contract_next",
		"story_contract_claim",
		"story_contract_close",
		"story_contract_resume",
		"story_contract_respond",
	} {
		if advertised[gone] {
			t.Errorf("tools/list still advertises retired name %q (story_775a7b49 rename)", gone)
		}
	}

	// 4. tools/call satellites_info returns the version payload.
	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "satellites_info",
			"arguments": map[string]any{},
		},
	}
	callResp := rpcCall(t, ctx, mcpURL, "key_test", callReq)
	if callResp["error"] != nil {
		t.Fatalf("tools/call error: %v", callResp["error"])
	}
	callResult, _ := callResp["result"].(map[string]any)
	content, _ := callResult["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tools/call result missing content: %+v", callResult)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("payload decode: %v; raw=%s", err, text)
	}
	// sty_0be97c3e: payload is now {server, caller, recent_activity}.
	for _, k := range []string{"server", "caller", "recent_activity"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("payload missing %q: %+v", k, payload)
		}
	}
	server, _ := payload["server"].(map[string]any)
	for _, k := range []string{"version", "build", "commit", "started_at"} {
		if _, ok := server[k]; !ok {
			t.Errorf("server block missing %q: %+v", k, server)
		}
	}
	// AC1: the AuthMiddleware-resolved CallerIdentity must reach the
	// tool handler ctx. For the env-keyset bearer the path stamps the
	// "apikey" sentinel email; the regression is the field being empty.
	caller, _ := payload["caller"].(map[string]any)
	if caller == nil {
		t.Fatalf("payload missing caller block: %+v", payload)
	}
	if email, _ := caller["email"].(string); email == "" {
		t.Errorf("caller.email empty over apikey bearer — context propagation regressed")
	}
	if authKind, _ := caller["auth_kind"].(string); authKind == "" {
		t.Errorf("caller.auth_kind empty — source not propagated")
	}
}

// rpcCall posts a JSON-RPC request to mcpURL with the API key, returns the
// parsed first JSON-RPC response (the mcp-go Streamable server may emit
// either application/json or text/event-stream; both are handled).
func rpcCall(t *testing.T, ctx context.Context, mcpURL, apiKey string, body any) map[string]any {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("rpc status = %d; body=%s", resp.StatusCode, string(b))
	}
	raw, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		// Parse the first data: line.
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				var out map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &out); err != nil {
					t.Fatalf("sse decode: %v; raw=%s", err, string(raw))
				}
				return out
			}
		}
		t.Fatalf("no data: line in SSE response; raw=%s", string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json decode: %v; raw=%s", err, string(raw))
	}
	return out
}

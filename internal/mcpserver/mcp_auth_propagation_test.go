// sty_0be97c3e: AuthMiddleware resolves a CallerIdentity onto
// r.Context(), but mcp-go's StreamableHTTPServer builds the
// tool-handler ctx via WithContext(r.Context(), session), which does
// not propagate values. Without WithHTTPContextFunc, auth.UserFrom(ctx)
// inside tool handlers returns (_, false) even for authed requests.
// These tests pin both the function-level shim and the end-to-end
// pipeline through s.streamable.ServeHTTP.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/auth"
)

// TestPropagateCallerIdentity_FunctionLevel pins the hook itself:
// values stamped on r.Context() must ride onto the returned ctx.
func TestPropagateCallerIdentity_FunctionLevel(t *testing.T) {
	t.Parallel()
	want := auth.CallerIdentity{
		UserID: "u_alice",
		Email:  "alice@local",
		Source: "apikey:apk_test01",
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rctx := auth.WithCaller(r.Context(), want)
	rctx = withScopedProjectID(rctx, "proj_test")
	rctx = withRequestBaseURL(rctx, "https://example.invalid")
	r = r.WithContext(rctx)

	got := propagateCallerIdentity(context.Background(), r)

	caller, ok := auth.UserFrom(got)
	if !ok {
		t.Fatalf("propagateCallerIdentity dropped CallerIdentity")
	}
	if caller != want {
		t.Errorf("CallerIdentity = %+v, want %+v", caller, want)
	}
	if scoped := ScopedProjectIDFrom(got); scoped != "proj_test" {
		t.Errorf("scoped project_id = %q, want proj_test", scoped)
	}
	if base := requestBaseURLFrom(got); base != "https://example.invalid" {
		t.Errorf("request base url = %q, want https://example.invalid", base)
	}
}

// TestPropagateCallerIdentity_NoCallerNoOp pins that the hook is a
// no-op when AuthMiddleware did not stamp an identity. mcp-go must
// still receive a usable ctx for unauthenticated transports (the
// HTTP layer's 401 is the gate, not this hook).
func TestPropagateCallerIdentity_NoCallerNoOp(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	got := propagateCallerIdentity(context.Background(), r)
	if _, ok := auth.UserFrom(got); ok {
		t.Errorf("propagateCallerIdentity invented a caller on empty request ctx")
	}
}

// TestStreamableServer_PropagatesCallerToToolCtx is the regression
// guard for the AC1 root cause: AuthMiddleware stamps the identity on
// r.Context(); the mcp-go session-context boundary must carry it
// through to the tool handler's ctx. Mount the real Server inside
// AuthMiddleware, fire an initialize + tools/call satellites_info
// over the streamable HTTP surface bearing Authorization: Bearer
// key_valid, and assert the resulting payload's caller.email is
// non-empty. Forgetting WithHTTPContextFunc regresses this assertion
// to "".
func TestStreamableServer_PropagatesCallerToToolCtx(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)

	mw := auth.AuthMiddleware(auth.AuthDeps{
		Sessions: auth.NewMemorySessionStore(),
		Users:    auth.NewMemoryUserStore(),
		APIKeys:  []string{"key_valid"},
	})
	ts := httptest.NewServer(mw(f.server))
	defer ts.Close()

	// initialize handshake
	initBody := mustMarshal(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "unit-test", "version": "0.0.1"},
		},
	})
	if got := rpcCallUnit(t, ts.URL+"/mcp", "key_valid", initBody); got["error"] != nil {
		t.Fatalf("initialize: %+v", got["error"])
	}

	callBody := mustMarshal(t, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "satellites_info",
			"arguments": map[string]any{},
		},
	})
	resp := rpcCallUnit(t, ts.URL+"/mcp", "key_valid", callBody)
	if resp["error"] != nil {
		t.Fatalf("tools/call: %+v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %+v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("payload decode: %v; raw=%s", err, text)
	}
	caller, _ := payload["caller"].(map[string]any)
	if caller == nil {
		t.Fatalf("payload missing caller block: %s", text)
	}
	// The env-keyset APIKeys path stamps CallerIdentity{UserID:"apikey",
	// Email:"apikey", Source:"apikey"}. The propagation hook MUST carry
	// the identity into the tool ctx — pre-fix this field was "".
	if userID, _ := caller["user_id"].(string); userID == "" {
		t.Errorf("caller.user_id empty — WithHTTPContextFunc hook not wired: %s", text)
	}
	if authKind, _ := caller["auth_kind"].(string); authKind == "" {
		t.Errorf("caller.auth_kind empty — caller source not propagated: %s", text)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func rpcCallUnit(t *testing.T, url, apiKey string, body []byte) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rpc status = %d; body=%s", resp.StatusCode, string(raw))
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
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
		t.Fatalf("no data: line in SSE; raw=%s", string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json decode: %v; raw=%s", err, string(raw))
	}
	return out
}

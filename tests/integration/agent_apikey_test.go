package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestAgentAPIKey_EndToEnd drives sty_3191fbfc against a real
// testcontainers satellites server:
//
//  1. Boot Surreal + satellites server in dev mode.
//  2. Login → session cookie.
//  3. Call agent_apikey_create with the cookie. Capture the
//     cleartext `key`.
//  4. Call satellites_info with Authorization: Bearer <key> and NO
//     cookie. Assert 200.
//  5. Call agent_apikey_list with the cookie. Assert the row
//     carries the prefix + name and DOES NOT carry the cleartext.
//  6. Call agent_apikey_delete with the cookie.
//  7. Re-call satellites_info with Bearer <key>. Assert 401.
func TestAgentAPIKey_EndToEnd(t *testing.T) {
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

	mcpURL := baseURL + "/mcp"

	// 1. Login → session cookie.
	cookie := devLogin(t, ctx, baseURL, "dev@local", "letmein")

	// 2. agent_apikey_create with the cookie.
	mintResp := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_create", map[string]any{
		"name": "dogfood-key",
	})
	cleartext, _ := mintResp["key"].(string)
	keyID, _ := mintResp["id"].(string)
	prefix, _ := mintResp["prefix"].(string)
	if cleartext == "" || keyID == "" || prefix == "" {
		t.Fatalf("create resp missing fields: %+v", mintResp)
	}
	if !strings.HasPrefix(cleartext, "sat_") {
		t.Errorf("cleartext %q missing sat_ prefix", cleartext)
	}
	if len(cleartext) < 40 {
		t.Errorf("cleartext len = %d, want >= 40", len(cleartext))
	}

	// 3. satellites_info with Bearer <key> and NO cookie. Initialize
	//    first so the streamable HTTP server has the protocol handshake.
	rpcInit(t, ctx, mcpURL, cleartext)
	infoResp := callTool(t, ctx, mcpURL, cleartext, "satellites_info", map[string]any{})
	server, _ := infoResp["server"].(map[string]any)
	if _, ok := server["version"]; !ok {
		t.Errorf("info response missing server.version: %+v", infoResp)
	}
	// sty_0be97c3e — AC4: the substrate apikey path resolves to the
	// owner's email, not the literal "apikey" sentinel from the
	// env-keyset path. Confirms the WithHTTPContextFunc hook carries
	// the resolved CallerIdentity through to the tool ctx.
	caller, _ := infoResp["caller"].(map[string]any)
	if email, _ := caller["email"].(string); email != "dev@local" {
		t.Errorf("caller.email = %q, want dev@local", email)
	}
	if authKind, _ := caller["auth_kind"].(string); !strings.HasPrefix(authKind, "apikey:") {
		t.Errorf("caller.auth_kind = %q, want apikey:<id> prefix", authKind)
	}

	// 4. agent_apikey_list with the cookie. Assert no cleartext leak.
	listResp := callToolRawWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_list", map[string]any{})
	listText := extractToolText(t, listResp)
	if !strings.Contains(listText, prefix) {
		t.Errorf("list response missing prefix %q: %s", prefix, listText)
	}
	if strings.Contains(listText, cleartext) {
		t.Errorf("list response leaks cleartext key: %s", listText)
	}
	for _, forbidden := range []string{`"key_hash"`, `"key_salt"`} {
		if strings.Contains(listText, forbidden) {
			t.Errorf("list response leaks %q: %s", forbidden, listText)
		}
	}

	// 5. agent_apikey_delete with the cookie.
	delResp := callToolWithCookie(t, ctx, mcpURL, cookie, "agent_apikey_delete", map[string]any{
		"id": keyID,
	})
	if status, _ := delResp["status"].(string); status != "archived" {
		t.Errorf("delete status = %q, want archived", status)
	}

	// 6. Re-call satellites_info with Bearer <key>. Assert 401.
	if got := bearerStatus(t, ctx, mcpURL, cleartext); got != http.StatusUnauthorized {
		t.Errorf("post-delete bearer status = %d, want 401", got)
	}
}

// devLogin POSTs the dev credentials and returns the
// satellites_session cookie.
func devLogin(t *testing.T, ctx context.Context, baseURL, user, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {user}, "password": {password}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "satellites_session" {
			return c
		}
	}
	t.Fatal("login did not return a session cookie")
	return nil
}

// callToolWithCookie posts a tools/call using the session cookie
// rather than a Bearer api-key.
func callToolWithCookie(t *testing.T, ctx context.Context, mcpURL string, cookie *http.Cookie, name string, args map[string]any) map[string]any {
	t.Helper()
	resp := callToolRawWithCookie(t, ctx, mcpURL, cookie, name, args)
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

// callToolRawWithCookie is the cookie-auth variant of callToolRaw.
func callToolRawWithCookie(t *testing.T, ctx context.Context, mcpURL string, cookie *http.Cookie, name string, args map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0", "id": time.Now().UnixNano(), "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rpc status = %d; body=%s", resp.StatusCode, string(raw))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
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

// bearerStatus POSTs an initialize/tools-call with the Bearer and
// returns the HTTP status. Used to assert 401 after a key has been
// archived.
func bearerStatus(t *testing.T, ctx context.Context, mcpURL, bearer string) int {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "agent-apikey-test", "version": "0.0.1"},
		},
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

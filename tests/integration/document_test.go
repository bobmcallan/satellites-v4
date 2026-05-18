package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestDocumentIngestAndGetRoundTrip spins up SurrealDB next to the
// satellites server in a shared network, waits for both to be ready, then
// drives document_ingest_file via /api/v1 (post-sty_4db0e025 slice B3
// it is HTTP/CLI-only) and document_get over MCP. Asserts body
// round-trip + boot-seed entry present.
func TestDocumentIngestAndGetRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv: map[string]string{"SATELLITES_API_KEYS": "key_doc"},
	})
	defer stack.Stop()
	baseURL := stack.BaseURL

	mcpURL := baseURL + "/mcp"

	// 1. Initialize.
	init := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
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

	// 2. tools/list must include document_get and satellites_info.
	// (document_ingest_file MCP registration was dropped in
	// sty_4db0e025 slice B3 — verify it is NOT advertised over MCP.)
	list := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result, _ := list["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	need := map[string]bool{"satellites_info": false, "document_get": false}
	mustAbsent := map[string]bool{"document_ingest_file": false}
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if name, _ := tool["name"].(string); name != "" {
				if _, tracked := need[name]; tracked {
					need[name] = true
				}
				if _, dropped := mustAbsent[name]; dropped {
					mustAbsent[name] = true
				}
			}
		}
	}
	for k, seen := range need {
		if !seen {
			t.Errorf("tools/list missing %q", k)
		}
	}
	for k, seen := range mustAbsent {
		if seen {
			t.Errorf("tools/list advertises %q which was removed in sty_4db0e025 B3", k)
		}
	}

	// 3. document_get architecture.md — should be seeded.
	getResp := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_get",
			"arguments": map[string]any{"name": "architecture.md"},
		},
	})
	body := extractToolText(t, getResp)
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode document_get body: %v; raw=%s", err, body)
	}
	if got, _ := doc["name"].(string); got != "architecture.md" {
		t.Errorf("seeded doc name = %q, want architecture.md", got)
	}
	if got, _ := doc["type"].(string); got != "artifact" {
		t.Errorf("seeded doc type = %q, want artifact", got)
	}
	if got, _ := doc["scope"].(string); got != "project" {
		t.Errorf("seeded doc scope = %q, want project", got)
	}
	seededBody, _ := doc["body"].(string)
	if !strings.Contains(seededBody, "Satellites v4 — Architecture") {
		t.Errorf("seeded doc body missing architecture heading; got %d bytes", len(seededBody))
	}

	// 4. document_ingest_file architecture.md again over /api/v1 — same
	// hash → no-op. (MCP registration dropped in sty_4db0e025 B3.)
	ingest := callAPIv1(t, ctx, baseURL, "key_doc", "document_ingest_file", map[string]any{
		"path": "architecture.md",
	})
	if ingest["changed"] != false {
		t.Errorf("re-ingest changed=%v, want false (hash match)", ingest["changed"])
	}

	// 5. Path traversal must be rejected by the /api/v1 handler.
	traversePath := apiPathForToolName("document_ingest_file")
	traverseBody, _ := json.Marshal(map[string]any{"path": "../etc/passwd"})
	traverseReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1"+traversePath, bytes.NewReader(traverseBody))
	traverseReq.Header.Set("Content-Type", "application/json")
	traverseReq.Header.Set("Authorization", "Bearer key_doc")
	traverseResp, err := http.DefaultClient.Do(traverseReq)
	if err != nil {
		t.Fatalf("traverse request: %v", err)
	}
	defer traverseResp.Body.Close()
	if traverseResp.StatusCode == http.StatusOK {
		t.Errorf("traversal returned 200; want a non-2xx status code")
	}
}

func extractToolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool response missing content: %+v", resp)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if text == "" {
		t.Fatalf("tool response content[0].text empty: %+v", resp)
	}
	return text
}

// startOptions allows the document integration test to attach the server
// container to a user network so it can reach the surreal container and to
// bind-mount the repo-side docs/ tree.
type startOptions struct {
	Network string
	Env     map[string]string
	Mounts  []mount.Mount
}

func startServerContainerWithOptions(t *testing.T, ctx context.Context, opts startOptions) (string, func()) {
	t.Helper()
	root := repoRoot(t)
	env := map[string]string{
		"SATELLITES_PORT":      "8080",
		"SATELLITES_ENV":       "dev",
		"SATELLITES_LOG_LEVEL": "info",
		"SATELLITES_DEV_MODE":  "true",
	}
	for k, v := range opts.Env {
		env[k] = v
	}
	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    root,
			Dockerfile: "docker/Dockerfile",
			KeepImage:  true,
		},
		ExposedPorts: []string{"8080/tcp"},
		Env:          env,
		WaitingFor: wait.ForHTTP("/healthz").
			WithPort("8080/tcp").
			WithStartupTimeout(120 * time.Second),
	}
	if opts.Network != "" {
		req.Networks = []string{opts.Network}
	}
	if len(opts.Mounts) > 0 {
		mounts := opts.Mounts
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.Mounts = append(hc.Mounts, mounts...)
		}
	}
	cont, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start container: %v", err)
	}
	host, err := cont.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mapped, err := cont.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, mapped.Port())
	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = cont.Terminate(stopCtx)
	}
	return baseURL, stop
}

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestDocumentIngestAndGetRoundTrip spins up SurrealDB next to the
// satellites server in a shared network, waits for both to be ready, then
// drives document_ingest_file + document_get over the MCP HTTP endpoint
// with an API key. Asserts body round-trip + boot-seed entry present.
func TestDocumentIngestAndGetRoundTrip(t *testing.T) {
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

	// SurrealDB.
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

	// Satellites with DB_DSN pointing at the surreal alias + docs/ mounted.
	docsHost := filepath.Join(repoRoot(t), "docs")
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"DB_DSN":              "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_doc",
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

	// 2. tools/list must include document_ingest_file + document_get.
	list := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result, _ := list["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	need := map[string]bool{"satellites_info": false, "document_ingest_file": false, "document_get": false}
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

	// 3. document_get architecture.md — should be seeded.
	getResp := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_get",
			"arguments": map[string]any{"filename": "architecture.md"},
		},
	})
	body := extractToolText(t, getResp)
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode document_get body: %v; raw=%s", err, body)
	}
	if filename, _ := doc["filename"].(string); filename != "architecture.md" {
		t.Errorf("seeded doc filename = %q, want architecture.md", filename)
	}
	seededBody, _ := doc["body"].(string)
	if !strings.Contains(seededBody, "Satellites v4 — Architecture") {
		t.Errorf("seeded doc body missing architecture heading; got %d bytes", len(seededBody))
	}

	// 4. document_ingest_file architecture.md again — same hash → no-op.
	ingestResp := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_ingest_file",
			"arguments": map[string]any{"path": "architecture.md"},
		},
	})
	ingestBody := extractToolText(t, ingestResp)
	var ingest map[string]any
	if err := json.Unmarshal([]byte(ingestBody), &ingest); err != nil {
		t.Fatalf("decode ingest: %v; raw=%s", err, ingestBody)
	}
	if ingest["changed"] != false {
		t.Errorf("re-ingest changed=%v, want false (hash match)", ingest["changed"])
	}

	// 5. Path traversal must be rejected.
	traverse := rpcCall(t, ctx, mcpURL, "key_doc", map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{
			"name":      "document_ingest_file",
			"arguments": map[string]any{"path": "../etc/passwd"},
		},
	})
	traverseResult, _ := traverse["result"].(map[string]any)
	if isErr, _ := traverseResult["isError"].(bool); !isErr {
		t.Errorf("traversal should return isError=true; got %+v", traverseResult)
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
		"PORT":      "8080",
		"ENV":       "dev",
		"LOG_LEVEL": "info",
		"DEV_MODE":  "true",
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

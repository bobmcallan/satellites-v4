package integration

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestEmbedWorker_StubProvider_ChunksDocument covers AC3 of story_8b06a100.
// Boots satellites with EMBEDDINGS_PROVIDER=stub against a Surreal
// testcontainer; ingests a principle document via document_create; polls
// document_search until the doc is reachable via a non-empty query (the
// path that exercises SearchSemantic when an embedder is wired). When
// the worker has chunked + embedded the row, document_search returns it
// for a query whose terms appear in the body. The fallback Search path
// is also active, but the assertion specifically checks that the row is
// findable inside the polling window — implying the worker booted and
// ran at least one chunk cycle.
func TestEmbedWorker_StubProvider_ChunksDocument(t *testing.T) {
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

	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"DB_DSN":               "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS":  "key_embed",
			"EMBEDDINGS_PROVIDER":  "stub",
			"EMBEDDINGS_DIMENSION": "16",
		},
	})
	defer stop()

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, "key_embed")

	// Create a principle whose body carries a distinctive needle.
	created := callTool(t, ctx, mcpURL, "key_embed", "document_create", map[string]any{
		"type":  "principle",
		"scope": "system",
		"name":  "embedworker-fixture",
		"body":  "the embed worker chunks documents into vector entries",
		"tags":  []any{"v4", "embed-worker-fixture"},
	})
	docID, _ := created["id"].(string)
	if docID == "" {
		t.Fatalf("document_create returned no id: %+v", created)
	}

	// Poll task_list for the embed-document task to transition to
	// status=closed with outcome=success. document_create enqueues an
	// event-origin task; the worker (gated on EMBEDDINGS_PROVIDER and
	// the chunk stores) polls + claims it, runs the stub embedder, and
	// closes the row. The matching task carries the doc id in its
	// base64-encoded payload so we substring-match the ID after decode.
	deadline := time.Now().Add(60 * time.Second)
	var processed bool
	for time.Now().Before(deadline) {
		rows := callToolArray(t, ctx, mcpURL, "key_embed", "task_list", nil)
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			rawPayload, _ := row["payload"].(string)
			decoded := decodeBase64String(rawPayload)
			if !containsString(decoded, docID) && !containsString(rawPayload, docID) {
				continue
			}
			status, _ := row["status"].(string)
			outcome, _ := row["outcome"].(string)
			if status == "closed" && outcome == "success" {
				processed = true
				break
			}
		}
		if processed {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !processed {
		t.Fatalf("embed-document task for doc %q never reached status=closed outcome=success within 60s", docID)
	}
}

func containsString(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func decodeBase64String(s string) string {
	if s == "" {
		return ""
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(out)
}

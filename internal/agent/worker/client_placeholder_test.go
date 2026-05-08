package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/config"
)

// recordedCall captures one MCP tools/call request. The stub server
// pushes one entry per request onto a slice the test then asserts on.
type recordedCall struct {
	Tool    string
	Args    map[string]any
	AuthHdr string
}

// mcpStub returns an httptest server that records every tools/call
// request and replies with the supplied result text. respond is keyed
// by tool name; absent → empty result.
func mcpStub(t *testing.T, respond map[string]string) (*httptest.Server, *[]recordedCall, *sync.Mutex) {
	t.Helper()
	calls := make([]recordedCall, 0, 4)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		mu.Lock()
		calls = append(calls, recordedCall{
			Tool: req.Params.Name, Args: req.Params.Arguments, AuthHdr: r.Header.Get("Authorization"),
		})
		mu.Unlock()
		text := respond[req.Params.Name]
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

func TestPlaceholderClient_Execute_WritesNoopRow(t *testing.T) {
	srv, calls, mu := mcpStub(t, nil)

	client := worker.NewPlaceholderClient(config.AgentConfig{
		MCPURL:         srv.URL,
		AuthToken:      "tok-noop",
		ExecuteTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outcome, err := client.Execute(ctx, worker.TaskEnvelope{
		ID: "task_test_x", WorkspaceID: "wksp_a", ProjectID: "proj_q",
	})
	require.NoError(t, err)
	assert.Equal(t, worker.OutcomeSuccess, outcome)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *calls, 1, "Execute should issue exactly one ledger_append")
	c := (*calls)[0]
	assert.Equal(t, "ledger_append", c.Tool)
	assert.Equal(t, "Bearer tok-noop", c.AuthHdr, "auth_token threaded as Bearer header")
	tags, ok := c.Args["tags"].([]any)
	require.True(t, ok, "tags must marshal back as []any")
	tagStrs := make([]string, len(tags))
	for i, t := range tags {
		tagStrs[i] = t.(string)
	}
	assert.Contains(t, tagStrs, "kind:agent-noop")
	assert.Contains(t, tagStrs, "task_id:task_test_x")
}

func TestPlaceholderClient_Execute_NoSubprocess(t *testing.T) {
	// Sentinel — assert the placeholder file does NOT import os/exec.
	// Reading the source string at test time is enough; the Go runtime
	// would already have failed to link if os/exec.Command had been
	// invoked from within Execute earlier.
	body, err := io.ReadAll(mustOpen(t, "client_placeholder.go"))
	require.NoError(t, err)
	assert.NotContains(t, string(body), `"os/exec"`,
		"placeholder client must not import os/exec — subprocess execution is order:03")
}

func TestPlaceholderClient_Claim_EmptyQueue_ReturnsNil(t *testing.T) {
	srv, _, _ := mcpStub(t, map[string]string{"task_claim": "null"})

	client := worker.NewPlaceholderClient(config.AgentConfig{
		MCPURL: srv.URL, ExecuteTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	env, err := client.Claim(ctx, "worker_a", []string{"wksp_a"})
	require.NoError(t, err)
	assert.Nil(t, env, "empty queue must surface as (nil, nil)")
}

func TestPlaceholderClient_Claim_TaskReturned(t *testing.T) {
	srv, _, _ := mcpStub(t, map[string]string{
		"task_claim": `{"id":"task_x","workspace_id":"wksp_a","project_id":"proj_q","origin":"scheduled"}`,
	})

	client := worker.NewPlaceholderClient(config.AgentConfig{
		MCPURL: srv.URL, ExecuteTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	env, err := client.Claim(ctx, "worker_a", []string{"wksp_a"})
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, "task_x", env.ID)
	assert.Equal(t, "wksp_a", env.WorkspaceID)
	assert.Equal(t, "proj_q", env.ProjectID)
	assert.Equal(t, "scheduled", env.Origin)
}

func TestPlaceholderClient_Heartbeat_WritesRow(t *testing.T) {
	srv, calls, mu := mcpStub(t, nil)

	client := worker.NewPlaceholderClient(config.AgentConfig{
		MCPURL: srv.URL, ExecuteTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Heartbeat(ctx, "worker_h", "task_h", "wksp_h"))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *calls, 1)
	c := (*calls)[0]
	assert.Equal(t, "ledger_append", c.Tool)
	tagsAny, _ := c.Args["tags"].([]any)
	tagStrs := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tagStrs[i] = v.(string)
	}
	assert.Contains(t, tagStrs, "kind:worker-heartbeat")
	assert.Contains(t, tagStrs, "task_id:task_h")
	assert.Contains(t, tagStrs, "worker_id:worker_h")
}

func TestPlaceholderClient_Close_SendsTaskUpdate(t *testing.T) {
	srv, calls, mu := mcpStub(t, nil)
	client := worker.NewPlaceholderClient(config.AgentConfig{
		MCPURL: srv.URL, ExecuteTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Close(ctx, "task_x", "worker_a", worker.OutcomeSuccess))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *calls, 1)
	c := (*calls)[0]
	assert.Equal(t, "task_update", c.Tool)
	assert.Equal(t, "task_x", c.Args["id"])
	assert.Equal(t, "closed", c.Args["status"])
	assert.Equal(t, "success", c.Args["outcome"])
}

func TestPlaceholderClient_Shutdown_WritesShutdownRow(t *testing.T) {
	srv, calls, mu := mcpStub(t, nil)
	client := worker.NewPlaceholderClient(config.AgentConfig{
		MCPURL: srv.URL, ExecuteTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Shutdown(ctx, "worker_a", "ctx canceled"))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *calls, 1)
	c := (*calls)[0]
	assert.Equal(t, "ledger_append", c.Tool)
	tagsAny, _ := c.Args["tags"].([]any)
	tagStrs := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tagStrs[i] = v.(string)
	}
	assert.Contains(t, tagStrs, "kind:worker-shutdown")
	assert.Contains(t, strings.Join(tagStrs, " "), "worker_id:worker_a")
}

// mustOpen opens a file in the package source dir at test time. Used
// by the no-subprocess sentinel.
func mustOpen(t *testing.T, name string) io.ReadCloser {
	t.Helper()
	f, err := openFile(name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

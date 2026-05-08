package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpRecord is a single tools/call request the stub captured.
type mcpRecord struct {
	Tool string
	Args map[string]any
}

// stubServers spins up an httptest MCP and WS pair, writes a
// matching agent.toml into a temp dir, and returns the path. The
// test cleanup tears everything down.
func stubServers(t *testing.T) (cfgPath string, mcpCalls *[]mcpRecord, mu *sync.Mutex, wsConns *atomic.Int32) {
	t.Helper()
	calls := make([]mcpRecord, 0, 8)
	var lock sync.Mutex
	wsCount := atomic.Int32{}

	respondTaskClaim := atomic.Value{}
	respondTaskClaim.Store("null") // empty queue by default

	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		lock.Lock()
		calls = append(calls, mcpRecord{Tool: req.Params.Name, Args: req.Params.Arguments})
		lock.Unlock()

		text := ""
		if req.Params.Name == "task_claim" {
			text = respondTaskClaim.Load().(string)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		})
	}))
	t.Cleanup(mcp.Close)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var sub map[string]any
		_ = conn.ReadJSON(&sub)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ws.Close)

	wsURL, _ := url.Parse(ws.URL)
	wsURL.Scheme = "ws"
	wsURL.Path = "/ws"

	tmp := t.TempDir()
	cfgPath = filepath.Join(tmp, "agent.toml")
	body := []byte(`worker_id = "test-worker"
workspace_ids = ["wksp_a"]
mcp_url = "` + mcp.URL + `"
auth_token = "tok-test"
idle_backoff = "20ms"
heartbeat_interval = "1h"
execute_timeout = "5s"
log_level = "info"

hub_url = "` + wsURL.String() + `"
subscribe_workspace_ids = ["wksp_a"]
ws_reconnect_min_backoff = "10ms"
ws_reconnect_max_backoff = "50ms"
`)
	require.NoError(t, os.WriteFile(cfgPath, body, 0o600))
	return cfgPath, &calls, &lock, &wsCount
}

func TestMain_Run_BootShutdown_ContextCancel(t *testing.T) {
	cfgPath, calls, mu, wsCount := stubServers(t)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"satellites-agent", "--config", cfgPath}, stdout, stderr)
	}()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*calls) > 0 && wsCount.Load() > 0
	}, 3*time.Second, 20*time.Millisecond, "agent never called MCP or WS")

	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code, "stderr=%q", stderr.String())
	case <-time.After(3 * time.Second):
		t.Fatalf("run did not exit within 3s after cancel; stderr=%q", stderr.String())
	}

	mu.Lock()
	defer mu.Unlock()
	sawShutdown := false
	for _, c := range *calls {
		if c.Tool != "ledger_append" {
			continue
		}
		tags, _ := c.Args["tags"].([]any)
		for _, tag := range tags {
			if s, _ := tag.(string); strings.Contains(s, "worker-shutdown") {
				sawShutdown = true
			}
		}
	}
	assert.True(t, sawShutdown, "expected one ledger_append with kind:worker-shutdown after graceful exit")
}

func TestMain_Run_BootShutdown_SIGINT(t *testing.T) {
	cfgPath, _, _, _ := stubServers(t)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	// Wire signal.NotifyContext just like the production main(); the
	// test then SIGINTs its own process to exercise the actual signal
	// path.
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"satellites-agent", "--config", cfgPath}, stdout, stderr)
	}()

	time.Sleep(150 * time.Millisecond)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	select {
	case code := <-done:
		assert.Equal(t, 0, code, "stderr=%q", stderr.String())
	case <-time.After(3 * time.Second):
		t.Fatalf("run did not exit within 3s after SIGINT; stderr=%q", stderr.String())
	}
}

func TestMain_Run_NoHubURL_PollingFallback(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "null"}}},
		})
	}))
	defer mcp.Close()

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "agent.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`worker_id = "p"
mcp_url = "`+mcp.URL+`"
hub_url = ""
idle_backoff = "20ms"
heartbeat_interval = "1h"
execute_timeout = "5s"
`), 0o600))

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	code := run(ctx, []string{"satellites-agent", "--config", cfgPath}, stdout, stderr)
	assert.Equal(t, 0, code, "stderr=%q", stderr.String())
}

func TestMain_Run_ConfigFlagMissing_ReturnsErr(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := run(ctx, []string{"satellites-agent", "--config", "/no/such/agent.toml"}, stdout, stderr)
	assert.NotZero(t, code, "explicit missing --config must surface as non-zero")
	assert.Contains(t, stderr.String(), "config")
}

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
transport = "mcp"
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
transport = "mcp"
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

// TestRun_StartupLogReflectsLoadedConfig is sty_ae1e9097's smoke
// test: the boot-time arbor log line carries the resolved
// repo_path / branch_template / worktree_root, proving the
// precedence chain wires every worktree-lifecycle field through to
// the runtime. Captures os.Stdout for the lifetime of the run() call
// — the arbor logger writes there directly (internal/arbor/logger.go).
func TestRun_StartupLogReflectsLoadedConfig(t *testing.T) {
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
	body := []byte(`worker_id = "smoke-startup-log"
mcp_url = "` + mcp.URL + `"
transport = "mcp"
hub_url = ""
idle_backoff = "20ms"
heartbeat_interval = "1h"
execute_timeout = "5s"
repo_path          = "/tmp/sty_ae1e9097/repo"
branch_template    = "agent-{task_id}-smoke"
worktree_root      = "/tmp/sty_ae1e9097/worktrees/"
claude_binary_path = "/opt/claude/bin/claude"
`)
	require.NoError(t, os.WriteFile(cfgPath, body, 0o600))

	// Redirect fd 2 (stderr) to a pipe so we can read the arbor
	// logger's boot line. arbor wraps phuslu/log's ConsoleWriter,
	// which defaults to os.Stderr; swapping the os package variable
	// doesn't redirect the underlying fd, so dup2 the pipe over fd 2
	// to redirect the kernel-level destination.
	r, wr, err := os.Pipe()
	require.NoError(t, err)
	savedStderr, err := syscall.Dup(int(os.Stderr.Fd()))
	require.NoError(t, err)
	require.NoError(t, syscall.Dup2(int(wr.Fd()), int(os.Stderr.Fd())))
	// Close the wr handle once dup2 has captured fd 2 — the kernel
	// keeps the pipe alive via fd 2, and we still want EOF on r once
	// fd 2 is restored at test end.
	wr.Close()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"satellites-agent", "--config", cfgPath}, stdout, stderr)
	}()

	captured := &bytes.Buffer{}
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(captured, r)
		close(readDone)
	}()

	select {
	case code := <-done:
		assert.Equal(t, 0, code, "stderr=%q", stderr.String())
	case <-time.After(2 * time.Second):
		t.Fatalf("run did not exit within 2s")
	}
	// Restore fd 2 to the original stderr. dup2 implicitly closes the
	// existing fd 2 (the pipe) which signals EOF to the reader.
	require.NoError(t, syscall.Dup2(savedStderr, int(os.Stderr.Fd())))
	syscall.Close(savedStderr)
	<-readDone

	logged := captured.String()
	assert.Contains(t, logged, "/tmp/sty_ae1e9097/repo", "repo_path missing from startup log: %s", logged)
	assert.Contains(t, logged, "agent-{task_id}-smoke", "branch_template missing from startup log: %s", logged)
	assert.Contains(t, logged, "/tmp/sty_ae1e9097/worktrees/", "worktree_root missing from startup log: %s", logged)
}

// TestRun_LogPath_WritesFile is sty_92bfd9e6's integration smoke:
// boot the binary against a stub MCP server with log_path pointing
// at $tmpdir, assert the satellites-agent.* file is created and
// receives the startup log line. Console writer continues to fire
// (verified via fd 2 capture analogous to the prior test).
func TestRun_LogPath_WritesFile(t *testing.T) {
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
	logDir := filepath.Join(tmp, "agent-logs")
	cfgPath := filepath.Join(tmp, "agent.toml")
	body := []byte(`worker_id = "smoke-logpath"
mcp_url = "` + mcp.URL + `"
transport = "mcp"
hub_url = ""
idle_backoff = "20ms"
heartbeat_interval = "1h"
execute_timeout = "5s"
log_path = "` + logDir + `"
`)
	require.NoError(t, os.WriteFile(cfgPath, body, 0o600))

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{"satellites-agent", "--config", cfgPath}, stdout, stderr)
	assert.Equal(t, 0, code, "stderr=%q", stderr.String())

	// Tolerate phuslu's async flush: poll for the file under logDir.
	deadline := time.Now().Add(2 * time.Second)
	var foundName string
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(logDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "satellites-agent") {
					foundName = e.Name()
					break
				}
			}
		}
		if foundName != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotEmpty(t, foundName, "no satellites-agent.* file appeared under %s", logDir)

	contents, err := os.ReadFile(filepath.Join(logDir, foundName))
	require.NoError(t, err)
	assert.Contains(t, string(contents), "satellites-agent", "file log missing startup line: %s", contents)
	assert.Contains(t, string(contents), logDir, "file log missing log_path field: %s", contents)
}

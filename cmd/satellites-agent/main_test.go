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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliRecord is a single satellites-client invocation the stub captured.
// Tool is the inferred verb name (`<noun>_<verb>` form so legacy
// assertions keyed off the MCP tool name still match) and Args is the
// flag map decoded from argv. The stub writes one JSON line per call
// to the recorder file the test reads back at assertion time.
type cliRecord struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// stubServers builds a stub satellites-client binary, spins up a WS
// hub, writes a matching agent.toml, and returns the config path plus
// a thunk that reads the recorded CLI calls. The test cleanup tears
// everything down.
func stubServers(t *testing.T) (cfgPath string, cliCalls func() []cliRecord, wsConns *atomic.Int32) {
	t.Helper()
	wsCount := atomic.Int32{}

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
	recorderPath := filepath.Join(tmp, "cli-calls.jsonl")
	stubPath := buildStubClient(t, tmp, recorderPath)

	cfgPath = filepath.Join(tmp, "agent.toml")
	body := []byte(`worker_id = "test-worker"
workspace_ids = ["wksp_a"]
mcp_url = "http://stub-unused"
cli_binary_path = "` + stubPath + `"
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

	readCalls := func() []cliRecord {
		raw, err := os.ReadFile(recorderPath)
		if err != nil {
			return nil
		}
		var out []cliRecord
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec cliRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				out = append(out, rec)
			}
		}
		return out
	}

	return cfgPath, readCalls, &wsCount
}

// buildStubClient compiles a tiny Go program that mimics
// satellites-client for the agent daemon's view. It writes one
// jsonl record per invocation into recorderPath, then emits an empty
// envelope on stdout so cliClient sees "no task / no error". Tests
// scan the jsonl file to assert which verbs the daemon shelled.
func buildStubClient(t *testing.T, dir, recorderPath string) string {
	t.Helper()
	src := filepath.Join(dir, "stubclient.go")
	stubSrc := `package main

import (
	"encoding/json"
	"os"
	"strings"
)

// rec is the per-invocation record shape main_test.go scans.
type rec struct {
	Tool string                 ` + "`json:\"tool\"`" + `
	Args map[string]interface{} ` + "`json:\"args\"`" + `
}

func main() {
	argv := os.Args[1:]
	noun := ""
	verb := ""
	args := map[string]interface{}{}
	// strip global flags --server / --token / --json (taking the value
	// for --server / --token); the remaining positionals are
	// <noun> <verb> followed by --flag value pairs.
	i := 0
	for i < len(argv) {
		a := argv[i]
		switch a {
		case "--server", "--token":
			i += 2
			continue
		case "--json":
			i += 1
			continue
		}
		if !strings.HasPrefix(a, "--") {
			if noun == "" {
				noun = a
			} else if verb == "" {
				verb = a
			}
			i++
			continue
		}
		key := strings.TrimPrefix(a, "--")
		key = strings.ReplaceAll(key, "-", "_")
		val := ""
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			val = argv[i+1]
			i += 2
		} else {
			val = "true"
			i++
		}
		args[key] = val
	}
	tool := noun + "_" + verb
	r := rec{Tool: tool, Args: args}
	body, _ := json.Marshal(r)
	f, err := os.OpenFile(` + "`" + recorderPath + "`" + `, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.Write(body)
		_, _ = f.Write([]byte("\n"))
		_ = f.Close()
	}
	// emit an empty envelope on stdout (task claim "null", others "{}")
	if tool == "task_claim" {
		os.Stdout.WriteString("null")
	} else {
		os.Stdout.WriteString("{}")
	}
}
`
	require.NoError(t, os.WriteFile(src, []byte(stubSrc), 0o644))
	bin := filepath.Join(dir, "stubclient")
	build := exec.Command("go", "build", "-o", bin, src)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build stub: %s", out)
	return bin
}

func TestMain_Run_BootShutdown_ContextCancel(t *testing.T) {
	cfgPath, cliCalls, wsCount := stubServers(t)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"satellites-agent", "--config", cfgPath}, stdout, stderr)
	}()

	require.Eventually(t, func() bool {
		return len(cliCalls()) > 0 && wsCount.Load() > 0
	}, 3*time.Second, 20*time.Millisecond, "agent never shelled satellites-client or attached to WS")

	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code, "stderr=%q", stderr.String())
	case <-time.After(3 * time.Second):
		t.Fatalf("run did not exit within 3s after cancel; stderr=%q", stderr.String())
	}

	sawShutdown := false
	for _, c := range cliCalls() {
		if c.Tool != "ledger_append" {
			continue
		}
		tags, _ := c.Args["tags"].(string)
		if strings.Contains(tags, "worker-shutdown") {
			sawShutdown = true
		}
	}
	assert.True(t, sawShutdown, "expected one ledger_append with kind:worker-shutdown after graceful exit")
}

func TestMain_Run_BootShutdown_SIGINT(t *testing.T) {
	cfgPath, _, _ := stubServers(t)

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
	tmp := t.TempDir()
	stubPath := buildStubClient(t, tmp, filepath.Join(tmp, "cli-calls.jsonl"))

	cfgPath := filepath.Join(tmp, "agent.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`worker_id = "p"
mcp_url = "http://stub-unused"
cli_binary_path = "`+stubPath+`"
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
	tmp := t.TempDir()
	stubPath := buildStubClient(t, tmp, filepath.Join(tmp, "cli-calls.jsonl"))

	cfgPath := filepath.Join(tmp, "agent.toml")
	body := []byte(`worker_id = "smoke-startup-log"
mcp_url = "http://stub-unused"
cli_binary_path = "` + stubPath + `"
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
	tmp := t.TempDir()
	stubPath := buildStubClient(t, tmp, filepath.Join(tmp, "cli-calls.jsonl"))
	logDir := filepath.Join(tmp, "agent-logs")
	cfgPath := filepath.Join(tmp, "agent.toml")
	body := []byte(`worker_id = "smoke-logpath"
mcp_url = "http://stub-unused"
cli_binary_path = "` + stubPath + `"
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

// task_run_async_test.go — sty_57210d2a (C2) integration suite.
//
// Each test boots a real internal/clientdaemon.Daemon in-process on a
// temp unix socket (the production code path, not a mock) and execs
// the real satellites-client binary against it. The daemon's Dispatch
// closure is injectable, so the test can synthesise a long-running or
// quickly-completing subprocess without forking a real `claude`.
//
// The stub substrate is a single httptest server the daemon's
// cliremote.Client targets for task_get / task_log_append / ledger_
// append. Hermetic locally — no pprod dependency. (pr_local_iteration.)
//
// AC mapping (from sty_57210d2a):
//   - TestTaskRunAsync                       — AC1
//   - TestTaskRunAsyncSurvivesCLIExit        — AC2
//   - TestTaskRunAsyncDaemonNotRunning       — AC3
//   - TestTaskRunAsyncIdempotent             — AC5

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/clientdaemon"
	"github.com/bobmcallan/satellites/internal/config"
)

// asyncTestEnv carries the in-process daemon + stub substrate handles
// the four AC cases share. dispatchFunc lets each test tune the
// simulated subprocess (block, sleep, etc.) without re-bootstrapping
// the daemon harness.
type asyncTestEnv struct {
	t          *testing.T
	stub       *httptest.Server
	stubCalls  *stubCallRecorder
	daemon     *clientdaemon.Daemon
	socketPath string
	runCancel  context.CancelFunc
	runDone    chan error
	clientBin  string
}

// stubCallRecorder accepts every /api/v1/* request and returns the
// canned response the daemon needs.
type stubCallRecorder struct {
	mu      sync.Mutex
	calls   []string
	taskIDs map[string]map[string]any
}

func (s *stubCallRecorder) addTask(id string, env map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taskIDs == nil {
		s.taskIDs = map[string]map[string]any{}
	}
	if env == nil {
		env = map[string]any{}
	}
	env["id"] = id
	s.taskIDs[id] = env
}

func (s *stubCallRecorder) record(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, path)
}

// newAsyncTestEnv stands up the stub substrate + an in-process daemon
// on a temp socket. The caller passes a dispatchFunc that mimics the
// dispatched subprocess body for the test case.
func newAsyncTestEnv(t *testing.T, dispatchFunc clientdaemon.DispatchFunc, parallelism int) *asyncTestEnv {
	t.Helper()

	rec := &stubCallRecorder{taskIDs: map[string]map[string]any{}}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if len(body) > 0 {
			_ = json.Unmarshal(body, &parsed)
		}
		rec.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/task/get":
			id, _ := parsed["id"].(string)
			rec.mu.Lock()
			env, ok := rec.taskIDs[id]
			rec.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(env)
		case "/api/v1/task/log/append":
			_, _ = w.Write([]byte(`{"id":"tlog_x","task_id":"x","seq":0,"kind":"x","created_at":"2026-05-14T00:00:00Z"}`))
		case "/api/v1/ledger/append":
			_, _ = w.Write([]byte(`{"id":"ldg_x"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.Close)

	tmp := t.TempDir()
	api := cliremote.New(stub.URL, "test-bearer", nil)
	logger := satarbor.New("warn")

	opts := clientdaemon.Options{
		Parallelism:  parallelism,
		MaxQueue:     10,
		Heartbeat:    100 * time.Millisecond,
		DrainTimeout: 500 * time.Millisecond,
		SocketPath:   filepath.Join(tmp, "daemon.sock"),
		PidfilePath:  filepath.Join(tmp, "daemon.pid"),
		StatePath:    filepath.Join(tmp, "state.json"),
		LogfilePath:  filepath.Join(tmp, "daemon.log"),
		LogsDir:      filepath.Join(tmp, "logs"),
		AgentConfig: config.AgentConfig{
			RepoPath:         tmp,
			BranchTemplate:   "client-{task_id}-from-{base_sha}",
			WorktreeRoot:     filepath.Join(tmp, "worktree"),
			ClaudeBinaryPath: "/bin/true",
			SpawnMCPURL:      stub.URL + "/mcp",
			AuthToken:        "test-bearer",
			ExecuteTimeout:   30 * time.Second,
			LogLevel:         "warn",
		},
		API:      api,
		Logger:   logger,
		Dispatch: dispatchFunc,
	}

	d, err := clientdaemon.New(opts)
	if err != nil {
		t.Fatalf("clientdaemon.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	// Wait for the socket to bind so the CLI invocations don't race.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(opts.SocketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(opts.SocketPath); err != nil {
		cancel()
		<-runDone
		t.Fatalf("daemon never bound socket at %s: %v", opts.SocketPath, err)
	}

	env := &asyncTestEnv{
		t:          t,
		stub:       stub,
		stubCalls:  rec,
		daemon:     d,
		socketPath: opts.SocketPath,
		runCancel:  cancel,
		runDone:    runDone,
		clientBin:  buildBinary(t, "satellites-client"),
	}
	t.Cleanup(env.shutdown)
	return env
}

func (e *asyncTestEnv) shutdown() {
	e.runCancel()
	select {
	case <-e.runDone:
	case <-time.After(3 * time.Second):
	}
	e.daemon.WaitInflight()
}

// runAsyncCLI execs `satellites-client task run --async <id> --socket
// <path>` against the in-process daemon. Returns stdout, stderr, exit
// code, and wall-clock elapsed.
func (e *asyncTestEnv) runAsyncCLI(t *testing.T, taskID string) (stdout, stderr string, exit int, elapsed time.Duration) {
	t.Helper()

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "satellites-client.toml")
	toml := strings.Join([]string{
		fmt.Sprintf("server = %q", e.stub.URL),
		`token = "test-bearer"`,
		fmt.Sprintf("repo_path = %q", dir),
		`branch_template = "agent-{task_id}-from-{base_sha}"`,
		fmt.Sprintf("worktree_root = %q", filepath.Join(dir, "wt")),
		`execute_timeout = "30s"`,
		`log_level = "warn"`,
	}, "\n") + "\n"
	if err := os.WriteFile(tomlPath, []byte(toml), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}

	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	daemonHome := filepath.Join(dir, "fresh-daemon-home")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, e.clientBin, "task", "run", "--async", "--socket", e.socketPath, taskID)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"SATELLITES_CLIENT_CONFIG=" + tomlPath,
		"SATELLITES_CLIENT_SKIP_UPDATE_CHECK=1",
		"HOME=" + homeDir,
		// SATELLITES_DAEMON_HOME isolates the "no auto-spawn" assertion:
		// AC3 checks no pidfile appears under a fresh daemon home.
		"SATELLITES_DAEMON_HOME=" + daemonHome,
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	start := time.Now()
	err := cmd.Run()
	elapsed = time.Since(start)

	exit = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("exec %s: %v", e.clientBin, err)
		}
	}
	return stdoutBuf.String(), stderrBuf.String(), exit, elapsed
}

// blockingDispatch returns a DispatchFunc that holds the dispatched
// subprocess open for `hold` before returning success. AC2 / AC5 use
// it so the task remains in `running` while the test inspects state.
func blockingDispatch(hold time.Duration) clientdaemon.DispatchFunc {
	return func(ctx context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, _ worker.TaskEnvelope, _, _ io.Writer) (worker.Outcome, error) {
		select {
		case <-ctx.Done():
			return worker.OutcomeTimeout, ctx.Err()
		case <-time.After(hold):
		}
		return worker.OutcomeSuccess, nil
	}
}

// quickDispatch returns a DispatchFunc that completes near-instantly.
// AC1 uses it to avoid leaking goroutines past the test.
func quickDispatch() clientdaemon.DispatchFunc {
	return blockingDispatch(50 * time.Millisecond)
}

// TestTaskRunAsync — AC1.
//
// Boots an in-process daemon, invokes `task run --async <id>`, and
// asserts:
//   - exit code 0
//   - elapsed wall-time < 1s
//   - stdout JSON decodes to {task_id, daemon_pid, queue_position}
//   - daemon_pid == os.Getpid() (the daemon process is the test
//     process)
func TestTaskRunAsync(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go build unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix socket required")
	}

	env := newAsyncTestEnv(t, quickDispatch(), 1)
	env.stubCalls.addTask("task_ac1", map[string]any{
		"workspace_id": "wksp_ac1",
		"project_id":   "proj_ac1",
		"story_id":     "sty_57210d2a",
		"origin":       "story_stage",
	})

	stdout, stderr, exit, elapsed := env.runAsyncCLI(t, "task_ac1")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	if elapsed >= time.Second {
		t.Errorf("elapsed = %s, want < 1s", elapsed)
	}
	t.Logf("AC1 elapsed: %s", elapsed)

	var resp clientdaemon.EnqueueResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil {
		t.Fatalf("decode stdout JSON: %v (stdout=%q)", err, stdout)
	}
	if resp.TaskID != "task_ac1" {
		t.Errorf("task_id = %q, want %q", resp.TaskID, "task_ac1")
	}
	if resp.DaemonPID != os.Getpid() {
		t.Errorf("daemon_pid = %d, want %d (os.Getpid of in-process daemon)", resp.DaemonPID, os.Getpid())
	}
	if resp.QueuePosition < 0 {
		t.Errorf("queue_position = %d, want >= 0", resp.QueuePosition)
	}
}

// TestTaskRunAsyncSurvivesCLIExit — AC2.
//
// The dispatched subprocess (the blocking Dispatch closure) holds for
// 10s. The CLI returns within 1s. 5s later we issue GET /v1/status?
// task_id over the same unix socket and assert state == "running"
// and pid != 0. This proves the subprocess outlives the CLI process.
func TestTaskRunAsyncSurvivesCLIExit(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go build unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix socket required")
	}
	if testing.Short() {
		t.Skip("AC2 needs a 5s mid-window observation; skip in -short")
	}

	env := newAsyncTestEnv(t, blockingDispatch(10*time.Second), 1)
	env.stubCalls.addTask("task_survivor", map[string]any{
		"workspace_id": "wksp_ac2",
		"project_id":   "proj_ac2",
		"story_id":     "sty_57210d2a",
		"origin":       "story_stage",
	})

	stdout, stderr, exit, elapsed := env.runAsyncCLI(t, "task_survivor")
	if exit != 0 {
		t.Fatalf("CLI exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	if elapsed >= time.Second {
		t.Errorf("CLI elapsed = %s, want < 1s (AC1 invariant on the survivor case)", elapsed)
	}

	// Wait 5s — AC2's mid-window observation point. The Dispatch
	// closure holds for 10s, so we expect "running" at the 5s mark.
	time.Sleep(5 * time.Second)

	statusBody := socketGET(t, env.socketPath, "/v1/status?task_id=task_survivor")
	var ts clientdaemon.TaskStatusResponse
	if err := json.Unmarshal(statusBody, &ts); err != nil {
		t.Fatalf("decode status: %v (raw=%s)", err, statusBody)
	}
	if ts.State != "running" {
		t.Errorf("state = %q, want %q (subprocess should outlive CLI exit)", ts.State, "running")
	}
	if ts.PID == 0 {
		t.Errorf("pid = 0, want non-zero")
	}
	t.Logf("AC2 status after 5s: state=%s pid=%d", ts.State, ts.PID)
}

// TestTaskRunAsyncDaemonNotRunning — AC3.
//
// Points the CLI at a nonexistent socket path under an isolated
// SATELLITES_DAEMON_HOME. Asserts:
//   - non-zero exit code (cliexit.Server == 5)
//   - stderr contains the literal "daemon not running …" substring
//   - no daemon pidfile appears under SATELLITES_DAEMON_HOME (no
//     auto-spawn)
func TestTaskRunAsyncDaemonNotRunning(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go build unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix socket required")
	}

	bin := buildBinary(t, "satellites-client")
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "satellites-client.toml")
	toml := strings.Join([]string{
		`server = "http://127.0.0.1:1"`,
		`token = "test-bearer"`,
		`log_level = "warn"`,
	}, "\n") + "\n"
	if err := os.WriteFile(tomlPath, []byte(toml), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}

	daemonHome := filepath.Join(dir, "fresh-daemon-home")
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	absentSocket := filepath.Join(daemonHome, "satellites-client.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "task", "run", "--async", "--socket", absentSocket, "task_absent")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"SATELLITES_CLIENT_CONFIG=" + tomlPath,
		"SATELLITES_CLIENT_SKIP_UPDATE_CHECK=1",
		"HOME=" + homeDir,
		"SATELLITES_DAEMON_HOME=" + daemonHome,
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if elapsed >= time.Second {
		t.Errorf("AC3 elapsed = %s, want < 1s (no auto-spawn implies fast-fail)", elapsed)
	}

	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if exit == 0 {
		t.Errorf("exit = 0, want non-zero on daemon-not-running")
	}
	// cliexit.Server == 5.
	if exit != 5 {
		t.Errorf("exit = %d, want 5 (cliexit.Server)", exit)
	}

	wantSubstr := "daemon not running — start it with: satellites-client serve start"
	if !strings.Contains(stderrBuf.String(), wantSubstr) {
		t.Errorf("stderr missing literal %q:\nstderr=%s", wantSubstr, stderrBuf.String())
	}

	// No auto-spawn: no pidfile under the fresh daemon home.
	pidfile := filepath.Join(daemonHome, "satellites-client.pid")
	if _, err := os.Stat(pidfile); err == nil {
		t.Errorf("auto-spawn detected: pidfile present at %s", pidfile)
	} else if !os.IsNotExist(err) {
		t.Errorf("pidfile stat: %v", err)
	}

	// And the socket file the CLI was pointed at remains absent.
	if _, err := os.Stat(absentSocket); err == nil {
		t.Errorf("auto-spawn detected: socket present at %s", absentSocket)
	}
}

// TestTaskRunAsyncIdempotent — AC5.
//
// Boots a daemon with a Dispatch closure that holds the running task
// in flight long enough for both CLI invocations to land while the
// task's state is steady (running). The daemon's idempotency branch
// reads the running map and returns {queue_position: 0} for both
// calls — the JSON bodies must be reflect.DeepEqual.
//
// Sequencing: a direct socket-POST pre-warms the daemon so the task
// is already in `running` before the operator-visible CLI calls run.
// This makes both CLI calls see the running case (queue_position == 0)
// and isolates AC5 from the scheduler's promotion latency. The CLI is
// still invoked twice — the assertion is about the operator-observable
// stdout JSON being identical across two `task run --async` calls.
func TestTaskRunAsyncIdempotent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go build unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix socket required")
	}

	env := newAsyncTestEnv(t, blockingDispatch(10*time.Second), 1)
	env.stubCalls.addTask("task_idem", map[string]any{
		"workspace_id": "wksp_ac5",
		"project_id":   "proj_ac5",
		"story_id":     "sty_57210d2a",
		"origin":       "story_stage",
	})

	// Pre-warm: post the task to the daemon so it promotes into
	// `running` before the CLI invocations. Wait up to 3s for
	// /v1/status to report running.
	socketPostEnqueue(t, env.socketPath, "task_idem")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body := socketGET(t, env.socketPath, "/v1/status?task_id=task_idem")
		var ts clientdaemon.TaskStatusResponse
		_ = json.Unmarshal(body, &ts)
		if ts.State == "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	stdout1, stderr1, exit1, _ := env.runAsyncCLI(t, "task_idem")
	if exit1 != 0 {
		t.Fatalf("call1 exit=%d stderr=%s", exit1, stderr1)
	}
	stdout2, stderr2, exit2, _ := env.runAsyncCLI(t, "task_idem")
	if exit2 != 0 {
		t.Fatalf("call2 exit=%d stderr=%s", exit2, stderr2)
	}

	var call1, call2 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout1)), &call1); err != nil {
		t.Fatalf("decode call1: %v", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout2)), &call2); err != nil {
		t.Fatalf("decode call2: %v", err)
	}
	if !reflect.DeepEqual(call1, call2) {
		t.Errorf("idempotent mismatch:\ncall1=%v\ncall2=%v", call1, call2)
	}
	t.Logf("AC5 idempotent calls: %v", call1)
}

// socketPostEnqueue POSTs /v1/enqueue over the daemon's unix socket
// — used by AC5 to pre-warm the daemon into `running` before the
// operator-visible CLI invocations.
func socketPostEnqueue(t *testing.T, sockPath, taskID string) {
	t.Helper()
	cli := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 3 * time.Second,
	}
	body, _ := json.Marshal(clientdaemon.EnqueueRequest{TaskID: taskID})
	resp, err := cli.Post("http://daemon/v1/enqueue", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("pre-warm enqueue: %v", err)
	}
	resp.Body.Close()
}

// socketGET issues a GET request to the daemon's unix socket and
// returns the response body.
func socketGET(t *testing.T, sockPath, path string) []byte {
	t.Helper()
	cli := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 3 * time.Second,
	}
	resp, err := cli.Get("http://daemon" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

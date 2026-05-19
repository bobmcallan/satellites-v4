// serve_daemon_home_test.go — sty_517a7db3 integration coverage.
//
// Boots the real satellites-client binary as a subprocess and asserts
// the daemon home resolves to `<repoRoot>/.satellites/daemon/` when
// the binary is invoked inside a project. A second sub-test exercises
// the no-project fallback path (binary outside `.satellites/`, no
// `.git` ancestor) and asserts the legacy `~/.satellites/daemon/`
// location is reached only via the fallback branch.
//
// AC mapping (review-criteria.md):
//   - TestServeDaemonHome/project       — AC2 / AC3 / AC8 boot path
//   - TestServeDaemonHome/no_project    — AC6 fallback boundary
//
// Hermetic: substrate calls go to an httptest stub (matching the
// `internal/clientdaemon/testharness_test.go::stubServer` shape); no
// real `claude` subprocess is invoked — the test only needs the
// daemon's runOne goroutine to enter dispatch so the per-task log
// directory is created (`worker.go` MkdirAll happens *before* the
// claude exec call which itself fails because PATH excludes it).

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/clientdaemon"
)

// daemonHomeStub is the minimal substrate stub the integration test
// stands up. The daemon issues task_get / task_log_append / ledger_append
// against this server during boot + scheduler ticks; the responses are
// canned so the test stays hermetic.
type daemonHomeStub struct {
	mu      sync.Mutex
	taskIDs map[string]map[string]any
	srv     *httptest.Server
}

func newDaemonHomeStub(t *testing.T) *daemonHomeStub {
	t.Helper()
	s := &daemonHomeStub{taskIDs: map[string]map[string]any{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *daemonHomeStub) URL() string { return s.srv.URL }

func (s *daemonHomeStub) addTask(id string, env map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if env == nil {
		env = map[string]any{}
	}
	env["id"] = id
	s.taskIDs[id] = env
}

func (s *daemonHomeStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/task/get":
		id, _ := parsed["id"].(string)
		s.mu.Lock()
		env, ok := s.taskIDs[id]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(env)
	case "/api/v1/task/log/append":
		_, _ = w.Write([]byte(`{"id":"tlog_x","task_id":"x","seq":0,"kind":"x","created_at":"2026-05-19T00:00:00Z"}`))
	case "/api/v1/ledger/append":
		_, _ = w.Write([]byte(`{"id":"ldg_x"}`))
	case "/api/v1/system/version":
		_, _ = w.Write([]byte(`{"version":"dev","build":"unknown","commit":"unknown"}`))
	default:
		// Default OK so the dispatch path doesn't fail on unknown
		// telemetry calls — the test's invariant is filesystem
		// placement, not substrate-call shape parity.
		_, _ = w.Write([]byte(`{}`))
	}
}

// copyFile copies src → dst with executable mode. Used to install the
// just-built binary at <repoRoot>/.satellites/ where the install-
// convention branch of the resolver fires.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy %s → %s: %v", src, dst, err)
	}
}

// writeTOML writes a satellites-client.toml at path with the stub's
// URL as `server` so the daemon doesn't dial the real pprod. token is
// a fixed test-bearer string the stub doesn't validate.
func writeTOML(t *testing.T, path, serverURL string) {
	t.Helper()
	toml := strings.Join([]string{
		fmt.Sprintf("server = %q", serverURL),
		`token = "test-bearer"`,
		`log_level = "warn"`,
		`update_check_disabled = true`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
}

// waitFor polls predicate every 40ms until it returns true or timeout
// elapses. Fails the test with combined output if the deadline trips.
func waitFor(t *testing.T, what string, timeout time.Duration, outBuf *bytes.Buffer, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s after %s\ncombined output:\n%s", what, timeout, outBuf.String())
}

// TestServeDaemonHome — sty_517a7db3 AC2 / AC3 / AC6 / AC8.
//
// The `project` sub-test stages a temp repoRoot with a `.git/`
// directory, installs the binary at `<repoRoot>/.satellites/
// satellites-client` (where the resolver's install-convention branch
// fires), runs `serve start`, enqueues a task to force per-task log
// directory creation, and asserts every artefact lands at
// `<repoRoot>/.satellites/daemon/` with the isolated HOME left
// untouched.
//
// The `no_project` sub-test installs the binary at a path NOT under
// any `.satellites/` directory and runs it from a CWD with no `.git`
// ancestor — the resolver falls through to the user-home branch and
// the daemon home lands at `<isolated_HOME>/.satellites/daemon/`.
func TestServeDaemonHome(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go build unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix socket required")
	}

	binSrc := buildBinary(t, "satellites-client")

	t.Run("project", func(t *testing.T) {
		runProjectScopedDaemon(t, binSrc)
	})
	t.Run("no_project", func(t *testing.T) {
		runNoProjectFallbackDaemon(t, binSrc)
	})
}

func runProjectScopedDaemon(t *testing.T, binSrc string) {
	stub := newDaemonHomeStub(t)
	stub.addTask("task_home", map[string]any{
		"workspace_id": "wksp_home",
		"project_id":   "proj_home",
		"story_id":     "sty_517a7db3",
		"origin":       "story_stage",
	})

	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	installedBin := filepath.Join(repoRoot, ".satellites", "satellites-client")
	copyFile(t, binSrc, installedBin)

	tomlPath := filepath.Join(repoRoot, ".satellites", "satellites-client.toml")
	writeTOML(t, tomlPath, stub.URL())

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}

	expectedDaemonHome := filepath.Join(repoRoot, ".satellites", "daemon")
	expectedSocket := filepath.Join(expectedDaemonHome, "satellites-client.sock")
	expectedPidfile := filepath.Join(expectedDaemonHome, "satellites-client.pid")

	envArgs := []string{
		"PATH=/usr/bin:/bin",
		"SATELLITES_CLIENT_CONFIG=" + tomlPath,
		"SATELLITES_CLIENT_SKIP_UPDATE_CHECK=1",
		"HOME=" + homeDir,
	}

	// `serve start` double-forks; the daemonised child becomes a
	// grandchild detached from this test process. The parent exits
	// after writing the pidfile, so we read the pidfile to find the
	// child and SIGTERM it in cleanup.
	startCtx, startCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer startCancel()
	startCmd := exec.CommandContext(startCtx, installedBin, "serve", "start")
	startCmd.Dir = repoRoot
	startCmd.Env = envArgs
	var startOut bytes.Buffer
	startCmd.Stdout = &startOut
	startCmd.Stderr = &startOut
	if err := startCmd.Run(); err != nil {
		t.Fatalf("serve start: %v\noutput:\n%s", err, startOut.String())
	}

	t.Cleanup(func() {
		if pid, err := readPidFromFile(expectedPidfile); err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	waitFor(t, "socket bind at "+expectedSocket, 5*time.Second, &startOut, func() bool {
		_, err := os.Stat(expectedSocket)
		return err == nil
	})

	// Enqueue a task to force runOne → MkdirAll(LogsDir). The
	// dispatch attempt will fail downstream because `claude` is
	// absent from PATH, but the per-task log file (and so the logs/
	// directory) is created before the failure.
	enqResp := enqueueOverUnixSocket(t, expectedSocket, "task_home")
	if enqResp.TaskID != "task_home" {
		t.Errorf("enqueue response task_id = %q, want %q", enqResp.TaskID, "task_home")
	}
	// AC3: the daemon_pid in the enqueue response identifies the
	// daemonised serve start child — proving the same binary on
	// both surfaces (serve start + the in-process /v1/enqueue
	// dispatch path) resolved to the same socket without flags.
	if enqResp.DaemonPID <= 0 {
		t.Errorf("enqueue daemon_pid = %d, want > 0", enqResp.DaemonPID)
	}
	pidfilePID, err := readPidFromFile(expectedPidfile)
	if err != nil {
		t.Fatalf("read pidfile %s: %v", expectedPidfile, err)
	}
	if enqResp.DaemonPID != pidfilePID {
		t.Errorf("enqueue daemon_pid %d != pidfile pid %d", enqResp.DaemonPID, pidfilePID)
	}

	// AC2: every artefact at <repoRoot>/.satellites/daemon/.
	waitFor(t, "logs/ directory at "+filepath.Join(expectedDaemonHome, "logs"), 5*time.Second, &startOut, func() bool {
		fi, err := os.Stat(filepath.Join(expectedDaemonHome, "logs"))
		return err == nil && fi.IsDir()
	})

	for _, artefact := range []string{
		"satellites-client.sock",
		"satellites-client.pid",
		"state.json",
		"daemon.log",
		"logs",
	} {
		path := filepath.Join(expectedDaemonHome, artefact)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing expected artefact %s: %v\ncombined output:\n%s", path, err, startOut.String())
		}
	}

	// AC2: NOTHING under the isolated HOME's .satellites/daemon/.
	legacyHome := filepath.Join(homeDir, ".satellites", "daemon")
	if entries, err := os.ReadDir(legacyHome); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("legacy home %s unexpectedly populated: %v", legacyHome, names)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat legacy home %s: %v", legacyHome, err)
	}
}

func runNoProjectFallbackDaemon(t *testing.T, binSrc string) {
	stub := newDaemonHomeStub(t)

	// Install the binary outside any `.satellites/` directory so the
	// install-convention branch misses.
	binDir := t.TempDir()
	installedBin := filepath.Join(binDir, "satellites-client")
	copyFile(t, binSrc, installedBin)

	// CWD has no `.git` ancestor — confirm hermetically before
	// proceeding; skip otherwise (CI workers occasionally mount the
	// test tempdir under an ancestor that already holds a `.git/`).
	noProjectDir := t.TempDir()
	for dir := noProjectDir; ; {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			t.Skipf("filesystem ancestor of t.TempDir() has a .git directory at %s — cannot exercise no-project fallback hermetically", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	tomlPath := filepath.Join(t.TempDir(), "satellites-client.toml")
	writeTOML(t, tomlPath, stub.URL())

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}

	expectedDaemonHome := filepath.Join(homeDir, ".satellites", "daemon")
	expectedSocket := filepath.Join(expectedDaemonHome, "satellites-client.sock")
	expectedPidfile := filepath.Join(expectedDaemonHome, "satellites-client.pid")

	envArgs := []string{
		"PATH=/usr/bin:/bin",
		"SATELLITES_CLIENT_CONFIG=" + tomlPath,
		"SATELLITES_CLIENT_SKIP_UPDATE_CHECK=1",
		"HOME=" + homeDir,
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer startCancel()
	startCmd := exec.CommandContext(startCtx, installedBin, "serve", "start")
	startCmd.Dir = noProjectDir
	startCmd.Env = envArgs
	var startOut bytes.Buffer
	startCmd.Stdout = &startOut
	startCmd.Stderr = &startOut
	if err := startCmd.Run(); err != nil {
		t.Fatalf("serve start: %v\noutput:\n%s", err, startOut.String())
	}

	t.Cleanup(func() {
		if pid, err := readPidFromFile(expectedPidfile); err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	waitFor(t, "fallback socket bind at "+expectedSocket, 5*time.Second, &startOut, func() bool {
		_, err := os.Stat(expectedSocket)
		return err == nil
	})

	// AC6: the resolver landed at the user-home path because
	// resolveRepoRoot() returned false (no install-convention match,
	// no .git walk-up hit). The fallback branch wrote the daemon
	// home — sock + pid + state.json — under HOME and NOT under
	// noProjectDir or binDir.
	for _, artefact := range []string{
		"satellites-client.sock",
		"satellites-client.pid",
		"state.json",
	} {
		path := filepath.Join(expectedDaemonHome, artefact)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing expected fallback artefact %s: %v\ncombined output:\n%s", path, err, startOut.String())
		}
	}

	for _, badRoot := range []string{noProjectDir, binDir} {
		bogusHome := filepath.Join(badRoot, ".satellites", "daemon")
		if _, err := os.Stat(bogusHome); err == nil {
			t.Errorf("unexpected daemon home at %s — fallback branch should not have written here", bogusHome)
		}
	}
}

// enqueueOverUnixSocket dials the daemon socket directly and POSTs
// /v1/enqueue. Equivalent to `satellites-client task run --async <id>`
// against the same socket, without re-execing the binary.
func enqueueOverUnixSocket(t *testing.T, sockPath, taskID string) clientdaemon.EnqueueResponse {
	t.Helper()
	cli := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	body, _ := json.Marshal(clientdaemon.EnqueueRequest{TaskID: taskID})
	resp, err := cli.Post("http://daemon/v1/enqueue", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/enqueue: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("enqueue HTTP %d: %s", resp.StatusCode, raw)
	}
	var out clientdaemon.EnqueueResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode enqueue response: %v", err)
	}
	return out
}

// readPidFromFile reads a daemon pidfile and parses the integer pid.
func readPidFromFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(raw))
	var pid int
	if _, err := fmt.Sscanf(s, "%d", &pid); err != nil {
		return 0, fmt.Errorf("parse pid %q: %w", s, err)
	}
	return pid, nil
}

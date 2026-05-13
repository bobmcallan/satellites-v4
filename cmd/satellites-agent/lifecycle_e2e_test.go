package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLifecycle_StartStatusStop_E2E builds the binary, starts it with
// a stub satellites-client on PATH (the agent daemon's cli transport),
// observes status=running, then stops it and observes status=stopped.
// This is the AC's "start, status, stop lifecycle" integration test.
func TestLifecycle_StartStatusStop_E2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid + signals are POSIX-shaped")
	}
	if testing.Short() {
		t.Skip("builds + spawns a binary; skipped under -short")
	}

	dir := t.TempDir()

	// Build the binary into the test tmpdir so we don't depend on a
	// pre-existing ./bin layout.
	binPath := filepath.Join(dir, "satellites-agent")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = repoCmdDir(t)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build failed: %s", out)

	// Stub satellites-client that records each invocation and emits
	// an empty envelope on stdout. Shared with main_test.go.
	stubPath := buildStubClient(t, dir, filepath.Join(dir, "cli-calls.jsonl"))

	cfgPath := filepath.Join(dir, "agent.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`worker_id = "lifecycle-e2e"
spawn_mcp_url = "http://stub-unused"
cli_binary_path = "`+stubPath+`"
hub_url = ""
idle_backoff = "1s"
heartbeat_interval = "1h"
execute_timeout = "5s"
`), 0o600))

	pidPath := filepath.Join(dir, "agent.pid")
	logPath := filepath.Join(dir, "spawn.log")

	// start
	startOut := runBin(t, binPath, "start", "--pidfile", pidPath, "--config", cfgPath, "--logfile", logPath)
	t.Cleanup(func() {
		_ = runBin(t, binPath, "stop", "--pidfile", pidPath, "--grace", "5s")
	})
	assert.Contains(t, startOut, "started")
	pid, err := readPidFile(pidPath)
	require.NoError(t, err, "pidfile missing after start")
	assert.True(t, processAlive(pid), "spawned pid not alive")

	// status (running)
	require.Eventually(t, func() bool {
		statusOut := runBin(t, binPath, "status", "--pidfile", pidPath)
		return bytes.Contains([]byte(statusOut), []byte("running"))
	}, 3*time.Second, 100*time.Millisecond)

	// stop
	stopOut := runBin(t, binPath, "stop", "--pidfile", pidPath, "--grace", "5s")
	assert.Contains(t, stopOut, "stopped")
	assert.False(t, processAlive(pid), "process still alive after stop")
	_, err = os.Stat(pidPath)
	assert.True(t, os.IsNotExist(err), "pidfile not cleaned after stop")

	// status (stopped) — exit code 2.
	cmd := exec.Command(binPath, "status", "--pidfile", pidPath)
	stdout, _ := cmd.CombinedOutput()
	assert.Contains(t, string(stdout), "stopped")
	assert.NotEqual(t, 0, cmd.ProcessState.ExitCode(), "status of stopped agent must be non-zero")
}

// repoCmdDir resolves cmd/satellites-agent at runtime via the test
// file's path. Required because t.TempDir() shifts cwd-relative builds.
func repoCmdDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	// This file lives under cmd/satellites-agent so wd is already
	// that dir under `go test ./cmd/satellites-agent`.
	return wd
}

// runBin executes the built binary with args and returns stdout.
// Test fails on non-zero exit unless the caller expects it (status
// of stopped agent intentionally reports exit 2).
func runBin(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Some commands (status of stopped agent) intentionally exit
		// non-zero. Surface output so the caller can assert on it.
		t.Logf("%s %v exit=%v output=%s", bin, args, err, out)
	}
	return string(out)
}

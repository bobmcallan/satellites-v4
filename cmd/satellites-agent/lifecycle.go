// Lifecycle subcommands for satellites-agent. Operator-facing CLI:
//
//	satellites-agent                 — foreground run loop (default; bw-compat).
//	satellites-agent run [--config]  — explicit foreground (synonym for default).
//	satellites-agent start [--config][--pidfile][--logfile]  — daemonise; writes pidfile.
//	satellites-agent stop  [--pidfile]                       — SIGTERM the pidfile pid.
//	satellites-agent status [--pidfile]                       — report running/stale/stopped.
//
// The pidfile defaults to $HOME/.satellites/agent.pid (or a value via
// --pidfile / SATELLITES_AGENT_PIDFILE). When --background is true
// (default for `start`), the parent re-execs itself as `run` with
// stdout/stderr redirected to the configured logfile, writes the
// child pid into the pidfile, and exits 0. The `run` mode is the
// existing run() entry point — same code path the foreground run
// uses today.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// defaultPidFilePath returns ~/.satellites/agent.pid, falling back to
// /tmp when HOME is unset (CI / test environments).
func defaultPidFilePath() string {
	if v := strings.TrimSpace(os.Getenv("SATELLITES_AGENT_PIDFILE")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "satellites-agent.pid")
	}
	return filepath.Join(home, ".satellites", "agent.pid")
}

// writePidFile writes pid to path, creating the parent directory if
// needed. Existing file is replaced (caller has already checked it
// for liveness via cleanStalePid).
func writePidFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("pidfile: mkdir %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("pidfile: write %q: %w", path, err)
	}
	return nil
}

// readPidFile parses the integer pid stored at path. Missing pidfile
// is reported as ErrPidFileNotFound so the caller can branch on it.
func readPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrPidFileNotFound
		}
		return 0, fmt.Errorf("pidfile: read %q: %w", path, err)
	}
	s := strings.TrimSpace(string(raw))
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pidfile: %q contains invalid pid %q", path, s)
	}
	return pid, nil
}

// ErrPidFileNotFound is returned by readPidFile when the file is absent.
var ErrPidFileNotFound = errors.New("pidfile: not found")

// processAlive reports whether the process with pid is currently
// alive. Uses kill(pid, 0) which signals nothing but returns errno =
// ESRCH for dead pids on POSIX systems.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// cleanStalePid removes a pidfile whose pid is dead. Returns true
// when a stale pidfile was removed; false when the pidfile is missing
// OR points at a live process.
func cleanStalePid(path string) (bool, error) {
	pid, err := readPidFile(path)
	if err != nil {
		if errors.Is(err, ErrPidFileNotFound) {
			return false, nil
		}
		return false, err
	}
	if processAlive(pid) {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("pidfile: remove stale %q: %w", path, err)
	}
	return true, nil
}

// runStart implements `satellites-agent start`. It re-execs self in
// background mode, writes the child pid to the pidfile, and returns
// 0 on success. Returns 1 when the agent is already running (live
// pid in pidfile).
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to TOML config (forwarded to the spawned run)")
	pidPath := fs.String("pidfile", defaultPidFilePath(), "path to pidfile")
	logPath := fs.String("logfile", "", "redirect spawned stdout+stderr to this file (optional)")
	background := fs.Bool("background", true, "spawn detached (default true)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if removed, err := cleanStalePid(*pidPath); err != nil {
		fmt.Fprintf(stderr, "satellites-agent start: %v\n", err)
		return 1
	} else if removed {
		fmt.Fprintf(stdout, "satellites-agent start: removed stale pidfile %s\n", *pidPath)
	}
	if pid, err := readPidFile(*pidPath); err == nil {
		fmt.Fprintf(stderr, "satellites-agent start: already running (pid=%d, pidfile=%s)\n", pid, *pidPath)
		return 1
	}

	if !*background {
		// Operator asked for foreground from start — just run() inline.
		runArgs := []string{"satellites-agent"}
		if *configPath != "" {
			runArgs = append(runArgs, "--config", *configPath)
		}
		// Cannot import context/signal cleanly here; defer to existing
		// foreground path by invoking via run() in main.go's dispatch.
		fmt.Fprintln(stderr, "satellites-agent start: --background=false; run as `satellites-agent run` instead")
		return 1
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "satellites-agent start: resolve executable: %v\n", err)
		return 1
	}
	childArgs := []string{"run"}
	if *configPath != "" {
		childArgs = append(childArgs, "--config", *configPath)
	}
	cmd := exec.Command(self, childArgs...)
	cmd.Stdin = nil
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "satellites-agent start: open logfile %q: %v\n", *logPath, err)
			return 1
		}
		cmd.Stdout = f
		cmd.Stderr = f
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "satellites-agent start: spawn: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	// Release so the parent doesn't wait on the child.
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(stderr, "satellites-agent start: release child: %v\n", err)
	}
	if err := writePidFile(*pidPath, pid); err != nil {
		fmt.Fprintf(stderr, "satellites-agent start: %v\n", err)
		// Best-effort kill of the orphaned child since pidfile failed.
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return 1
	}
	fmt.Fprintf(stdout, "satellites-agent started (pid=%d, pidfile=%s)\n", pid, *pidPath)
	return 0
}

// runStop implements `satellites-agent stop`. Sends SIGTERM to the
// pidfile's pid; waits up to --grace seconds for exit; removes the
// pidfile on observed exit. Returns 0 on success, 1 when pidfile
// missing or stop verification fails.
func runStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pidPath := fs.String("pidfile", defaultPidFilePath(), "path to pidfile")
	grace := fs.Duration("grace", 10*time.Second, "wait this long for graceful exit before reporting failure")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	pid, err := readPidFile(*pidPath)
	if err != nil {
		if errors.Is(err, ErrPidFileNotFound) {
			fmt.Fprintf(stdout, "satellites-agent stop: not running (no pidfile at %s)\n", *pidPath)
			return 0
		}
		fmt.Fprintf(stderr, "satellites-agent stop: %v\n", err)
		return 1
	}

	if !processAlive(pid) {
		_ = os.Remove(*pidPath)
		fmt.Fprintf(stdout, "satellites-agent stop: pid=%d not alive; removed stale pidfile\n", pid)
		return 0
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(stderr, "satellites-agent stop: kill pid=%d: %v\n", pid, err)
		return 1
	}

	deadline := time.Now().Add(*grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			_ = os.Remove(*pidPath)
			fmt.Fprintf(stdout, "satellites-agent stopped (pid=%d)\n", pid)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "satellites-agent stop: pid=%d still alive after %s; pidfile retained\n", pid, *grace)
	return 1
}

// runStatus implements `satellites-agent status`. Reports
// running / stale-pidfile / stopped. Exit codes:
//
//	0 — running.
//	1 — stale pidfile (pid in file, but process is dead).
//	2 — stopped (no pidfile).
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pidPath := fs.String("pidfile", defaultPidFilePath(), "path to pidfile")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pid, err := readPidFile(*pidPath)
	if err != nil {
		if errors.Is(err, ErrPidFileNotFound) {
			fmt.Fprintf(stdout, "stopped (no pidfile at %s)\n", *pidPath)
			return 2
		}
		fmt.Fprintf(stderr, "satellites-agent status: %v\n", err)
		return 2
	}

	if processAlive(pid) {
		fmt.Fprintf(stdout, "running (pid=%d, pidfile=%s)\n", pid, *pidPath)
		return 0
	}
	fmt.Fprintf(stdout, "stale (pid=%d not alive, pidfile=%s) — run `satellites-agent stop` to clean up\n", pid, *pidPath)
	return 1
}

// serve_lifecycle.go — pidfile + double-fork helpers shared by the
// `satellites-client serve` verbs. Mirrors cmd/satellites-agent/
// lifecycle.go (sty_5aa20f1b plan §1.AC1 / §2.AC1).

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// errPidFileNotFound signals readClientPidFile that no pidfile exists.
var errPidFileNotFound = errors.New("client pidfile: not found")

// writeClientPidFile writes pid to path, creating the parent directory
// if needed. Existing file is replaced (caller cleans stale pidfiles
// via cleanStaleClientPid first).
func writeClientPidFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("client pidfile: mkdir %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("client pidfile: write %q: %w", path, err)
	}
	return nil
}

// readClientPidFile parses the integer pid stored at path. Missing
// pidfile is reported as errPidFileNotFound so callers can branch.
func readClientPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, errPidFileNotFound
		}
		return 0, fmt.Errorf("client pidfile: read %q: %w", path, err)
	}
	s := strings.TrimSpace(string(raw))
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("client pidfile: %q contains invalid pid %q", path, s)
	}
	return pid, nil
}

// clientProcessAlive reports whether pid is currently alive.
func clientProcessAlive(pid int) bool {
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

// cleanStaleClientPid removes a pidfile whose pid is dead. Returns
// true when a stale pidfile was removed; false when the pidfile is
// missing OR points at a live process.
func cleanStaleClientPid(path string) (bool, error) {
	pid, err := readClientPidFile(path)
	if err != nil {
		if errors.Is(err, errPidFileNotFound) {
			return false, nil
		}
		return false, err
	}
	if clientProcessAlive(pid) {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("client pidfile: remove stale %q: %w", path, err)
	}
	return true, nil
}

// daemoniseSelf re-execs the running binary with the supplied argv as
// `serve run` in a detached session, redirecting stdout+stderr to
// logfile (when non-empty), and returns the spawned child pid. The
// child's pid is what writes the pidfile.
func daemoniseSelf(argv []string, logfile string, stderr io.Writer) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("client daemonise: resolve executable: %w", err)
	}
	cmd := exec.Command(self, argv...)
	cmd.Stdin = nil
	if logfile != "" {
		if err := os.MkdirAll(filepath.Dir(logfile), 0o755); err != nil {
			return 0, fmt.Errorf("client daemonise: mkdir logfile dir %q: %w", filepath.Dir(logfile), err)
		}
		f, err := os.OpenFile(logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 0, fmt.Errorf("client daemonise: open logfile %q: %w", logfile, err)
		}
		cmd.Stdout = f
		cmd.Stderr = f
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("client daemonise: spawn: %w", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(stderr, "satellites-client serve start: release child: %v\n", err)
	}
	return pid, nil
}

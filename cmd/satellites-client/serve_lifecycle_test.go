package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPidfileWriteReadRoundTrip exercises the pidfile happy path.
func TestPidfileWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.pid")
	if err := writeClientPidFile(path, 12345); err != nil {
		t.Fatalf("write: %v", err)
	}
	pid, err := readClientPidFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if pid != 12345 {
		t.Errorf("pid = %d, want 12345", pid)
	}
}

// TestReadClientPidFile_Missing returns errPidFileNotFound.
func TestReadClientPidFile_Missing(t *testing.T) {
	_, err := readClientPidFile(filepath.Join(t.TempDir(), "absent.pid"))
	if !errors.Is(err, errPidFileNotFound) {
		t.Errorf("err = %v, want errPidFileNotFound", err)
	}
}

// TestCleanStaleClientPid removes a pidfile whose pid is dead.
func TestCleanStaleClientPid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.pid")
	if err := writeClientPidFile(path, 999999); err != nil { // assume not running
		t.Fatalf("write: %v", err)
	}
	removed, err := cleanStaleClientPid(path)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if !removed {
		t.Errorf("expected stale pidfile to be removed")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected pidfile %s to be deleted; err=%v", path, err)
	}
}

// TestProcessAlive_Self is the trivial branch.
func TestProcessAlive_Self(t *testing.T) {
	if !clientProcessAlive(os.Getpid()) {
		t.Errorf("clientProcessAlive(self) = false")
	}
	if clientProcessAlive(0) || clientProcessAlive(-1) {
		t.Errorf("clientProcessAlive(non-positive) = true")
	}
}

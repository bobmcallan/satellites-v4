package arbor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestIDRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if got := RequestIDFrom(ctx); got != "" {
		t.Fatalf("expected empty request id on fresh context, got %q", got)
	}

	ctx = WithRequestID(ctx, "req_abc123")
	if got := RequestIDFrom(ctx); got != "req_abc123" {
		t.Fatalf("expected %q, got %q", "req_abc123", got)
	}
}

func TestPruneOldLogs_KeepsLatest20(t *testing.T) {
	dir := t.TempDir()
	stem := "satellites-client"

	// Create 25 stub log files with strictly-ordered mtimes.
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("%s.2026-05-12T%02d.log", stem, i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}

	// Unrelated files that must survive the prune.
	if err := os.WriteFile(filepath.Join(dir, "satellites-agent.2026.log"), []byte("x"), 0o600); err != nil {
		t.Fatalf("agent file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stem+".log"), []byte("x"), 0o600); err != nil {
		t.Fatalf("stem.log pointer: %v", err)
	}

	pruneOldLogs(dir, stem, 20)

	got, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var kept []string
	for _, e := range got {
		kept = append(kept, e.Name())
	}
	// Expected: 20 newest stub files (indices 5..24) + "satellites-agent.2026.log" + bare "<stem>.log".
	if len(kept) != 22 {
		t.Fatalf("kept %d files, want 22 (20 newest + agent + stem pointer); got %v", len(kept), kept)
	}
	for i := 0; i < 5; i++ {
		removed := fmt.Sprintf("%s.2026-05-12T%02d.log", stem, i)
		if _, err := os.Stat(filepath.Join(dir, removed)); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err = %v", removed, err)
		}
	}
	for i := 5; i < 25; i++ {
		retained := fmt.Sprintf("%s.2026-05-12T%02d.log", stem, i)
		if _, err := os.Stat(filepath.Join(dir, retained)); err != nil {
			t.Fatalf("expected %s retained, stat err = %v", retained, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "satellites-agent.2026.log")); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stem+".log")); err != nil {
		t.Fatalf("stem pointer removed: %v", err)
	}
}

func TestDefaultLoggerNotNil(t *testing.T) {
	t.Parallel()
	if Default() == nil {
		t.Fatal("Default() must return a non-nil logger")
	}
}

func TestNewRespectsLevel(t *testing.T) {
	t.Parallel()
	if New("debug") == nil {
		t.Fatal("New(debug) must return a non-nil logger")
	}
	if New("garbage") == nil {
		t.Fatal("New(unknown level) must still return a logger")
	}
}

// TestNewWithFile_ConsoleOnlyWhenEmpty asserts that an empty logDir
// degrades to console-only behaviour (matching New). sty_92bfd9e6.
func TestNewWithFile_ConsoleOnlyWhenEmpty(t *testing.T) {
	t.Parallel()
	if NewWithFile("info", "", "satellites-agent.log") == nil {
		t.Fatal("NewWithFile(empty) must return a non-nil logger")
	}
}

// TestNewWithFile_WritesFile asserts that a non-empty logDir produces
// a logger whose writes land on disk under the supplied directory.
// arbor's phuslu/log backend creates the directory with EnsureFolder
// even when the parent doesn't exist; the file appears on first
// successful Info() call. sty_92bfd9e6.
func TestNewWithFile_WritesFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	logDir := filepath.Join(tmp, "nested", "logs")

	logger := NewWithFile("info", logDir, "satellites-agent.log")
	if logger == nil {
		t.Fatal("NewWithFile must return a non-nil logger")
	}
	logger.Info().Str("smoke", "sty_92bfd9e6").Msg("file-writer smoke")

	// Phuslu rotates filenames as <name>.<timestamp>.<ext> and writes a
	// stable symlink/hardlink at <name>. We don't pin the rotation
	// shape — just that SOMETHING landed under logDir. Tolerate up to
	// 2s for the async writer to flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(logDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "satellites-agent") {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no satellites-agent.* file appeared under %s within 2s", logDir)
}

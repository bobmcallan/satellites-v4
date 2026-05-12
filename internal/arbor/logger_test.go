package arbor

import (
	"context"
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

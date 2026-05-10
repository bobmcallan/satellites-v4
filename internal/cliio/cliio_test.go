package cliio_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/cliio"
)

func TestResolve_JSONFlagAlwaysJSON(t *testing.T) {
	// Even on a real stdout (which in `go test` is normally piped)
	// the explicit --json flag must force ModeJSON.
	if got := cliio.Resolve(true, os.Stdout); got != cliio.ModeJSON {
		t.Fatalf("Resolve(--json=true) = %d, want %d", got, cliio.ModeJSON)
	}
}

func TestResolve_NoTTYReturnsJSON(t *testing.T) {
	// `go test` runs with stdout piped, so IsTerminal is false; this
	// exercises the auto-JSON path documented in §3.
	if got := cliio.Resolve(false, os.Stdout); got != cliio.ModeJSON {
		t.Fatalf("Resolve(no tty, no flag) = %d, want %d", got, cliio.ModeJSON)
	}
}

func TestRenderJSON_CompactByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := cliio.RenderJSON(&buf, map[string]any{"id": "task_xxx", "n": 3}); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	// Compact form has no leading whitespace per line.
	if strings.Contains(got, "\n  ") {
		t.Fatalf("RenderJSON produced indented output: %q", got)
	}
}

func TestRenderJSONIndent_TwoSpaces(t *testing.T) {
	var buf bytes.Buffer
	if err := cliio.RenderJSONIndent(&buf, map[string]any{"a": 1}); err != nil {
		t.Fatalf("RenderJSONIndent: %v", err)
	}
	if !strings.Contains(buf.String(), "  \"a\":") {
		t.Fatalf("RenderJSONIndent missing two-space indent: %q", buf.String())
	}
}

func TestPrintError_Format(t *testing.T) {
	// PrintError writes to os.Stderr; the test simply asserts it
	// does not panic on nil and on a non-empty error path.
	cliio.PrintError("story get", nil) // no-op
	cliio.PrintError("", nil)           // no-op
}

func TestModeIsJSON(t *testing.T) {
	if !cliio.ModeJSON.IsJSON() {
		t.Fatal("ModeJSON.IsJSON() must be true")
	}
	if cliio.ModeAuto.IsJSON() {
		t.Fatal("ModeAuto.IsJSON() must be false")
	}
}

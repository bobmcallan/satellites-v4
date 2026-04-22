// Package integration is the single home for end-to-end smoke tests against
// satellites-v4 binaries. Future feature stories extend this package rather
// than building a parallel one — see docs/development.md for the extension
// pattern (e.g. adding a testcontainers-go backed DB when a schema lands).
package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// repoRoot resolves the satellites-v4 repo root by walking up from this file's
// path at runtime. Avoids depending on `go list -m` at test time.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve harness.go path")
	}
	// <root>/tests/integration/harness.go → <root>
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// buildBinary builds the named cmd target into t.TempDir() and returns the
// absolute path of the resulting binary. Build failures fail the test with
// the toolchain's stderr attached.
//
// `name` is the folder under cmd/, e.g. "satellites" or "satellites-agent".
func buildBinary(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/"+name)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build ./cmd/%s failed: %v\n%s", name, err, out)
	}
	return dst
}

// runBinary executes path with a bounded timeout, returning combined stdout
// and stderr. The current satellites-v4 binaries print a single boot line
// and exit; when a future feature introduces a long-running mode, switch the
// callers to a start/stop pattern rather than re-interpreting this helper.
func runBinary(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path).CombinedOutput()
	if err != nil {
		t.Fatalf("binary %s exited non-zero: %v\n%s", filepath.Base(path), err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

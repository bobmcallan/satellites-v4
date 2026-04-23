// Package integration is the single home for end-to-end smoke tests against
// satellites-v4 binaries. Future feature stories extend this package rather
// than building a parallel one — see docs/development.md for the extension
// pattern (e.g. adding a testcontainers-go backed DB when a schema lands).
package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
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
// and stderr. Used for the agent smoke — which still print-and-exits pre-agent
// story.
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

// startServerContainer builds the satellites image from docker/Dockerfile and
// starts it as a testcontainer waiting on /healthz. Returns the base URL
// (scheme+host+mapped-port) and a cleanup function the caller must defer.
func startServerContainer(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	root := repoRoot(t)
	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    root,
			Dockerfile: "docker/Dockerfile",
			KeepImage:  true,
		},
		ExposedPorts: []string{"8080/tcp"},
		Env: map[string]string{
			"PORT":      "8080",
			"ENV":       "dev",
			"LOG_LEVEL": "info",
			"DEV_MODE":  "true",
		},
		WaitingFor: wait.ForHTTP("/healthz").
			WithPort("8080/tcp").
			WithStartupTimeout(90 * time.Second).
			WithResponseMatcher(func(body io.Reader) bool {
				// The URL matcher already handles the 200 status; the body
				// matcher just drains and accepts.
				_, _ = io.Copy(io.Discard, body)
				return true
			}),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start container: %v", err)
	}
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mapped, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, mapped.Port())
	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := container.Terminate(stopCtx); err != nil {
			t.Logf("container terminate: %v", err)
		}
	}
	// Extra safety: do a plain GET /healthz before handing control back.
	hcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req2, _ := http.NewRequestWithContext(hcCtx, http.MethodGet, baseURL+"/healthz", nil)
	if resp, err := http.DefaultClient.Do(req2); err == nil {
		resp.Body.Close()
	}
	return baseURL, stop
}

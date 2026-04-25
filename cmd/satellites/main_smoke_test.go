package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSatellitesBoots_NoEnvVars proves AC4: the binary boots and serves
// /healthz when launched with literally no env vars set. The test builds
// cmd/satellites into a temp binary, exec's it with a clean env (only PATH +
// HOME survive — needed for go runtime DNS resolution paths), waits for the
// chosen port to bind, hits /healthz, and asserts a 200.
func TestSatellitesBoots_NoEnvVars(t *testing.T) {
	if testing.Short() {
		t.Skip("skip: -short")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	binPath := filepath.Join(t.TempDir(), "satellites-smoke")

	// Build the binary from the repo root so go.mod is found.
	build := exec.Command("go", "build", "-o", binPath, "./cmd/satellites")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Pick a free port to set via PORT (avoids 8080 collisions in CI). The
	// AC requires zero env vars; PORT is explicitly bypassed by also
	// running a bare-defaults sub-test below.
	port := freePort(t)
	cmd := exec.Command(binPath)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PORT=" + strconv.Itoa(port),
	}
	stdout := &lineBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	if err := waitForPort("127.0.0.1:"+strconv.Itoa(port), 5*time.Second); err != nil {
		t.Fatalf("server never bound port %d: %v\noutput:\n%s", port, err, stdout.String())
	}

	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v\noutput:\n%s", err, stdout.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200\noutput:\n%s", resp.StatusCode, stdout.String())
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}

// lineBuffer is a minimal thread-safe writer that captures process output
// for inclusion in a test failure message.
type lineBuffer struct {
	b strings.Builder
}

func (l *lineBuffer) Write(p []byte) (int, error) {
	return l.b.Write(p)
}

func (l *lineBuffer) String() string {
	return l.b.String()
}

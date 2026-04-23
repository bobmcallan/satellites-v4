package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Smoke tests prove the two binaries compile and boot. They exist before any
// feature code so the local dev loop is green from the first feature story
// forward.

const bootTimeout = 10 * time.Second

// TestServerBootsWithVersionLine starts the satellites server locally on a
// free-by-convention port, hits /healthz, then sends SIGTERM and asserts the
// process exits cleanly within the 10-second graceful-shutdown bound (AC 2).
// For docker-image verification see TestHealthzReturnsVersion in http_test.go.
func TestServerBootsWithVersionLine(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t, "satellites")

	// Use a non-default port so the test doesn't collide with a running
	// local instance. Port 0 would be nice but the binary doesn't parse
	// "listen on ephemeral" yet; pick a high-range fixed port and rely on
	// the test running alone.
	const port = "19080"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(cmd.Env,
		"PORT="+port,
		"ENV=dev",
		"LOG_LEVEL=info",
		"DEV_MODE=true",
		"PATH=/usr/bin:/bin",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
	}()

	baseURL := "http://127.0.0.1:" + port
	if err := waitForHealthz(baseURL, 10*time.Second); err != nil {
		t.Fatalf("healthz never came up: %v", err)
	}

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"version", "build", "commit", "started_at", "uptime_seconds"} {
		if _, ok := body[k]; !ok {
			t.Errorf("healthz missing %q: %+v", k, body)
		}
	}

	// Send SIGTERM and assert a clean exit within 10s.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && !ee.Success() {
				t.Fatalf("server exited non-zero: %v", err)
			}
			t.Fatalf("server Wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit within 10 seconds of SIGTERM")
	}
}

// TestAgentBootsWithVersionLine keeps the print-and-exit smoke against
// satellites-agent until a later epic grows the agent into a long-running
// task loop.
func TestAgentBootsWithVersionLine(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t, "satellites-agent")
	got := runBinary(t, bin, bootTimeout)
	t.Logf("boot line: %q", got)

	if !strings.Contains(got, "satellites-agent") {
		t.Fatalf("expected %q fragment, got %q", "satellites-agent", got)
	}
	for _, frag := range []string{"build", "commit", "version"} {
		if !strings.Contains(got, frag) {
			t.Errorf("boot line missing %q fragment: %q", frag, got)
		}
	}
}

// waitForHealthz polls baseURL+"/healthz" until it returns 200 or the
// deadline elapses.
func waitForHealthz(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = &httpStatusErr{status: resp.StatusCode}
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return lastErr
}

type httpStatusErr struct{ status int }

func (e *httpStatusErr) Error() string { return http.StatusText(e.status) }

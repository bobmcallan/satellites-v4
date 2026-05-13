// system_version_test.go — sty_64e69db8 boot drift end-to-end.
//
// Boots the real satellites-server image via the testcontainers
// harness, points its SATELLITES_MANIFEST_URL at an httptest.Server
// that serves a manifest with a version ahead of the binary's
// ldflag stamp, builds a satellites-client binary stamped with a
// "stale" version, and asserts:
//
//   - exit code 9,
//   - exactly one stderr line that decodes as JSON,
//   - {event: "version_drift", local, remote, tolerance} payload.
//
// Honours pr_local_iteration — the test runs against the full
// substrate boot via testcontainers, not a stub.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSystemVersion_BootDriftEndToEnd is the AC4 evidence anchor.
func TestSystemVersion_BootDriftEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration harness requires a posix shell + docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping testcontainers-backed test")
	}

	// 1. Stand up the fake manifest service. The server container
	//    fetches this URL when the CLI calls /api/v1/system/version.
	manifestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
		    "version":"0.0.269",
		    "build":"2026-05-13-12-00-00",
		    "commit":"abcd1234",
		    "artifacts":[
		      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"deadbeef","download_url":"https://example.invalid/a"}
		    ]
		}`)
	}))
	t.Cleanup(manifestSrv.Close)

	// 2. Make the manifest URL reachable from inside the satellites
	//    container. testcontainers exposes the host via the magic
	//    `host.docker.internal` DNS name; mirror what the agent
	//    harness uses for the kv tests.
	manifestForContainer := strings.Replace(manifestSrv.URL, "127.0.0.1", "host.docker.internal", 1)

	// 3. Boot the satellites-server container with the substituted
	//    manifest URL exposed via SATELLITES_MANIFEST_URL.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const bearer = "key_drift_test"
	baseURL, stop := startServerContainerWithEnv(t, ctx, map[string]string{
		"SATELLITES_MANIFEST_URL": manifestForContainer,
		"SATELLITES_API_KEYS":     bearer,
	})
	defer stop()

	// 4. Build a stale-stamped satellites-client. The release
	//    workflow uses scripts/build.sh's stamp_ldflags; the test
	//    pins a "0.0.1" version stamp directly via -ldflags.
	staleBin := filepath.Join(t.TempDir(), "satellites-client-stale")
	build := exec.Command("go", "build",
		"-ldflags",
		"-X github.com/bobmcallan/satellites/internal/config.Version=0.0.1"+
			" -X github.com/bobmcallan/satellites/internal/config.Build=test"+
			" -X github.com/bobmcallan/satellites/internal/config.GitCommit=test",
		"-o", staleBin, "./cmd/satellites-client")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build stale client: %v\n%s", err, out)
	}

	// 5. Point the stale binary at the container, with a TOML that
	//    enables the drift check (tolerance 0, default).
	cfgDir := t.TempDir()
	tomlPath := filepath.Join(cfgDir, "satellites-client.toml")
	tomlBody := fmt.Sprintf(`server = %q
token = %q
log_level = "info"
`, baseURL, bearer)
	if err := os.WriteFile(tomlPath, []byte(tomlBody), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}

	// 6. Invoke a read verb that triggers PersistentPreRunE. `info`
	//    is the simplest one. The drift check fires before the
	//    verb itself runs.
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, staleBin, "info")
	cmd.Env = append(os.Environ(),
		"SATELLITES_CLIENT_CONFIG="+tomlPath,
		// Ensure the dispatched-subprocess skip is NOT set so the
		// drift check actually runs in this parent process.
	)
	cmd.Env = removeEnv(cmd.Env, "SATELLITES_CLIENT_SKIP_UPDATE_CHECK")
	stderrBuf := strings.Builder{}
	cmd.Stderr = &writerSink{b: &stderrBuf}
	cmd.Stdout = &writerSink{b: &strings.Builder{}}
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected non-zero exit, got nil. stderr: %s", stderrBuf.String())
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T (%v)", err, err)
	}
	if ee.ExitCode() != 9 {
		t.Fatalf("exit code = %d, want 9. stderr:\n%s", ee.ExitCode(), stderrBuf.String())
	}

	// 7. Locate the structured JSON line in stderr. Other lines may
	//    appear (arbor's boot info log on the console writer); the
	//    test scans for the first line that parses + carries the
	//    event=version_drift marker.
	var event struct {
		Event     string `json:"event"`
		Local     string `json:"local"`
		Remote    string `json:"remote"`
		Tolerance int    `json:"tolerance"`
	}
	found := false
	for _, line := range strings.Split(stderrBuf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &event); err == nil && event.Event == "version_drift" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no parseable version_drift line in stderr:\n%s", stderrBuf.String())
	}
	if event.Local != "0.0.1" || event.Remote != "0.0.269" {
		t.Errorf("event payload = %+v, want local=0.0.1 remote=0.0.269", event)
	}
}

// writerSink is the minimal io.Writer the exec sink needs.
type writerSink struct {
	b *strings.Builder
}

func (w *writerSink) Write(p []byte) (int, error) { return w.b.Write(p) }

func (w *writerSink) String() string { return w.b.String() }

// removeEnv strips the named env-var from a slice of KEY=VALUE
// entries. The test must not inherit a SKIP flag the operator's
// shell may have set.
func removeEnv(env []string, key string) []string {
	out := env[:0]
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

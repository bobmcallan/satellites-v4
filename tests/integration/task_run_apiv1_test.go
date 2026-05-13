// task_run_apiv1_test.go — sty_74e67353 work#3.
//
// Drives the real satellites-client binary end-to-end and asserts the
// `task run <task_id>` flow issues ALL substrate calls against the
// /api/v1/<noun>/<verb> HTTP surface (zero hits on /mcp paths). Closes
// the story's "Local integration test" AC.
//
// Cites pr_local_iteration: this test runs against an in-process
// httptest.Server stub, not the testcontainers harness — the underlying
// transport assertion ("does the binary POST to /api/v1/* and no
// /mcp/*?") is identical and the unit doesn't need surreal's truth.
// The full surreal-backed integration covers other AC slices via the
// existing testcontainers tests (api_parity_test.go).
//
// Stub design:
//
//   - One httptest.Server records every request path and body.
//   - It stubs every /api/v1/<noun>/<verb> route the worker touches
//     (task/get, document/get, story/get, ledger/append) plus task/update
//     (called by the stub-claude subprocess on close).
//   - It 404s every /mcp* route so a regression to MCP transport would
//     surface as a non-2xx the test assertion picks up.
//
// Stub claude:
//
//   - A posix shell script that calls `satellites-client task update
//     --id $TASK_ID --status closed --outcome success` then exits 0.
//   - That re-invocation rides SATELLITES_CLIENT_CONFIG (which the
//     dispatcher forwards into the subprocess env per sty_ef4eedaa),
//     hitting the same httptest server.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTaskRun_AllRequestsOnAPIv1 — story sty_74e67353 work#3 AC
// "Local integration test".
//
// Boots an httptest stub, builds the real satellites-client, dispatches
// `task run <task_id>` with a stub claude, and asserts:
//
//   - ZERO requests on any /mcp* path.
//   - ≥1 hit on each of /api/v1/task/get, /api/v1/document/get,
//     /api/v1/story/get, /api/v1/ledger/append, /api/v1/task/update.
func TestTaskRun_AllRequestsOnAPIv1(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub claude is a posix shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const (
		taskID    = "task_apiv1_smoke"
		storyID   = "sty_apiv1_smoke"
		projectID = "proj_apiv1_smoke"
		wkspID    = "wksp_apiv1_smoke"
		agentID   = "doc_apiv1_smoke"
		token     = "tok-apiv1-smoke"
	)

	// 1. Recording stub server. Routes every /api/v1/<noun>/<verb>
	//    the dispatcher + the stub-claude task_update touch; 404s
	//    every /mcp* path so a transport regression to the MCP
	//    surface surfaces as a non-2xx.
	calls, srvURL, srvClose := newRecordingStub(t, taskID, storyID, projectID, wkspID, agentID)
	defer srvClose()

	// 2. Real git repo for the worktree commands.
	repoRoot := newTestRepo(t)

	// 3. satellites-client.toml — point the binary at the stub
	//    server, name a per-test worktree root, and stamp the
	//    bearer.
	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	tomlPath := filepath.Join(binDir, "satellites-client.toml")
	toml := strings.Join([]string{
		fmt.Sprintf("server = %q", srvURL),
		fmt.Sprintf("token = %q", token),
		fmt.Sprintf("repo_path = %q", repoRoot),
		fmt.Sprintf("worktree_root = %q", worktreeRoot),
		`branch_template = "agent-{task_id}-from-{base_sha}"`,
		`execute_timeout = "30s"`,
		`log_level = "info"`,
	}, "\n") + "\n"
	if err := os.WriteFile(tomlPath, []byte(toml), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}

	// 4. Build satellites-client + the stub-claude (a shell script
	//    that delegates close to satellites-client task update).
	clientBin := buildBinary(t, "satellites-client")
	claudeStub := writeApiV1StubClaude(t, clientBin, tomlPath, taskID)

	// 5. Point the agent config's ClaudeBinaryPath at the stub.
	//    The toml has no claude_binary_path key; task_run.go hard-
	//    codes "claude" but we override via the cmd's env: satellites-
	//    client doesn't currently surface a --claude-binary flag, so
	//    we patch PATH to put a `claude` shim resolving to the stub
	//    first.
	pathDir := filepath.Join(t.TempDir(), "pathshim")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatalf("mkdir pathshim: %v", err)
	}
	claudeLink := filepath.Join(pathDir, "claude")
	if err := os.Symlink(claudeStub, claudeLink); err != nil {
		t.Fatalf("symlink claude shim: %v", err)
	}

	// 6. Dispatch.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, clientBin, "task", "run", taskID)
	cmd.Env = append(os.Environ(),
		"SATELLITES_CLIENT_CONFIG="+tomlPath,
		"PATH="+pathDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("satellites-client task run failed: %v\noutput:\n%s", err, string(out))
	}
	t.Logf("task run stdout/stderr:\n%s", string(out))

	// 7. Assertions.
	recorded := calls()
	pathHits := map[string]int{}
	for _, c := range recorded {
		pathHits[c.Path]++
		if strings.HasPrefix(c.Path, "/mcp") {
			t.Errorf("unexpected /mcp request from satellites-client: %s", c.Path)
		}
	}

	expectations := []string{
		"/api/v1/task/get",
		"/api/v1/document/get",
		"/api/v1/story/get",
		"/api/v1/ledger/append",
		"/api/v1/task/update",
	}
	for _, want := range expectations {
		if pathHits[want] < 1 {
			t.Errorf("expected ≥1 hit on %s — saw paths=%v", want, pathHits)
		}
	}
}

// recordedRequest captures the path + decoded body of one HTTP request
// the satellites-client process issued during a task run.
type recordedRequest struct {
	Path    string
	Body    map[string]any
	RawBody string
}

// newRecordingStub stands up an httptest server that records every
// inbound request and responds to the /api/v1/<noun>/<verb> routes the
// dispatcher + the stub-claude close touch.
func newRecordingStub(t *testing.T, taskID, storyID, projectID, wkspID, agentID string) (func() []recordedRequest, string, func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		mu.Lock()
		calls = append(calls, recordedRequest{
			Path:    r.URL.Path,
			Body:    parsed,
			RawBody: string(body),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/task/get":
			fmt.Fprintf(w,
				`{"id":%q,"story_id":%q,"project_id":%q,"workspace_id":%q,"agent_id":%q,"action":"contract:develop","description":"smoke","origin":"story_stage"}`,
				taskID, storyID, projectID, wkspID, agentID,
			)
		case "/api/v1/document/get":
			docType, _ := parsed["type"].(string)
			switch docType {
			case "agent":
				fmt.Fprintf(w, `{"id":%q,"name":"developer_agent"}`, agentID)
			case "contract":
				fmt.Fprintf(w, `{"name":"develop"}`)
			default:
				fmt.Fprintf(w, `{}`)
			}
		case "/api/v1/story/get":
			fmt.Fprintf(w, `{"story":{"id":%q,"title":"smoke story"}}`, storyID)
		case "/api/v1/ledger/append":
			fmt.Fprintf(w, `{"id":"ldg_apiv1_smoke"}`)
		case "/api/v1/task/update":
			fmt.Fprintf(w, `{"id":%q,"status":"closed","outcome":"success"}`, taskID)
		default:
			http.Error(w, "unhandled "+r.URL.Path, http.StatusNotFound)
		}
	}))

	getCalls := func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedRequest, len(calls))
		copy(out, calls)
		return out
	}
	return getCalls, srv.URL, srv.Close
}

// newTestRepo initialises a fresh git repo with a single commit so the
// worker's `git worktree add` succeeds.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// writeApiV1StubClaude writes a posix shell script that the dispatcher
// will spawn as `claude`. The stub re-invokes the same satellites-
// client binary to issue task_update — the dispatched-subprocess close
// path the integration test must exercise. SATELLITES_CLIENT_CONFIG is
// forwarded by the dispatcher (sty_ef4eedaa) so the re-invocation
// reaches the same stub server.
func writeApiV1StubClaude(t *testing.T, clientBin, tomlPath, taskID string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "claude-stub.sh")
	script := fmt.Sprintf(`#!/bin/sh
set -e
SATELLITES_CLIENT_CONFIG=%q %q task update --id %s --status closed --outcome success >/dev/null 2>&1 || true
exit 0
`, tomlPath, clientBin, taskID)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub claude: %v", err)
	}
	return scriptPath
}

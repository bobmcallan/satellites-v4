package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestClaudeClient_Execute_EndToEnd_StubBinary drives the seven-step
// Execute flow with a stub `claude` script and a stub MCP server. It
// is the AC's "end-to-end claim → execute → close" coverage minus
// testcontainers — the substrate live dogfood (kind:dogfood-evidence)
// is the canonical pprod-side proof, captured separately on the
// story's ledger.
func TestClaudeClient_Execute_EndToEnd_StubBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub claude is a posix shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// 1. Real git repo for the worktree commands.
	repoRoot := initGitRepo(t)

	// 2. Stub claude binary — captures argv + env into per-arg files,
	//    so the test asserts what the orchestrator passed.
	stubPath, captureDir := writeStubClaude(t)

	// 3. Stub MCP server — responds to task_get / agent_get /
	//    contract_get / story_get / ledger_append.
	mcpCalls, mcpURL, mcpClose := stubMCP(t, "task_test_e2e")
	defer mcpClose()

	// 4. Cleansed source HOME (no projects/) so cleanseHome has
	//    something to copy.
	srcHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", ".credentials.json"), []byte(`{"oauth":"x"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", "settings.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.Setenv("HOME", srcHome))
	t.Cleanup(func() { _ = os.Unsetenv("HOME") })

	worktreeRoot := filepath.Join(t.TempDir(), "wt")

	cfg := config.AgentConfig{
		MCPURL:           mcpURL,
		AuthToken:        "tok-e2e",
		ExecuteTimeout:   30 * time.Second,
		RepoPath:         repoRoot,
		WorktreeRoot:     worktreeRoot,
		BranchTemplate:   "agent-{task_id}-from-{base_sha}",
		ClaudeBinaryPath: stubPath,
	}

	client := worker.NewClaudeClient(cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	outcome, err := client.Execute(ctx, worker.TaskEnvelope{
		ID: "task_test_e2e", WorkspaceID: "wksp_e", ProjectID: "proj_e",
	})
	require.NoError(t, err)
	assert.Equal(t, worker.OutcomeSuccess, outcome)

	// Stub manifest captures argv + env as exec received them.
	manifest, err := readStubManifest(t, captureDir)
	require.NoError(t, err, "stub captures never written — claude not invoked")

	// Argv carries the strict-mcp-config shape.
	argvJoined := joinArgs(manifest.Argv)
	assert.Contains(t, argvJoined, "--permission-mode bypassPermissions")
	assert.Contains(t, argvJoined, "--strict-mcp-config")
	assert.Contains(t, argvJoined, "--mcp-config")
	assert.Contains(t, argvJoined, "-p")

	// HOME landed at the cleansed tmpdir, not the operator's HOME.
	assert.NotEmpty(t, manifest.HomeEnv)
	assert.NotEqual(t, srcHome, manifest.HomeEnv, "stub claude saw operator HOME — cleansing failed")

	// Worktree exists at <root>/<task_id> and is the cmd.Dir.
	expectedWorktree := filepath.Join(worktreeRoot, "task_test_e2e")
	assert.Equal(t, expectedWorktree, manifest.PWD)
	st, err := os.Stat(expectedWorktree)
	require.NoError(t, err)
	assert.True(t, st.IsDir())

	// .satellites-agent.log exists inside the worktree.
	logPath := filepath.Join(expectedWorktree, ".satellites-agent.log")
	_, err = os.Stat(logPath)
	require.NoError(t, err, "subprocess log file missing")

	// MCP saw the four context fetches + a kind:agent-execute-evidence
	// ledger_append.
	calls := mcpCalls()
	tools := make(map[string]bool)
	for _, c := range calls {
		tools[c.Tool] = true
	}
	for _, want := range []string{"task_get", "agent_get", "contract_get", "story_get", "ledger_append"} {
		assert.True(t, tools[want], "missing MCP call %q (saw %v)", want, tools)
	}

	// Evidence row carries the kind:agent-execute-evidence tag.
	var sawEvidence bool
	for _, c := range calls {
		if c.Tool != "ledger_append" {
			continue
		}
		tagsAny, _ := c.Args["tags"].([]any)
		for _, tag := range tagsAny {
			if s, _ := tag.(string); s == "kind:agent-execute-evidence" {
				sawEvidence = true
			}
		}
	}
	assert.True(t, sawEvidence, "no ledger_append carried kind:agent-execute-evidence")

	// Worktree must be left in place on success (forensic policy).
	_, err = os.Stat(expectedWorktree)
	assert.NoError(t, err, "worktree was removed on success — AC requires forensic preservation")
}

// initGitRepo creates a real git repo with an initial commit. Returns
// the repo root.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return dir
}

// writeStubClaude writes a posix shell script that captures argv +
// env via newline-separated files (avoiding JSON escaping pitfalls
// for the multi-line prompt argument). Returns (script path,
// directory holding the captures).
func writeStubClaude(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "claude")
	script := `#!/bin/sh
set -e
echo "$PWD" > "` + dir + `/pwd"
echo "$HOME" > "` + dir + `/home"
: > "` + dir + `/argc"
i=0
for arg in "$@"; do
  printf '%s' "$arg" > "` + dir + `/argv-$i"
  i=$((i+1))
done
echo "$i" > "` + dir + `/argc"

# Touch a fake session JSONL so findSessionJSONL picks something up.
mkdir -p "$HOME/.claude/projects/fake"
echo '{"role":"user","content":"hello"}' > "$HOME/.claude/projects/fake/session.jsonl"

exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	return scriptPath, dir
}

type stubManifest struct {
	PWD     string
	HomeEnv string
	Argv    []string
}

// readStubManifest reads the captures the stub script wrote out and
// reconstructs the manifest. Treats a missing argc file as "stub did
// not run" — the caller decides whether that's a test failure.
func readStubManifest(t *testing.T, dir string) (stubManifest, error) {
	t.Helper()
	var m stubManifest
	pwd, err := os.ReadFile(filepath.Join(dir, "pwd"))
	if err != nil {
		return m, err
	}
	home, err := os.ReadFile(filepath.Join(dir, "home"))
	if err != nil {
		return m, err
	}
	argcRaw, err := os.ReadFile(filepath.Join(dir, "argc"))
	if err != nil {
		return m, err
	}
	m.PWD = trimNewline(string(pwd))
	m.HomeEnv = trimNewline(string(home))
	var argc int
	for _, b := range argcRaw {
		if b >= '0' && b <= '9' {
			argc = argc*10 + int(b-'0')
		}
	}
	for i := 0; i < argc; i++ {
		argRaw, err := os.ReadFile(filepath.Join(dir, "argv-"+itoa(i)))
		if err != nil {
			return m, err
		}
		m.Argv = append(m.Argv, string(argRaw))
	}
	return m, nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// stubMCP returns an httptest server responding to the verbs the
// claude client invokes. Calls are recorded and accessible via the
// returned closure.
type recordedMCPCall struct {
	Tool string
	Args map[string]any
}

func stubMCP(t *testing.T, taskID string) (func() []recordedMCPCall, string, func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedMCPCall

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		mu.Lock()
		calls = append(calls, recordedMCPCall{Tool: req.Params.Name, Args: req.Params.Arguments})
		mu.Unlock()

		var text string
		switch req.Params.Name {
		case "task_get":
			text = `{"id":"` + taskID + `","story_id":"sty_e2e","project_id":"proj_e","workspace_id":"wksp_e","agent_id":"doc_dev","action":"contract:develop","description":"do work"}`
		case "agent_get":
			text = `{"id":"doc_dev","name":"developer_agent"}`
		case "contract_get":
			text = `{"name":"develop"}`
		case "story_get":
			text = `{"story":{"id":"sty_e2e","title":"E2E story"}}`
		case "ledger_append":
			text = `{"id":"ldg_test"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		})
	}))
	getCalls := func() []recordedMCPCall {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedMCPCall, len(calls))
		copy(out, calls)
		return out
	}
	return getCalls, srv.URL, srv.Close
}

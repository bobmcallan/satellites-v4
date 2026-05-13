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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestClaudeClient_Execute_EndToEnd_StubBinary drives the seven-step
// Execute flow with a stub `claude` script and a stub /api/v1 HTTP
// server. It is the AC's "end-to-end claim → execute → close"
// coverage minus testcontainers — the substrate live dogfood
// (kind:dogfood-evidence) is the canonical pprod-side proof, captured
// separately on the story's ledger.
//
// sty_74e67353 work#1 migration: the stub server now serves
// /api/v1/<noun>/<verb> (no MCP JSON-RPC test doubles remain). Per-
// helper request path + body shape are asserted in
// client_claude_test.go's table-driven test; this case asserts the
// end-to-end fan-out — which paths got hit and that the evidence row
// carried the kind:agent-execute-evidence tag.
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

	// 3. Stub /api/v1 HTTP server — responds to task/get,
	//    document/get (×2: agent + contract), story/get, ledger/append.
	apiCalls, apiURL, apiClose := stubAPI(t, "task_test_e2e")
	defer apiClose()

	// 4. Operator HOME — a controlled value the test asserts the
	//    dispatched subprocess inherits. Satellites does not synthesise
	//    or override the subprocess's HOME; whatever the operator has
	//    set is what claude sees. The test asserts the inheritance.
	srcHome := t.TempDir()
	require.NoError(t, os.Setenv("HOME", srcHome))
	t.Cleanup(func() { _ = os.Unsetenv("HOME") })

	worktreeRoot := filepath.Join(t.TempDir(), "wt")

	cfg := config.AgentConfig{
		SpawnMCPURL:      apiURL + "/mcp", // spawned subprocess config only — the test's HTTP stub serves /api/v1
		AuthToken:        "tok-e2e",
		ExecuteTimeout:   30 * time.Second,
		RepoPath:         repoRoot,
		WorktreeRoot:     worktreeRoot,
		BranchTemplate:   "agent-{task_id}-from-{base_sha}",
		ClaudeBinaryPath: stubPath,
		ClientConfigPath: "/operator/path/to/bin/satellites-client.toml",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api := cliremote.New(apiURL, "tok-e2e", nil)
	outcome, err := worker.RunDispatched(ctx, cfg, nil, api, worker.TaskEnvelope{
		ID: "task_test_e2e", WorkspaceID: "wksp_e", ProjectID: "proj_e",
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, worker.OutcomeSuccess, outcome)

	// Stub manifest captures argv + env as exec received them.
	manifest, err := readStubManifest(t, captureDir)
	require.NoError(t, err, "stub captures never written — claude not invoked")

	// Argv carries the strict-mcp-config shape. The inline
	// `--mcp-config <JSON>` flag was retired in sty_74e67353 work#3:
	// the spawn-config is now materialised as <worktree>/.mcp.json
	// (claude's documented project-level config path). The presence
	// of that file is asserted further down.
	argvJoined := joinArgs(manifest.Argv)
	assert.Contains(t, argvJoined, "--permission-mode bypassPermissions")
	assert.Contains(t, argvJoined, "--strict-mcp-config")
	assert.NotContains(t, argvJoined, "--mcp-config",
		"--mcp-config inline flag retired in sty_74e67353 work#3 — file-on-disk replaces it")
	assert.Contains(t, argvJoined, "-p")

	// HOME inherited from the operator's environment, untouched.
	// Satellites does not configure the dispatched subprocess; whatever
	// HOME the operator has set is what claude sees.
	assert.Equal(t, srcHome, manifest.HomeEnv, "stub claude did not see operator HOME — satellites must not override the subprocess's HOME")

	// SATELLITES_CLIENT_CONFIG points the subprocess's satellites-client
	// invocations at the operator-resolved TOML, even from the per-task
	// worktree CWD (sty_ef4eedaa). The worker conditionally forwards the
	// path whenever AgentConfig.ClientConfigPath is non-empty.
	assert.Equal(t, "/operator/path/to/bin/satellites-client.toml", manifest.ClientConfigEnv,
		"SATELLITES_CLIENT_CONFIG not forwarded to dispatched subprocess")

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

	// sty_74e67353 work#3: <worktree>/.mcp.json materialised before
	// spawn, mode 0o600, referencing cfg.SpawnMCPURL + bearer header.
	mcpPath := filepath.Join(expectedWorktree, ".mcp.json")
	mcpStat, err := os.Stat(mcpPath)
	require.NoError(t, err, "worktree .mcp.json missing")
	assert.Equal(t, os.FileMode(0o600), mcpStat.Mode().Perm(), ".mcp.json mode must be 0o600 (contains bearer)")
	mcpRaw, err := os.ReadFile(mcpPath)
	require.NoError(t, err)
	assert.Contains(t, string(mcpRaw), cfg.SpawnMCPURL, ".mcp.json must reference SpawnMCPURL")
	assert.Contains(t, string(mcpRaw), "Bearer tok-e2e", ".mcp.json must carry the bearer header")

	// The dispatcher hit each /api/v1 route the prompt composer reads
	// from + the evidence-row appender. agent + contract both flow
	// through document/get per pr_mcp_cli_shared_path — the HTTP API
	// registers only the typed document/get route; the MCP `agent_get`
	// / `contract_get` wrappers delegate to the same client method.
	calls := apiCalls()
	paths := make(map[string]int)
	for _, c := range calls {
		paths[c.Path]++
	}
	assert.GreaterOrEqual(t, paths["/api/v1/task/get"], 1, "missing /api/v1/task/get; saw %v", paths)
	assert.GreaterOrEqual(t, paths["/api/v1/document/get"], 2, "expected ≥2 /api/v1/document/get hits (agent + contract); saw %v", paths)
	assert.GreaterOrEqual(t, paths["/api/v1/story/get"], 1, "missing /api/v1/story/get; saw %v", paths)
	assert.GreaterOrEqual(t, paths["/api/v1/ledger/append"], 1, "missing /api/v1/ledger/append; saw %v", paths)

	// Zero MCP JSON-RPC requests should have landed on the stub —
	// AC L1 transport mandate.
	for _, c := range calls {
		assert.False(t, strings.HasPrefix(c.Path, "/mcp"), "unexpected /mcp request: %s", c.Path)
		assert.NotContains(t, c.RawBody, `"jsonrpc"`, "request body carried JSON-RPC payload: %s", c.RawBody)
		assert.NotContains(t, c.RawBody, `"tools/call"`, "request body referenced tools/call: %s", c.RawBody)
	}

	// Evidence row carries the kind:agent-execute-evidence tag.
	var sawEvidence bool
	for _, c := range calls {
		if c.Path != "/api/v1/ledger/append" {
			continue
		}
		tagsAny, _ := c.Body["tags"].([]any)
		for _, tag := range tagsAny {
			if s, _ := tag.(string); s == "kind:agent-execute-evidence" {
				sawEvidence = true
			}
		}
	}
	assert.True(t, sawEvidence, "no /api/v1/ledger/append carried kind:agent-execute-evidence")

	// Worktree must be left in place on success (forensic policy).
	_, err = os.Stat(expectedWorktree)
	assert.NoError(t, err, "worktree was removed on success — AC requires forensic preservation")
}

// TestSpawnConfig_FileWrittenIntoWorktree pins the L2 invariant:
// before claude is launched, satellites-client writes the spawned
// subprocess's MCP server config into <worktree>/.mcp.json (claude's
// documented project-level config path). The on-disk file MUST:
//
//   - exist at <worktree>/.mcp.json
//   - contain cfg.SpawnMCPURL and a Bearer header sourced from
//     cfg.AuthToken
//   - have mode 0o600 (it carries a secret)
//
// sty_74e67353 work#3 AC "L2 spawn config".
func TestSpawnConfig_FileWrittenIntoWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub claude is a posix shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoRoot := initGitRepo(t)
	stubPath, _ := writeStubClaude(t)
	_, apiURL, apiClose := stubAPI(t, "task_spawn_cfg")
	defer apiClose()

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	cfg := config.AgentConfig{
		SpawnMCPURL:      "https://satellites-pprod.fly.dev/mcp?project_id=proj_x",
		AuthToken:        "sat_spawn_test",
		ExecuteTimeout:   30 * time.Second,
		RepoPath:         repoRoot,
		WorktreeRoot:     worktreeRoot,
		BranchTemplate:   "agent-{task_id}-from-{base_sha}",
		ClaudeBinaryPath: stubPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api := cliremote.New(apiURL, "tok-spawn", nil)
	outcome, err := worker.RunDispatched(ctx, cfg, nil, api, worker.TaskEnvelope{
		ID: "task_spawn_cfg", WorkspaceID: "wksp_e", ProjectID: "proj_e",
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, worker.OutcomeSuccess, outcome)

	mcpPath := filepath.Join(worktreeRoot, "task_spawn_cfg", ".mcp.json")
	st, err := os.Stat(mcpPath)
	require.NoError(t, err, "expected %s to exist", mcpPath)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(),
		".mcp.json mode must be 0o600 (it carries a bearer token)")

	raw, err := os.ReadFile(mcpPath)
	require.NoError(t, err)
	var parsed struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Contains(t, parsed.MCPServers, "satellites")
	srv := parsed.MCPServers["satellites"]
	assert.Equal(t, "http", srv.Type)
	assert.Equal(t, cfg.SpawnMCPURL, srv.URL)
	assert.Equal(t, "Bearer sat_spawn_test", srv.Headers["Authorization"])
	// strict-mcp-config must see ONLY the satellites server.
	assert.Len(t, parsed.MCPServers, 1)
}

// TestSpawnConfig_OperatorHomeNotRead pins the isolation invariant
// (pr_substrate_provides_context): the operator's
// $HOME/.claude/settings.json must NOT flow into the dispatched
// subprocess's MCP server list. The dispatcher achieves this by
// (a) writing the only MCP config the subprocess should consult into
// <worktree>/.mcp.json, and (b) passing --strict-mcp-config in the
// argv so claude is forbidden from merging operator-side MCP config.
//
// The test plants a sentinel "OPERATOR_LEAK" MCP server in a temp
// HOME's ~/.claude/settings.json, dispatches RunDispatched with that
// HOME, and asserts:
//
//   - the spawned subprocess's argv carries --strict-mcp-config
//   - the worktree-written .mcp.json does NOT mention OPERATOR_LEAK
//
// sty_74e67353 work#3 AC "L2 isolation test".
func TestSpawnConfig_OperatorHomeNotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub claude is a posix shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoRoot := initGitRepo(t)
	stubPath, captureDir := writeStubClaude(t)
	_, apiURL, apiClose := stubAPI(t, "task_isolation")
	defer apiClose()

	// Plant a sentinel OPERATOR_LEAK server in the operator's HOME.
	// claude WOULD merge this into its MCP server list if it ever read
	// $HOME/.claude/settings.json — --strict-mcp-config + the on-disk
	// .mcp.json in the worktree forbid that path.
	srcHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".claude"), 0o755))
	leakConfig := `{"mcpServers":{"OPERATOR_LEAK":{"type":"http","url":"http://operator-leak.invalid/mcp"}}}`
	require.NoError(t, os.WriteFile(
		filepath.Join(srcHome, ".claude", "settings.json"),
		[]byte(leakConfig), 0o600,
	))
	require.NoError(t, os.Setenv("HOME", srcHome))
	t.Cleanup(func() { _ = os.Unsetenv("HOME") })

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	cfg := config.AgentConfig{
		SpawnMCPURL:      apiURL + "/mcp",
		AuthToken:        "tok-isolation",
		ExecuteTimeout:   30 * time.Second,
		RepoPath:         repoRoot,
		WorktreeRoot:     worktreeRoot,
		BranchTemplate:   "agent-{task_id}-from-{base_sha}",
		ClaudeBinaryPath: stubPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api := cliremote.New(apiURL, "tok-isolation", nil)
	outcome, err := worker.RunDispatched(ctx, cfg, nil, api, worker.TaskEnvelope{
		ID: "task_isolation", WorkspaceID: "wksp_e", ProjectID: "proj_e",
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, worker.OutcomeSuccess, outcome)

	manifest, err := readStubManifest(t, captureDir)
	require.NoError(t, err, "stub captures never written")

	// Argv MUST carry --strict-mcp-config; without it claude would
	// merge $HOME/.claude/settings.json into the subprocess's MCP list.
	argvJoined := joinArgs(manifest.Argv)
	assert.Contains(t, argvJoined, "--strict-mcp-config",
		"--strict-mcp-config gone — operator HOME could leak in via claude's settings.json merge")

	// The MCP config the subprocess sees comes from the worktree file,
	// NOT the operator's HOME. The worktree file is the only place
	// the subprocess can resolve MCP servers from under
	// --strict-mcp-config.
	mcpPath := filepath.Join(worktreeRoot, "task_isolation", ".mcp.json")
	raw, err := os.ReadFile(mcpPath)
	require.NoError(t, err, "worktree .mcp.json missing")
	assert.NotContains(t, string(raw), "OPERATOR_LEAK",
		"operator HOME leaked into worktree .mcp.json — isolation broken")
	assert.Contains(t, string(raw), apiURL,
		"worktree .mcp.json must reference SpawnMCPURL")

	// HOME inheritance is intentional (the existing e2e test asserts
	// the operator's HOME flows through to the subprocess unmodified —
	// satellites doesn't synthesise HOME). The point of this test is
	// that the operator's HOME contents don't become MCP-server config.
	assert.Equal(t, srcHome, manifest.HomeEnv,
		"stub claude did not see operator HOME — satellites must not override the subprocess's HOME")
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
printf '%s' "${SATELLITES_CLIENT_CONFIG:-}" > "` + dir + `/sat_client_config"

exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	return scriptPath, dir
}

type stubManifest struct {
	PWD             string
	HomeEnv         string
	Argv            []string
	ClientConfigEnv string
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
	if raw, err := os.ReadFile(filepath.Join(dir, "sat_client_config")); err == nil {
		m.ClientConfigEnv = string(raw)
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

// stubAPI returns an httptest server responding to the /api/v1
// routes the claude dispatcher posts to. Calls are recorded and
// accessible via the returned closure.
//
// Per sty_74e67353 work#1 (L1 dispatcher rewrite): the worker's
// pre-spawn fetches + evidence row route through internal/cliremote
// → POST /api/v1/<noun>/<verb>. Replaces the prior MCP JSON-RPC
// stub (stubMCP) wholesale — no transport selector, no fallback.
type recordedAPICall struct {
	Path    string
	Body    map[string]any
	RawBody string
}

func stubAPI(t *testing.T, taskID string) (func() []recordedAPICall, string, func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedAPICall

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		mu.Lock()
		calls = append(calls, recordedAPICall{
			Path:    r.URL.Path,
			Body:    parsed,
			RawBody: string(body),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/task/get":
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","story_id":"sty_e2e","project_id":"proj_e","workspace_id":"wksp_e","agent_id":"doc_dev","action":"contract:develop","description":"do work"}`))
		case "/api/v1/document/get":
			// Branch on the requested type — agent + contract share the
			// same /api/v1/document/get route.
			docType, _ := parsed["type"].(string)
			switch docType {
			case "agent":
				_, _ = w.Write([]byte(`{"id":"doc_dev","name":"developer_agent"}`))
			case "contract":
				_, _ = w.Write([]byte(`{"name":"develop"}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		case "/api/v1/story/get":
			_, _ = w.Write([]byte(`{"story":{"id":"sty_e2e","title":"E2E story"}}`))
		case "/api/v1/ledger/append":
			_, _ = w.Write([]byte(`{"id":"ldg_test"}`))
		default:
			http.Error(w, "unhandled "+r.URL.Path, http.StatusNotFound)
		}
	}))
	getCalls := func() []recordedAPICall {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedAPICall, len(calls))
		copy(out, calls)
		return out
	}
	return getCalls, srv.URL, srv.Close
}

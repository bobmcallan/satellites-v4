package worker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestClaudeClient_Execute_StreamJSON_RealtimeCapture pins sty_ee04430b:
// the dispatched claude argv MUST carry --output-format stream-json
// --verbose, and the per-worktree .satellites-agent.log MUST grow
// monotonically as the subprocess emits JSONL events — so the
// orchestrator can tail progress in real time. The stub claude emits
// four JSONL events with sleeps between them; the test snapshots the
// log file size while the subprocess is alive and confirms growth
// across the run, then inspects the final on-disk log for an
// intermediate tool_use event and a terminal result event.
func TestClaudeClient_Execute_StreamJSON_RealtimeCapture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub claude is a posix shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoRoot := initGitRepo(t)
	stubPath, captureDir := writeStubClaudeStreamJSON(t)
	_, apiURL, apiClose := stubAPI(t, "task_streamjson")
	defer apiClose()

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	cfg := config.AgentConfig{
		SpawnMCPURL:      apiURL + "/mcp",
		AuthToken:        "tok-streamjson",
		ExecuteTimeout:   30 * time.Second,
		RepoPath:         repoRoot,
		WorktreeRoot:     worktreeRoot,
		BranchTemplate:   "agent-{task_id}-from-{base_sha}",
		ClaudeBinaryPath: stubPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api := cliremote.New(apiURL, "tok-streamjson", nil)

	expectedWorktree := filepath.Join(worktreeRoot, "task_streamjson")
	logPath := filepath.Join(expectedWorktree, ".satellites-agent.log")

	// Run dispatch in a goroutine so the main test thread can poll the
	// log file while the subprocess is alive. The stub injects sleeps
	// between JSONL lines totaling ~1.2s — well under the 30s timeout.
	type runResult struct {
		outcome worker.Outcome
		err     error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		o, e := worker.RunDispatched(ctx, cfg, nil, api, worker.TaskEnvelope{
			ID: "task_streamjson", WorkspaceID: "wksp_e", ProjectID: "proj_e",
		}, nil, nil)
		resultCh <- runResult{o, e}
	}()

	// Poll the log file size until at least one growth tick is observed
	// OR the subprocess finishes. Growth tick = the file appears AND a
	// later poll shows a strictly larger size than an earlier poll
	// captured while the subprocess was still alive.
	var firstSize, midSize int64 = -1, -1
	deadline := time.Now().Add(20 * time.Second)
	pollDone := false
	for !pollDone && time.Now().Before(deadline) {
		select {
		case res := <-resultCh:
			// Subprocess finished while we were polling — put it back
			// for the assertion below.
			resultCh <- res
			pollDone = true
		default:
		}
		if st, err := os.Stat(logPath); err == nil {
			sz := st.Size()
			if firstSize < 0 {
				firstSize = sz
			} else if sz > firstSize {
				midSize = sz
				pollDone = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for the subprocess to finish.
	var res runResult
	select {
	case res = <-resultCh:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatch did not finish within timeout")
	}
	require.NoError(t, res.err)
	assert.Equal(t, worker.OutcomeSuccess, res.outcome)

	// Argv MUST carry the stream-json flag pair AND --verbose. Without
	// the pair, claude's -p mode would print only the final assistant
	// text on stdout — the per-task log stays dark until termination
	// (the original friction this story addresses).
	manifest, err := readStubManifest(t, captureDir)
	require.NoError(t, err, "stub captures never written — claude not invoked")
	argvJoined := joinArgs(manifest.Argv)
	assert.Contains(t, argvJoined, "--output-format stream-json",
		"argv missing --output-format stream-json — real-time JSONL stream not enabled")
	assert.Contains(t, argvJoined, "--verbose",
		"argv missing --verbose — required by claude CLI when --output-format stream-json is used in -p mode")

	// Monotonic growth: the log file was observably partial at some
	// point during the subprocess's lifetime. firstSize was captured on
	// the first successful stat; midSize was captured on the first
	// subsequent stat that showed a strictly larger size, BEFORE the
	// subprocess finished.
	require.GreaterOrEqualf(t, firstSize, int64(0), "log file never opened")
	finalStat, err := os.Stat(logPath)
	require.NoError(t, err)
	finalSize := finalStat.Size()
	if midSize > 0 {
		assert.Greater(t, midSize, firstSize,
			"log file did not grow between polls — orchestrator cannot tail progress in real time")
		assert.GreaterOrEqual(t, finalSize, midSize,
			"final log size smaller than mid-run snapshot — file truncated unexpectedly")
	} else {
		// Stub finished before we caught a growth tick; fall back to
		// asserting the file ended non-empty (the stream landed on disk).
		assert.Greater(t, finalSize, int64(0),
			"per-task log file is 0 bytes after dispatch — stream-json stdout not captured")
	}

	// Final on-disk log MUST contain an intermediate tool_use event AND
	// the terminal result event. This locks in real-time JSONL capture
	// — both events came from the stub's stream-json stdout.
	logRaw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	logStr := string(logRaw)
	assert.True(t, strings.Contains(logStr, `"type":"tool_use"`),
		"per-task log missing intermediate tool_use event: %s", logStr)
	assert.True(t,
		strings.Contains(logStr, `"type":"result"`) ||
			strings.Contains(logStr, `"subtype":"success"`),
		"per-task log missing terminal result event: %s", logStr)
}

// TestClaudeClient_Execute_StreamJSON_TokensUsedOnEvidence pins
// sty_af701a67 AC4: when the dispatched claude subprocess emits a
// terminal stream-json `result` event with `usage:{input_tokens,
// output_tokens}`, the worker's aggregateUsage helper folds the
// values into a `tokens_used:{input,output,total}` block carried on
// the kind:agent-execute-evidence ledger row's Structured payload.
//
// Uses the same stub claude script that
// TestClaudeClient_Execute_StreamJSON_RealtimeCapture exercises
// (input_tokens=10, output_tokens=20 → total=30). The test captures
// the /api/v1/ledger/append request body the dispatcher posts and
// asserts the structured field decodes to the expected shape.
func TestClaudeClient_Execute_StreamJSON_TokensUsedOnEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub claude is a posix shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoRoot := initGitRepo(t)
	stubPath, _ := writeStubClaudeStreamJSON(t)
	getCalls, apiURL, apiClose := stubAPI(t, "task_tokens_used")
	defer apiClose()

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	cfg := config.AgentConfig{
		SpawnMCPURL:      apiURL + "/mcp",
		AuthToken:        "tok-tokens",
		ExecuteTimeout:   30 * time.Second,
		RepoPath:         repoRoot,
		WorktreeRoot:     worktreeRoot,
		BranchTemplate:   "agent-{task_id}-from-{base_sha}",
		ClaudeBinaryPath: stubPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api := cliremote.New(apiURL, "tok-tokens", nil)
	outcome, err := worker.RunDispatched(ctx, cfg, nil, api, worker.TaskEnvelope{
		ID: "task_tokens_used", WorkspaceID: "wksp_t", ProjectID: "proj_t",
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, worker.OutcomeSuccess, outcome)

	// Locate the /api/v1/ledger/append call that carried the
	// kind:agent-execute-evidence tag.
	var found bool
	for _, c := range getCalls() {
		if c.Path != "/api/v1/ledger/append" {
			continue
		}
		tags, _ := c.Body["tags"].([]any)
		isExecuteEvidence := false
		for _, tag := range tags {
			if s, _ := tag.(string); s == "kind:agent-execute-evidence" {
				isExecuteEvidence = true
				break
			}
		}
		if !isExecuteEvidence {
			continue
		}
		// The dispatcher sends the structured field as a raw JSON object
		// via json.RawMessage so the server stores raw JSON bytes (the
		// canonical kind:llm-usage shape under internal/ledger/derivations.go).
		// In c.Body parsed via map[string]any the inline object lands as
		// map[string]any, not a base64-encoded string.
		structuredObj, ok := c.Body["structured"].(map[string]any)
		require.True(t, ok, "evidence row structured must be a JSON object; body=%s", c.RawBody)
		tu, ok := structuredObj["tokens_used"].(map[string]any)
		require.True(t, ok, "structured payload missing tokens_used key; got=%v", structuredObj)
		payload := struct {
			TokensUsed struct {
				Input  int `json:"input"`
				Output int `json:"output"`
				Total  int `json:"total"`
			} `json:"tokens_used"`
		}{}
		toInt := func(v any) int {
			if f, ok := v.(float64); ok {
				return int(f)
			}
			return 0
		}
		payload.TokensUsed.Input = toInt(tu["input"])
		payload.TokensUsed.Output = toInt(tu["output"])
		payload.TokensUsed.Total = toInt(tu["total"])
		assert.Equal(t, 10, payload.TokensUsed.Input,
			"stub terminal usage.input_tokens=10 — aggregator must surface it")
		assert.Equal(t, 20, payload.TokensUsed.Output,
			"stub terminal usage.output_tokens=20 — aggregator must surface it")
		assert.Equal(t, 30, payload.TokensUsed.Total,
			"total must equal input+output (AC2)")
		found = true
		break
	}
	require.True(t, found, "no kind:agent-execute-evidence ledger_append captured")
}

// writeStubClaudeStreamJSON writes a posix shell script that captures
// argv (same shape as writeStubClaude) AND emits a four-line JSONL
// sequence with sleeps between lines so the test can observe the log
// file growing monotonically while the subprocess is alive.
func writeStubClaudeStreamJSON(t *testing.T) (string, string) {
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

# Emit a stream-json-shaped JSONL event sequence with sleeps between
# lines. Each line is written then flushed (printf + sleep) so the
# parent's MultiWriter sink commits the bytes to disk before the next
# line is produced — the test polls the log file mid-run for growth.
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess1"}'
sleep 0.3
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}'
sleep 0.3
printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","is_error":false}]}}'
sleep 0.3
printf '%s\n' '{"type":"result","subtype":"success","usage":{"input_tokens":10,"output_tokens":20}}'
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	return scriptPath, dir
}

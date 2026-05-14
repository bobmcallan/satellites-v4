package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// Tests live in the worker package (not worker_test) because they
// exercise unexported helpers — runHotPath, the per-contract runners,
// requiredClosingFieldsMissing, etc.
//
// Per sty_74e67353 work#2: the hot-path runners route through
// internal/cliremote.Client → POST /api/v1/<noun>/<verb>. The test
// double is an httptest.NewServer serving those routes. The prior
// MCP JSON-RPC stub (roundTripperFunc on cc.http.Transport) is gone.

// hotpathStub composes a *claudeClient backed by an httptest server
// responding to /api/v1/<noun>/<verb> + a mock gitRunner suitable for
// asserting against the recorded command stream + ledger row contents.
//
// ghCmds + ghOutputs + ghErrors mock the gh CLI shell-out the
// merge_to_main release runner makes (run list + run watch).
// pprodCommits is the scripted commit sequence the converge poll
// observes — each entry is consumed in order, and the runner is
// expected to see the final entry match the pushed SHA.
type hotpathStub struct {
	t            *testing.T
	gitCmds      [][]string
	gitOutputs   map[string]string // joined args → stdout
	gitErrors    map[string]error  // joined args → error
	ghCmds       [][]string
	ghOutputs    map[string]string // joined args → stdout
	ghErrors     map[string]error  // joined args → error
	pprodCommits []string          // scripted converge-poll responses
	pprodCalls   int
	apiCalls     []recordedAPICall
	apiResp      map[string]string // path → response body (JSON)
	mu           sync.Mutex
	srv          *httptest.Server
}

// recordedAPICall captures one POST /api/v1/<noun>/<verb> request.
type recordedAPICall struct {
	Path string
	Body map[string]any
}

func newHotpathStub(t *testing.T) *hotpathStub {
	return &hotpathStub{
		t:          t,
		gitOutputs: map[string]string{},
		gitErrors:  map[string]error{},
		ghOutputs:  map[string]string{},
		ghErrors:   map[string]error{},
		apiResp:    map[string]string{},
	}
}

func (s *hotpathStub) gitRunner(_ context.Context, dir string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := append([]string{"DIR=" + dir}, args...)
	s.gitCmds = append(s.gitCmds, cmd)
	joined := strings.Join(args, " ")
	if err := s.gitErrors[joined]; err != nil {
		return []byte(s.gitOutputs[joined]), err
	}
	return []byte(s.gitOutputs[joined]), nil
}

// ghRunner records gh-CLI invocations and returns the scripted
// stdout/err pair. The runner uses prefix matching on the joined
// args so tests can register "run list" once and have it apply to
// `run list --branch main --limit 1 --json …`.
func (s *hotpathStub) ghRunner(ctx context.Context, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghCmds = append(s.ghCmds, args)
	joined := strings.Join(args, " ")
	for key, err := range s.ghErrors {
		if strings.HasPrefix(joined, key) {
			out := s.ghOutputs[key]
			return []byte(out), err
		}
	}
	for key, out := range s.ghOutputs {
		if strings.HasPrefix(joined, key) {
			// Honour context cancellation so timeout-driven cases can
			// surface ctx.Err() the same way the production gh shell-out
			// would on a watch that hangs past the deadline.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return []byte(out), nil
		}
	}
	return nil, fmt.Errorf("hotpathStub: no gh scripted response for %q", joined)
}

// pprodFetcher returns the next scripted commit. When pprodCommits
// is exhausted, the last entry is repeated indefinitely. Tests
// scripting the converge timeout case set pprodCommits to a stale
// value the runner never matches.
func (s *hotpathStub) pprodFetcher(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pprodCommits) == 0 {
		return "", fmt.Errorf("hotpathStub: no pprod commits scripted")
	}
	idx := s.pprodCalls
	if idx >= len(s.pprodCommits) {
		idx = len(s.pprodCommits) - 1
	}
	s.pprodCalls++
	return s.pprodCommits[idx], nil
}

// setAPIResp registers the response body for a tool's /api/v1/<noun>/<verb>
// path. toolName uses the `<noun>_<verb>` form (e.g. "task_walk").
func (s *hotpathStub) setAPIResp(toolName, body string) {
	s.apiResp["/api/v1"+cliremote.ToolNameToPath(toolName)] = body
}

// client builds a *claudeClient whose api field POSTs to an
// httptest.NewServer serving /api/v1 shape responses. Each request is
// recorded for assertion.
func (s *hotpathStub) client(cfg config.AgentConfig) *claudeClient {
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(s.t, err)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		s.mu.Lock()
		s.apiCalls = append(s.apiCalls, recordedAPICall{
			Path: r.URL.Path,
			Body: parsed,
		})
		resp, ok := s.apiResp[r.URL.Path]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			// Default to empty-object so the substrate call returns
			// nil-error; per-test apiResp entries supply realistic
			// payloads where the runner reads them.
			resp = "{}"
		}
		_, _ = w.Write([]byte(resp))
	}))
	s.t.Cleanup(s.srv.Close)

	cc := newClaudeClient(cfg, nil)
	cc.api = cliremote.New(s.srv.URL, "tok", nil)
	cc.gitRunner = s.gitRunner
	cc.ghRunner = s.ghRunner
	cc.pprodInfoFetcher = s.pprodFetcher
	// Test-friendly defaults so converge polls don't sleep for seconds.
	cc.pprodPollInterval = time.Millisecond
	cc.pprodConvergeTimeout = 500 * time.Millisecond
	cc.ghWatchTimeout = 500 * time.Millisecond
	return cc
}

// findAPICall returns the first recorded call to the given /api/v1
// path. toolName uses the `<noun>_<verb>` form.
func (s *hotpathStub) findAPICall(toolName string) *recordedAPICall {
	target := "/api/v1" + cliremote.ToolNameToPath(toolName)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.apiCalls {
		if s.apiCalls[i].Path == target {
			return &s.apiCalls[i]
		}
	}
	return nil
}

// TestRunHotPath_DispatchesByContractName: the selector lookup table
// covers commit / merge_to_main and returns errHotUnimplemented for
// anything else.
func TestRunHotPath_DispatchesByContractName(t *testing.T) {
	// Unknown contract → errHotUnimplemented sentinel so Execute
	// falls back to the heavy path.
	s := newHotpathStub(t)
	cc := s.client(config.AgentConfig{})
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_test"},
		taskInfo{ID: "task_test", Action: "contract:bogus"},
		agentInfo{},
		contractInfo{Name: "bogus"},
		storyInfo{})
	assert.Equal(t, OutcomeFailure, outcome)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errHotUnimplemented),
		"unknown contract should return errHotUnimplemented sentinel; got %v", err)
}

// TestRunCommitHotPath_HappyPath drives the full commit runner:
// trigger-supplied branch wins, gitRunner records the three expected
// commands (log, push, ls-remote — branch resolution short-circuits
// via trigger), and the ledger evidence row carries the renamed
// `phase:commit` tag.
func TestRunCommitHotPath_HappyPath(t *testing.T) {
	s := newHotpathStub(t)
	s.gitOutputs["log -1 --pretty=fuller agent-task_dev-from-833a28e"] =
		"commit deadbeef\nAuthor: ...\n\n    refactor(auth): foo\n"
	s.gitOutputs["push origin agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"] =
		"To github.com:org/repo.git\n * [new branch]      agent-task_dev-from-833a28e -> agent-task_dev-from-833a28e\n"
	s.gitOutputs["ls-remote origin agent-task_dev-from-833a28e"] =
		"deadbeefcafe\trefs/heads/agent-task_dev-from-833a28e\n"
	s.setAPIResp("ledger_append", `{"id":"ldg_xxx"}`)
	s.setAPIResp("task_update", `{"task_id":"task_commit","status":"closed","outcome":"success"}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})

	trigger, _ := json.Marshal(map[string]string{"branch": "agent-task_dev-from-833a28e"})
	ti := taskInfo{
		ID: "task_commit", StoryID: "sty_target", ProjectID: "proj_x",
		Action: "contract:commit", Trigger: trigger,
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_commit", ProjectID: "proj_x"},
		ti, agentInfo{Name: "releaser_agent"},
		contractInfo{Name: "commit"},
		storyInfo{ID: "sty_target"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)

	// gitRunner saw exactly three commands (log, push, ls-remote) —
	// branch resolution via trigger skipped the chain walk.
	require.Len(t, s.gitCmds, 3, "trigger override should skip branch-list git call")
	assert.Equal(t, []string{"DIR=/repo", "log", "-1", "--pretty=fuller", "agent-task_dev-from-833a28e"}, s.gitCmds[0])
	assert.Equal(t, []string{"DIR=/repo", "push", "origin", "agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"}, s.gitCmds[1])
	assert.Equal(t, []string{"DIR=/repo", "ls-remote", "origin", "agent-task_dev-from-833a28e"}, s.gitCmds[2])

	// Evidence row tags carry phase:commit (renamed from phase:push).
	led := s.findAPICall("ledger_append")
	require.NotNil(t, led, "ledger_append must be called via /api/v1/ledger/append")
	tagsAny, _ := led.Body["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "task_id:task_commit")
	assert.Contains(t, tags, "story_id:sty_target")
	assert.Contains(t, tags, "phase:commit")
	assert.Contains(t, tags, "branch:agent-task_dev-from-833a28e")
	assert.Contains(t, tags, "kind:evidence")
	assert.Contains(t, tags, "dispatch_class:hot")

	// task_update issued at the end of the runner.
	require.NotNil(t, s.findAPICall("task_update"))
}

// TestRunCommitHotPath_InferBranchFromChain: with no trigger, the
// runner walks the task chain, finds the latest contract:develop
// success, and resolves agent-<dev>-from-* via git branch --list.
func TestRunCommitHotPath_InferBranchFromChain(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_plan", Action: "contract:plan", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_dev_a", Action: "contract:develop", Kind: "work", Iteration: 1, Status: "closed", Outcome: "failure"},
		{ID: "task_dev_b", Action: "contract:develop", Kind: "work", Iteration: 2, Status: "closed", Outcome: "success"},
		{ID: "task_dev_rev", Action: "contract:develop", Kind: "review", Status: "closed", Outcome: "success"},
		{ID: "task_commit", Action: "contract:commit", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))
	s.gitOutputs["branch --list agent-task_dev_b-from-*"] = "  agent-task_dev_b-from-833a28e\n"
	s.gitOutputs["log -1 --pretty=fuller agent-task_dev_b-from-833a28e"] = "commit feedface\n"
	s.gitOutputs["push origin agent-task_dev_b-from-833a28e:agent-task_dev_b-from-833a28e"] =
		"To origin\n * [new branch]      agent-task_dev_b-from-833a28e -> agent-task_dev_b-from-833a28e\n"
	s.gitOutputs["ls-remote origin agent-task_dev_b-from-833a28e"] =
		"feedface\trefs/heads/agent-task_dev_b-from-833a28e\n"
	s.setAPIResp("ledger_append", `{"id":"ldg_y"}`)
	s.setAPIResp("task_update", `{}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})

	ti := taskInfo{
		ID: "task_commit", StoryID: "sty_x", ProjectID: "proj_x",
		Action: "contract:commit",
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_commit", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "commit"}, storyInfo{ID: "sty_x"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)

	// branch --list invocation should target the LATEST successful
	// develop iteration (task_dev_b), not iter-1 failure (task_dev_a).
	var sawBranchList bool
	for _, cmd := range s.gitCmds {
		if len(cmd) >= 4 && cmd[1] == "branch" && cmd[2] == "--list" && cmd[3] == "agent-task_dev_b-from-*" {
			sawBranchList = true
			break
		}
	}
	assert.True(t, sawBranchList, "branch --list should target the latest successful develop iteration; got %v", s.gitCmds)
}

// seedMergeToMainHappyStubs primes the gitOutputs + ghOutputs + pprod
// converge sequence + walk response for a PASS run of the extended
// merge_to_main release runner. The pushedSHA is the SHA the final
// `rev-parse main` returns AND the converge poll expects to observe.
func seedMergeToMainHappyStubs(t *testing.T, s *hotpathStub, pushedSHA string) {
	t.Helper()
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_plan", Action: "contract:plan", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_commit", Action: "contract:commit", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_merge", Action: "contract:merge_to_main", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))
	s.gitOutputs["fetch origin --quiet"] = ""
	s.gitOutputs["rev-parse main"] = pushedSHA + "\n"
	s.gitOutputs["merge-base --is-ancestor main agent-task_dev-from-833a28e"] = ""
	s.gitOutputs["merge --ff-only agent-task_dev-from-833a28e"] =
		"Updating abc123.." + pushedSHA + "\nFast-forward\n internal/auth/middleware.go | 10 ++++++++++\n"
	s.gitOutputs["push origin main"] = "To origin\n   abc123.." + pushedSHA + "  main -> main\n"
	s.ghOutputs["run list"] = fmt.Sprintf(`[{"databaseId":12345,"headSha":"%s","status":"completed","conclusion":"success"}]`, pushedSHA)
	s.ghOutputs["run watch"] = "✓ release deploy · main\n12345 · success\n"
	s.pprodCommits = []string{"stale-1", pushedSHA}
	s.setAPIResp("ledger_append", `{"id":"ldg_m"}`)
	s.setAPIResp("task_update", `{}`)
}

// runMergeToMainTask drives the merge_to_main runner with the
// trigger-supplied branch and returns the outcome + error.
func runMergeToMainTask(t *testing.T, cc *claudeClient) (Outcome, error) {
	t.Helper()
	trigger, _ := json.Marshal(map[string]string{"branch": "agent-task_dev-from-833a28e"})
	ti := taskInfo{
		ID: "task_merge", StoryID: "sty_m", ProjectID: "proj_x",
		Action: "contract:merge_to_main", Trigger: trigger,
	}
	return cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_merge", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "merge_to_main"}, storyInfo{ID: "sty_m"})
}

// TestRunMergeToMainHotPath_ReleasePass — PASS path. The extended
// release runner: fast-forwards main, pushes main to origin, watches
// the GH workflow run to success, polls pprod's satellites_info until
// reported commit matches the pushed SHA. Writes a single
// `kind:release-evidence` ledger row carrying SHAs + GH run id +
// converge samples.
func TestRunMergeToMainHotPath_ReleasePass(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)

	// gitRunner sequence covers the extended release: fetch, rev-parse,
	// ancestor, merge, rev-parse, push-main.
	expectedHead := []string{"fetch", "rev-parse", "merge-base", "merge", "rev-parse", "push"}
	require.GreaterOrEqual(t, len(s.gitCmds), len(expectedHead))
	for i, want := range expectedHead {
		assert.Equal(t, want, s.gitCmds[i][1], "git command #%d should be %q (got %v)", i, want, s.gitCmds[i])
	}

	// gh CLI shell-out covers run list + run watch.
	var sawRunList, sawRunWatch bool
	for _, cmd := range s.ghCmds {
		joined := strings.Join(cmd, " ")
		if strings.HasPrefix(joined, "run list") {
			sawRunList = true
		}
		if strings.HasPrefix(joined, "run watch") {
			sawRunWatch = true
		}
	}
	assert.True(t, sawRunList, "gh run list expected; got %v", s.ghCmds)
	assert.True(t, sawRunWatch, "gh run watch expected; got %v", s.ghCmds)

	// kind:release-evidence with pushed_sha + gh_run_id tags.
	led := s.findAPICall("ledger_append")
	require.NotNil(t, led)
	tagsAny, _ := led.Body["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "phase:merge_to_main")
	assert.Contains(t, tags, "dispatch_class:hot")
	assert.Contains(t, tags, "kind:release-evidence")
	assert.Contains(t, tags, "pushed_sha:deadbeef")
	assert.Contains(t, tags, "gh_run_id:12345")
}

// TestRunMergeToMainHotPath_GHTimeout — gh run watch hangs past the
// configured timeout. The runner returns failure + no
// kind:release-evidence row is appended.
func TestRunMergeToMainHotPath_GHTimeout(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	// gh run watch hangs forever: the stub's ctx.Err() path returns the
	// deadline once the watch ctx fires.
	s.ghOutputs["run watch"] = ""
	s.ghErrors["run watch"] = context.DeadlineExceeded

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "gh run watch")

	// No kind:release-evidence row appended on failure.
	led := s.findAPICall("ledger_append")
	if led != nil {
		tagsAny, _ := led.Body["tags"].([]any)
		for _, v := range tagsAny {
			if v == "kind:release-evidence" {
				t.Errorf("release-evidence row appended on GH timeout: %+v", led)
			}
		}
	}
}

// TestRunMergeToMainHotPath_GHFailure — gh run watch returns
// conclusion=failure (exec.Command exits non-zero with stderr). The
// runner returns failure + no kind:release-evidence row.
func TestRunMergeToMainHotPath_GHFailure(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	s.ghOutputs["run watch"] = "✗ release deploy · failed\n12345 · failure\n"
	s.ghErrors["run watch"] = errors.New("exit status 1")

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "gh run watch")

	led := s.findAPICall("ledger_append")
	if led != nil {
		tagsAny, _ := led.Body["tags"].([]any)
		for _, v := range tagsAny {
			if v == "kind:release-evidence" {
				t.Errorf("release-evidence row appended on GH failure: %+v", led)
			}
		}
	}
}

// TestRunMergeToMainHotPath_PprodConvergeTimeout — gh succeeds but
// pprod's satellites_info never reports the pushed SHA within the
// configured converge timeout. The runner returns failure + no
// kind:release-evidence row.
func TestRunMergeToMainHotPath_PprodConvergeTimeout(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	// Override the scripted commits with a stale-only sequence that
	// never matches the pushed SHA.
	s.pprodCommits = []string{"stale-only"}

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "pprod converge timeout")

	led := s.findAPICall("ledger_append")
	if led != nil {
		tagsAny, _ := led.Body["tags"].([]any)
		for _, v := range tagsAny {
			if v == "kind:release-evidence" {
				t.Errorf("release-evidence row appended on converge timeout: %+v", led)
			}
		}
	}
}

// TestVerifyChainPriorWorkSuccess: open work task on the chain
// fails the gate; a closed=failure work task superseded by a retry
// carrying prior_task_id passes; an orphaned closed=failure still
// trips the gate; review tasks are ignored.
func TestVerifyChainPriorWorkSuccess(t *testing.T) {
	// Open work task — gate fails.
	tw := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "published"},
		{ID: "task_merge", Action: "contract:merge_to_main", Kind: "work", Status: "claimed"},
	}}
	err := verifyChainPriorWorkSuccess(tw, "task_merge")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open work task")

	// AC3: [failed_merge → retry_merge(prior_task_id=failed_merge)] —
	// gate passes with retry as ignoreID. This is the documented
	// recovery primitive per pr_pipeline_authority.
	tw = taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_merge_1", Action: "contract:merge_to_main", Kind: "work", Status: "closed", Outcome: "failure"},
		{ID: "task_merge_2", Action: "contract:merge_to_main", Kind: "work", Status: "claimed", PriorTaskID: "task_merge_1"},
	}}
	err = verifyChainPriorWorkSuccess(tw, "task_merge_2")
	assert.NoError(t, err, "retry carrying prior_task_id to closed=failure predecessor must clear the gate")

	// Negative path: orphaned closed=failure work task with no
	// successor pointing at it via prior_task_id still trips the
	// gate. Protects against the helper degrading to a no-op.
	tw = taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev_failed", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "failure"},
		{ID: "task_merge", Action: "contract:merge_to_main", Kind: "work", Status: "claimed"},
	}}
	err = verifyChainPriorWorkSuccess(tw, "task_merge")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no retry successor")

	// Review tasks ignored — even a failed review on the chain
	// shouldn't block merge (reviewer's contract authors a retry).
	tw = taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_rev", Action: "contract:develop", Kind: "review", Status: "closed", Outcome: "failure"},
		{ID: "task_merge", Action: "contract:merge_to_main", Kind: "work", Status: "claimed"},
	}}
	err = verifyChainPriorWorkSuccess(tw, "task_merge")
	assert.NoError(t, err)
}

// TestInferDevelopTaskID_LatestSuccessWins: chain inference picks the
// most-recent successful develop iteration.
func TestInferDevelopTaskID_LatestSuccessWins(t *testing.T) {
	tw := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev_a", Action: "contract:develop", Kind: "work", Iteration: 1, Status: "closed", Outcome: "failure"},
		{ID: "task_dev_b", Action: "contract:develop", Kind: "work", Iteration: 2, Status: "closed", Outcome: "success"},
		{ID: "task_dev_c", Action: "contract:develop", Kind: "review", Status: "closed", Outcome: "success"},
	}}
	id, err := inferDevelopTaskID(tw)
	require.NoError(t, err)
	assert.Equal(t, "task_dev_b", id, "review tasks should not match")

	// No develop on chain → error.
	tw = taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_plan", Action: "contract:plan", Kind: "work", Status: "closed", Outcome: "success"},
	}}
	_, err = inferDevelopTaskID(tw)
	require.Error(t, err)
}

// TestResolveLocalBranch_AmbiguousFails: when more than one local
// branch matches the template-derived glob, the runner refuses to
// guess.
func TestResolveLocalBranch_AmbiguousFails(t *testing.T) {
	s := newHotpathStub(t)
	s.gitOutputs["branch --list agent-task_dev-from-*"] =
		"  agent-task_dev-from-aaaaaaa\n  agent-task_dev-from-bbbbbbb\n"

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	_, err := cc.resolveLocalBranch(context.Background(), "task_dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple matches")
}

// TestResolveLocalBranch_TemplateParameterised: the resolver derives
// its glob from cfg.BranchTemplate, so swapping templates (client- /
// agent- / dev- / no `from-` segment) must just work. Single source of
// truth for writer and reader — sty_2b24ac64.
func TestResolveLocalBranch_TemplateParameterised(t *testing.T) {
	cases := []struct {
		name     string
		template string
		taskID   string
		listKey  string // joined args fed to gitOutputs
		listOut  string // raw `git branch --list` stdout
		want     string
	}{
		{
			name:     "client_prefix_install_payload",
			template: "client-{task_id}-from-{base_sha}",
			taskID:   "task_997de08c",
			listKey:  "branch --list client-task_997de08c-from-*",
			listOut:  "  client-task_997de08c-from-833a28e\n",
			want:     "client-task_997de08c-from-833a28e",
		},
		{
			name:     "agent_prefix_legacy_compat",
			template: "agent-{task_id}-from-{base_sha}",
			taskID:   "task_dev",
			listKey:  "branch --list agent-task_dev-from-*",
			listOut:  "  agent-task_dev-from-833a28e\n",
			want:     "agent-task_dev-from-833a28e",
		},
		{
			name:     "dev_prefix_no_from_segment",
			template: "dev-{task_id}-{base_sha}",
			taskID:   "task_xyz",
			listKey:  "branch --list dev-task_xyz-*",
			listOut:  "* dev-task_xyz-deadbeef\n",
			want:     "dev-task_xyz-deadbeef",
		},
		{
			// Worktree-held: when the matched branch is checked out in
			// another worktree, `git branch --list` prefixes it with
			// `+ ` (vs `* ` for current). Dispatched develop tasks run
			// inside a worktree, so the repo-root resolver sees `+ `
			// every time. Iter-2's fix stripped only `* ` — this case
			// catches the gap surfaced in ldg_837cd8fa.
			name:     "client_prefix_worktree_held",
			template: "client-{task_id}-from-{base_sha}",
			taskID:   "task_dev",
			listKey:  "branch --list client-task_dev-from-*",
			listOut:  "+ client-task_dev-from-833a28e\n",
			want:     "client-task_dev-from-833a28e",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newHotpathStub(t)
			s.gitOutputs[tc.listKey] = tc.listOut
			cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: tc.template})
			got, err := cc.resolveLocalBranch(context.Background(), tc.taskID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveLocalBranch_EmptyTemplateErrors: an empty BranchTemplate
// is a misconfiguration we surface before calling git, not a fallback
// we tolerate. `git branch --list -from-*` would match every branch
// in the repo — that failure mode must never reach the gitRunner.
func TestResolveLocalBranch_EmptyTemplateErrors(t *testing.T) {
	s := newHotpathStub(t)
	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: ""})
	_, err := cc.resolveLocalBranch(context.Background(), "task_dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch_template empty")
	assert.Empty(t, s.gitCmds, "resolver must not call git when template is empty")
}

// TestHotPath_RoutesThroughAPIv1 asserts that every substrate call the
// hot runners make lands on /api/v1/<noun>/<verb> — no JSON-RPC
// tools/call request reaches the stub server.
func TestHotPath_RoutesThroughAPIv1(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_plan", Action: "contract:plan", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_commit", Action: "contract:commit", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))
	s.gitOutputs["branch --list agent-task_dev-from-*"] = "  agent-task_dev-from-833a28e\n"
	s.gitOutputs["log -1 --pretty=fuller agent-task_dev-from-833a28e"] = "commit feedface\n"
	s.gitOutputs["push origin agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"] =
		"To origin\n * [new branch]      agent-task_dev-from-833a28e -> agent-task_dev-from-833a28e\n"
	s.gitOutputs["ls-remote origin agent-task_dev-from-833a28e"] =
		"feedface\trefs/heads/agent-task_dev-from-833a28e\n"
	s.setAPIResp("ledger_append", `{"id":"ldg_v1"}`)
	s.setAPIResp("task_update", `{}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})

	ti := taskInfo{
		ID: "task_commit", StoryID: "sty_v1", ProjectID: "proj_x",
		Action: "contract:commit",
	}
	_, _ = cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_commit", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "commit"}, storyInfo{ID: "sty_v1"})

	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.apiCalls, "hot runner must issue at least one substrate call")
	for _, call := range s.apiCalls {
		assert.True(t, strings.HasPrefix(call.Path, "/api/v1/"),
			"every substrate request must route through /api/v1; got %s", call.Path)
	}
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
type hotpathStub struct {
	t          *testing.T
	gitCmds    [][]string
	gitOutputs map[string]string // joined args → stdout
	gitErrors  map[string]error  // joined args → error
	apiCalls   []recordedAPICall
	apiResp    map[string]string // path → response body (JSON)
	mu         sync.Mutex
	srv        *httptest.Server
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
// covers push / merge_to_main / story_close and returns
// errHotUnimplemented for anything else.
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

// TestRunPushHotPath_HappyPath drives the full push runner: trigger-
// supplied branch wins, gitRunner records the three expected commands
// (log, push, ls-remote — branch resolution short-circuits via
// trigger), and the ledger evidence row carries the shape-equivalent
// tag set on /api/v1/ledger/append.
func TestRunPushHotPath_HappyPath(t *testing.T) {
	s := newHotpathStub(t)
	s.gitOutputs["log -1 --pretty=fuller agent-task_dev-from-833a28e"] =
		"commit deadbeef\nAuthor: ...\n\n    refactor(auth): foo\n"
	s.gitOutputs["push origin agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"] =
		"To github.com:org/repo.git\n * [new branch]      agent-task_dev-from-833a28e -> agent-task_dev-from-833a28e\n"
	s.gitOutputs["ls-remote origin agent-task_dev-from-833a28e"] =
		"deadbeefcafe\trefs/heads/agent-task_dev-from-833a28e\n"
	s.setAPIResp("ledger_append", `{"id":"ldg_xxx"}`)
	s.setAPIResp("task_update", `{"task_id":"task_push","status":"closed","outcome":"success"}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})

	trigger, _ := json.Marshal(map[string]string{"branch": "agent-task_dev-from-833a28e"})
	ti := taskInfo{
		ID: "task_push", StoryID: "sty_target", ProjectID: "proj_x",
		Action: "contract:push", Trigger: trigger,
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_push", ProjectID: "proj_x"},
		ti, agentInfo{Name: "releaser_agent"},
		contractInfo{Name: "push"},
		storyInfo{ID: "sty_target"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)

	// gitRunner saw exactly three commands (log, push, ls-remote) —
	// branch resolution via trigger skipped the chain walk.
	require.Len(t, s.gitCmds, 3, "trigger override should skip branch-list git call")
	assert.Equal(t, []string{"DIR=/repo", "log", "-1", "--pretty=fuller", "agent-task_dev-from-833a28e"}, s.gitCmds[0])
	assert.Equal(t, []string{"DIR=/repo", "push", "origin", "agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"}, s.gitCmds[1])
	assert.Equal(t, []string{"DIR=/repo", "ls-remote", "origin", "agent-task_dev-from-833a28e"}, s.gitCmds[2])

	// Evidence row tags must match the heavy-path fixture shape.
	led := s.findAPICall("ledger_append")
	require.NotNil(t, led, "ledger_append must be called via /api/v1/ledger/append")
	tagsAny, _ := led.Body["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "task_id:task_push")
	assert.Contains(t, tags, "story_id:sty_target")
	assert.Contains(t, tags, "phase:push")
	assert.Contains(t, tags, "branch:agent-task_dev-from-833a28e")
	assert.Contains(t, tags, "kind:evidence")
	assert.Contains(t, tags, "dispatch_class:hot")

	// task_update issued at the end of the runner.
	require.NotNil(t, s.findAPICall("task_update"))
}

// TestRunPushHotPath_InferBranchFromChain: with no trigger, the
// runner walks the task chain, finds the latest contract:develop
// success, and resolves agent-<dev>-from-* via git branch --list.
func TestRunPushHotPath_InferBranchFromChain(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_plan", Action: "contract:plan", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_dev_a", Action: "contract:develop", Kind: "work", Iteration: 1, Status: "closed", Outcome: "failure"},
		{ID: "task_dev_b", Action: "contract:develop", Kind: "work", Iteration: 2, Status: "closed", Outcome: "success"},
		{ID: "task_dev_rev", Action: "contract:develop", Kind: "review", Status: "closed", Outcome: "success"},
		{ID: "task_push", Action: "contract:push", Kind: "work", Status: "claimed"},
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

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})

	ti := taskInfo{
		ID: "task_push", StoryID: "sty_x", ProjectID: "proj_x",
		Action: "contract:push",
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_push", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "push"}, storyInfo{ID: "sty_x"})
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

// TestRunMergeToMainHotPath drives the merge runner end-to-end.
func TestRunMergeToMainHotPath(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_plan", Action: "contract:plan", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_push", Action: "contract:push", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_merge", Action: "contract:merge_to_main", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))
	s.gitOutputs["fetch origin --quiet"] = ""
	s.gitOutputs["rev-parse main"] = "abc123\n"
	s.gitOutputs["merge-base --is-ancestor main agent-task_dev-from-833a28e"] = ""
	s.gitOutputs["merge --ff-only agent-task_dev-from-833a28e"] =
		"Updating abc123..deadbeef\nFast-forward\n internal/auth/middleware.go | 10 ++++++++++\n"
	// Second rev-parse main returns the post-merge SHA.
	s.gitOutputs["rev-parse main"] = "deadbeef\n"
	s.setAPIResp("ledger_append", `{"id":"ldg_m"}`)
	s.setAPIResp("task_update", `{}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})

	trigger, _ := json.Marshal(map[string]string{"branch": "agent-task_dev-from-833a28e"})
	ti := taskInfo{
		ID: "task_merge", StoryID: "sty_m", ProjectID: "proj_x",
		Action: "contract:merge_to_main", Trigger: trigger,
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_merge", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "merge_to_main"}, storyInfo{ID: "sty_m"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)

	// gitRunner sequence: fetch, rev-parse, ancestor, merge, rev-parse.
	expectedHead := []string{
		"fetch", "rev-parse", "merge-base", "merge", "rev-parse",
	}
	require.GreaterOrEqual(t, len(s.gitCmds), len(expectedHead))
	for i, want := range expectedHead {
		assert.Equal(t, want, s.gitCmds[i][1], "git command #%d should be %q", i, want)
	}

	led := s.findAPICall("ledger_append")
	require.NotNil(t, led)
	tagsAny, _ := led.Body["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "phase:merge_to_main")
	assert.Contains(t, tags, "dispatch_class:hot")
}

// TestRunStoryCloseHotPath_RefusesEmptyRequiredFields fails the runner
// when a template-required field is unpopulated. The error mentions
// the missing field name so the orchestrator knows what to set.
func TestRunStoryCloseHotPath_RefusesEmptyRequiredFields(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_close", Action: "contract:story_close", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))

	story := storyRaw{
		Story: storyRawStory{
			ID: "sty_close", Title: "test", Status: "in_progress",
			Fields: map[string]any{"scope": "auth"}, // rollout_plan absent
		},
		Template: storyTemplate{
			Category: "infrastructure",
			Fields: []storyTemplateField{
				{Name: "scope", Required: true},
				{Name: "rollout_plan", Required: false},
			},
			Hooks: map[string]storyHookSet{
				"done": {Structured: []storyHookCheck{
					{Type: "field_present", Field: "rollout_plan"},
					{Type: "field_present", Field: "fix_commit"},
				}},
			},
		},
	}
	storyBytes, _ := json.Marshal(story)
	s.setAPIResp("story_get", string(storyBytes))

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})

	ti := taskInfo{
		ID: "task_close", StoryID: "sty_close", ProjectID: "proj_x",
		Action: "contract:story_close",
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_close", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "story_close"}, storyInfo{ID: "sty_close"})
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "rollout_plan")
	assert.Contains(t, err.Error(), "fix_commit")
	assert.Contains(t, err.Error(), "story_update")
}

// TestRunStoryCloseHotPath_HappyPath: every required field populated,
// chain consistent → ledger row + task_update.
func TestRunStoryCloseHotPath_HappyPath(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_close", Action: "contract:story_close", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))

	story := storyRaw{
		Story: storyRawStory{
			ID: "sty_z", Title: "demo story", Status: "in_progress",
			Fields: map[string]any{
				"scope":                "internal/agent/worker/",
				"rollout_plan":         "single deploy, rollback = revert",
				"fix_commit":           "deadbeef",
				"regression_test_path": "internal/agent/worker/hotpath_test.go",
				"post_deploy_check":    "operator runs story through hot path",
			},
		},
		Template: storyTemplate{
			Category: "infrastructure",
			Fields: []storyTemplateField{
				{Name: "scope", Required: true},
				{Name: "rollout_plan", Required: false},
				{Name: "fix_commit", Required: false},
				{Name: "regression_test_path", Required: false},
				{Name: "post_deploy_check", Required: false},
			},
			Hooks: map[string]storyHookSet{
				"done": {Structured: []storyHookCheck{
					{Type: "field_present", Field: "rollout_plan"},
					{Type: "field_present", Field: "fix_commit"},
					{Type: "field_present", Field: "regression_test_path"},
					{Type: "field_present", Field: "post_deploy_check"},
				}},
			},
		},
	}
	storyBytes, _ := json.Marshal(story)
	s.setAPIResp("story_get", string(storyBytes))
	s.setAPIResp("ledger_append", `{"id":"ldg_c"}`)
	s.setAPIResp("task_update", `{}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})

	trigger, _ := json.Marshal(map[string]string{"resolution": "delivered"})
	ti := taskInfo{
		ID: "task_close", StoryID: "sty_z", ProjectID: "proj_x",
		Action: "contract:story_close", Trigger: trigger,
	}
	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_close", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "story_close"}, storyInfo{ID: "sty_z"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)

	led := s.findAPICall("ledger_append")
	require.NotNil(t, led)
	tagsAny, _ := led.Body["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "phase:story_close")
	assert.Contains(t, tags, "resolution:delivered")
	assert.Contains(t, tags, "dispatch_class:hot")
	content, _ := led.Body["content"].(string)
	assert.Contains(t, content, "internal/agent/worker/")
	assert.Contains(t, content, "deadbeef")
	assert.Contains(t, content, "Chain summary")
}

// TestRequiredClosingFieldsMissing_HooksTakePrecedence: when both
// `template.fields[*].required` and `hooks.done.structured` exist,
// the hook list is authoritative.
func TestRequiredClosingFieldsMissing_HooksTakePrecedence(t *testing.T) {
	sr := storyRaw{
		Story: storyRawStory{Fields: map[string]any{"scope": "x"}}, // only scope populated
		Template: storyTemplate{
			Fields: []storyTemplateField{
				{Name: "scope", Required: true}, // template says required
			},
			Hooks: map[string]storyHookSet{
				"done": {Structured: []storyHookCheck{
					{Type: "field_present", Field: "rollout_plan"}, // hook says rollout_plan
				}},
			},
		},
	}
	missing := requiredClosingFieldsMissing(sr)
	require.Len(t, missing, 1)
	assert.Equal(t, "rollout_plan", missing[0],
		"hook structured list should override required:true precedence")
}

// TestVerifyChainPriorWorkSuccess: open work task on the chain
// fails the gate; superseded iter-1 failure with iter-2 success
// passes; review tasks are ignored.
func TestVerifyChainPriorWorkSuccess(t *testing.T) {
	// Open work task — gate fails.
	tw := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "published"},
		{ID: "task_close", Action: "contract:story_close", Kind: "work", Status: "claimed"},
	}}
	err := verifyChainPriorWorkSuccess(tw, "task_close")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open work task")

	// iter-1 failure superseded by iter-2 success — gate passes.
	tw = taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev_1", Action: "contract:develop", Kind: "work", Iteration: 1, Status: "closed", Outcome: "failure"},
		{ID: "task_dev_2", Action: "contract:develop", Kind: "work", Iteration: 2, Status: "closed", Outcome: "success"},
		{ID: "task_close", Action: "contract:story_close", Kind: "work", Status: "claimed"},
	}}
	err = verifyChainPriorWorkSuccess(tw, "task_close")
	assert.NoError(t, err)

	// Review tasks ignored — even a failed review on the chain
	// shouldn't block close (reviewer's contract authors a retry).
	tw = taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_rev", Action: "contract:develop", Kind: "review", Status: "closed", Outcome: "failure"},
		{ID: "task_close", Action: "contract:story_close", Kind: "work", Status: "claimed"},
	}}
	err = verifyChainPriorWorkSuccess(tw, "task_close")
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
// branch matches the agent-<dev>-from-* glob, the runner refuses to
// guess.
func TestResolveLocalBranch_AmbiguousFails(t *testing.T) {
	s := newHotpathStub(t)
	s.gitOutputs["branch --list agent-task_dev-from-*"] =
		"  agent-task_dev-from-aaaaaaa\n  agent-task_dev-from-bbbbbbb\n"

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})
	_, err := cc.resolveLocalBranch(context.Background(), "task_dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple matches")
}

// TestHotPath_RoutesThroughAPIv1 asserts that every substrate call the
// hot runners make lands on /api/v1/<noun>/<verb> — no JSON-RPC
// tools/call request reaches the stub server.
func TestHotPath_RoutesThroughAPIv1(t *testing.T) {
	s := newHotpathStub(t)
	walk := taskWalkResponse{Tasks: []taskWalkTask{
		{ID: "task_dev", Action: "contract:develop", Kind: "work", Status: "closed", Outcome: "success"},
		{ID: "task_close", Action: "contract:story_close", Kind: "work", Status: "claimed"},
	}}
	walkBytes, _ := json.Marshal(walk)
	s.setAPIResp("task_walk", string(walkBytes))
	story := storyRaw{
		Story: storyRawStory{
			ID: "sty_v1", Title: "api-v1 routing", Status: "in_progress",
			Fields: map[string]any{"scope": "x"},
		},
		Template: storyTemplate{
			Category: "infrastructure",
			Fields:   []storyTemplateField{{Name: "scope", Required: true}},
		},
	}
	storyBytes, _ := json.Marshal(story)
	s.setAPIResp("story_get", string(storyBytes))
	s.setAPIResp("ledger_append", `{"id":"ldg_v1"}`)
	s.setAPIResp("task_update", `{}`)

	cc := s.client(config.AgentConfig{RepoPath: "/repo"})

	ti := taskInfo{
		ID: "task_close", StoryID: "sty_v1", ProjectID: "proj_x",
		Action: "contract:story_close",
	}
	_, _ = cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_close", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "story_close"}, storyInfo{ID: "sty_v1"})

	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.apiCalls, "hot runner must issue at least one substrate call")
	for _, call := range s.apiCalls {
		assert.True(t, strings.HasPrefix(call.Path, "/api/v1/"),
			"every substrate request must route through /api/v1; got %s", call.Path)
	}
}

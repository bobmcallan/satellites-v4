package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
)

// Tests live in the worker package (not worker_test) because they
// exercise unexported helpers — runHotPath, the per-contract runners,
// requiredClosingFieldsMissing, etc.

// hotpathStub composes a *claudeClient backed by mock callTool +
// gitRunner channels suitable for asserting against the recorded
// command stream + ledger row contents.
type hotpathStub struct {
	t          *testing.T
	gitCmds    [][]string
	gitOutputs map[string]string // joined args → stdout
	gitErrors  map[string]error  // joined args → error
	mcpCalls   []recordedMCPCall
	mcpResp    map[string]string // tool name → response text
	mcpErr     map[string]error  // tool name → error
	mu         sync.Mutex
}

type recordedMCPCall struct {
	Tool string
	Args map[string]any
}

func newHotpathStub(t *testing.T) *hotpathStub {
	return &hotpathStub{
		t:          t,
		gitOutputs: map[string]string{},
		gitErrors:  map[string]error{},
		mcpResp:    map[string]string{},
		mcpErr:     map[string]error{},
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

// patchCallTool replaces (*claudeClient).callTool by overriding the
// underlying http.Do. Easier: build a real httptest mcp stub.

// We attach the stub to a *claudeClient via the placeholderClient's
// http field. Lighter-weight: wrap the placeholderClient's http with
// an in-process http.RoundTripper that records the request.
func (s *hotpathStub) client(cfg config.AgentConfig) *claudeClient {
	pc := newPlaceholderClient(cfg, nil)
	pc.http.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(s.t, err)
		var parsed struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
			ID int64 `json:"id"`
		}
		require.NoError(s.t, json.Unmarshal(body, &parsed))
		s.mu.Lock()
		s.mcpCalls = append(s.mcpCalls, recordedMCPCall{
			Tool: parsed.Params.Name,
			Args: parsed.Params.Arguments,
		})
		s.mu.Unlock()
		if err := s.mcpErr[parsed.Params.Name]; err != nil {
			return nil, err
		}
		text := s.mcpResp[parsed.Params.Name]
		respBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      parsed.ID,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(respBody))),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	cc := &claudeClient{
		placeholderClient: pc,
		gitRunner:         s.gitRunner,
		findHome:          func() (string, error) { return "/dev/null/home", nil },
	}
	return cc
}

// findMCPCall returns the first recorded call to the named tool, or
// nil when absent.
func (s *hotpathStub) findMCPCall(tool string) *recordedMCPCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.mcpCalls {
		if s.mcpCalls[i].Tool == tool {
			return &s.mcpCalls[i]
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
	cc := s.client(config.AgentConfig{MCPURL: "http://mock"})
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
// supplied branch wins, gitRunner records the four expected commands
// (log, push, ls-remote — branch resolution short-circuits via
// trigger), and the ledger evidence row carries the shape-equivalent
// tag set.
func TestRunPushHotPath_HappyPath(t *testing.T) {
	s := newHotpathStub(t)
	s.gitOutputs["log -1 --pretty=fuller agent-task_dev-from-833a28e"] =
		"commit deadbeef\nAuthor: ...\n\n    refactor(auth): foo\n"
	s.gitOutputs["push origin agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"] =
		"To github.com:org/repo.git\n * [new branch]      agent-task_dev-from-833a28e -> agent-task_dev-from-833a28e\n"
	s.gitOutputs["ls-remote origin agent-task_dev-from-833a28e"] =
		"deadbeefcafe\trefs/heads/agent-task_dev-from-833a28e\n"
	s.mcpResp["ledger_append"] = `{"id":"ldg_xxx"}`
	s.mcpResp["task_update"] = `{"id":"task_push","status":"closed"}`

	cc := s.client(config.AgentConfig{MCPURL: "http://mock", RepoPath: "/repo"})

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
	led := s.findMCPCall("ledger_append")
	require.NotNil(t, led, "ledger_append must be called")
	tagsAny, _ := led.Args["tags"].([]any)
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
	require.NotNil(t, s.findMCPCall("task_update"))
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
	s.mcpResp["task_walk"] = string(walkBytes)
	s.gitOutputs["branch --list agent-task_dev_b-from-*"] = "  agent-task_dev_b-from-833a28e\n"
	s.gitOutputs["log -1 --pretty=fuller agent-task_dev_b-from-833a28e"] = "commit feedface\n"
	s.gitOutputs["push origin agent-task_dev_b-from-833a28e:agent-task_dev_b-from-833a28e"] =
		"To origin\n * [new branch]      agent-task_dev_b-from-833a28e -> agent-task_dev_b-from-833a28e\n"
	s.gitOutputs["ls-remote origin agent-task_dev_b-from-833a28e"] =
		"feedface\trefs/heads/agent-task_dev_b-from-833a28e\n"
	s.mcpResp["ledger_append"] = `{"id":"ldg_y"}`
	s.mcpResp["task_update"] = `{}`

	cc := s.client(config.AgentConfig{MCPURL: "http://mock", RepoPath: "/repo"})

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
	s.mcpResp["task_walk"] = string(walkBytes)
	s.gitOutputs["fetch origin --quiet"] = ""
	s.gitOutputs["rev-parse main"] = "abc123\n"
	s.gitOutputs["merge-base --is-ancestor main agent-task_dev-from-833a28e"] = ""
	s.gitOutputs["merge --ff-only agent-task_dev-from-833a28e"] =
		"Updating abc123..deadbeef\nFast-forward\n internal/auth/middleware.go | 10 ++++++++++\n"
	// Second rev-parse main returns the post-merge SHA.
	s.gitOutputs["rev-parse main"] = "deadbeef\n"
	s.mcpResp["ledger_append"] = `{"id":"ldg_m"}`
	s.mcpResp["task_update"] = `{}`

	cc := s.client(config.AgentConfig{MCPURL: "http://mock", RepoPath: "/repo"})

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

	led := s.findMCPCall("ledger_append")
	require.NotNil(t, led)
	tagsAny, _ := led.Args["tags"].([]any)
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
	s.mcpResp["task_walk"] = string(walkBytes)

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
	s.mcpResp["story_get"] = string(storyBytes)

	cc := s.client(config.AgentConfig{MCPURL: "http://mock", RepoPath: "/repo"})

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
	assert.Contains(t, err.Error(), "story_field_set")
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
	s.mcpResp["task_walk"] = string(walkBytes)

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
	s.mcpResp["story_get"] = string(storyBytes)
	s.mcpResp["ledger_append"] = `{"id":"ldg_c"}`
	s.mcpResp["task_update"] = `{}`

	cc := s.client(config.AgentConfig{MCPURL: "http://mock", RepoPath: "/repo"})

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

	led := s.findMCPCall("ledger_append")
	require.NotNil(t, led)
	tagsAny, _ := led.Args["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "phase:story_close")
	assert.Contains(t, tags, "resolution:delivered")
	assert.Contains(t, tags, "dispatch_class:hot")
	content, _ := led.Args["content"].(string)
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

	cc := s.client(config.AgentConfig{MCPURL: "http://mock", RepoPath: "/repo"})
	_, err := cc.resolveLocalBranch(context.Background(), "task_dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple matches")
}

// roundTripperFunc adapts a closure to http.RoundTripper for the
// inline MCP stub.
type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }


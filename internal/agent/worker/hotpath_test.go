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
	// Empty-push gate (sty_85b9ec3e): HEAD distinct from base lets the
	// runner through to log/push/ls-remote.
	s.gitOutputs["rev-parse agent-task_dev-from-833a28e"] = "feedfacecafe1234\n"
	s.gitOutputs["rev-parse 833a28e"] = "833a28edeadbeef0000\n"
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

	// gitRunner sequence: rev-parse <branch>, rev-parse <baseSHA>, log,
	// push, ls-remote. Branch resolution via trigger skipped the
	// chain walk.
	require.Len(t, s.gitCmds, 5, "trigger override should skip branch-list git call")
	assert.Equal(t, []string{"DIR=/repo", "rev-parse", "agent-task_dev-from-833a28e"}, s.gitCmds[0])
	assert.Equal(t, []string{"DIR=/repo", "rev-parse", "833a28e"}, s.gitCmds[1])
	assert.Equal(t, []string{"DIR=/repo", "log", "-1", "--pretty=fuller", "agent-task_dev-from-833a28e"}, s.gitCmds[2])
	assert.Equal(t, []string{"DIR=/repo", "push", "origin", "agent-task_dev-from-833a28e:agent-task_dev-from-833a28e"}, s.gitCmds[3])
	assert.Equal(t, []string{"DIR=/repo", "ls-remote", "origin", "agent-task_dev-from-833a28e"}, s.gitCmds[4])

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
	// Empty-push gate (sty_85b9ec3e): HEAD distinct from base.
	s.gitOutputs["rev-parse agent-task_dev_b-from-833a28e"] = "feedface00112233\n"
	s.gitOutputs["rev-parse 833a28e"] = "833a28edeadbeef00\n"
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

// TestRunCommitHotPath_EmptyPushFails (sty_85b9ec3e AC1) — when the
// work branch HEAD resolves to the same SHA as the rendered
// `{base_sha}` suffix, the runner trips the empty-push gate BEFORE
// running git log / git push / git ls-remote. No ledger row is
// written, no task_update is issued. The returned error carries the
// literal AC1 substrings: `commit: work branch has no new commits
// relative to base`, `HEAD=<sha>`, `base=<sha>`.
func TestRunCommitHotPath_EmptyPushFails(t *testing.T) {
	s := newHotpathStub(t)
	// Both rev-parse calls return the same SHA — the gate must trip.
	s.gitOutputs["rev-parse client-task_dev-from-001b1591"] = "001b1591aa3f7abd6b973c8fad57471fa0078085\n"
	s.gitOutputs["rev-parse 001b1591"] = "001b1591aa3f7abd6b973c8fad57471fa0078085\n"
	// Intentionally NOT scripting log/push/ls-remote outputs: if the
	// runner reaches those calls the gate has failed open and the test
	// surfaces it as a gitOutputs miss (default empty bytes, no error
	// — so we additionally assert on the recorded gitCmds shape).

	cc := s.client(config.AgentConfig{
		RepoPath:       "/repo",
		BranchTemplate: "client-{task_id}-from-{base_sha}",
	})

	trigger, _ := json.Marshal(map[string]string{
		"branch": "client-task_dev-from-001b1591",
	})
	ti := taskInfo{
		ID: "task_commit", StoryID: "sty_x", ProjectID: "proj_x",
		Action: "contract:commit", Trigger: trigger,
	}

	outcome, err := cc.runHotPath(context.Background(),
		TaskEnvelope{ID: "task_commit", ProjectID: "proj_x"},
		ti, agentInfo{}, contractInfo{Name: "commit"}, storyInfo{ID: "sty_x"})

	assert.Equal(t, OutcomeFailure, outcome)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"commit: work branch has no new commits relative to base",
		"error must name the substrate-level empty-push failure")
	assert.Contains(t, err.Error(), "HEAD=", "error must embed HEAD= literal")
	assert.Contains(t, err.Error(), "base=", "error must embed base= literal")

	// Gate must short-circuit BEFORE push / ls-remote.
	for _, cmd := range s.gitCmds {
		joined := strings.Join(cmd, " ")
		assert.NotContains(t, joined, "push",
			"gate must short-circuit before git push")
		assert.NotContains(t, joined, "ls-remote",
			"gate must short-circuit before git ls-remote")
	}
	// Evidence row + task close MUST NOT run on a tripped gate.
	assert.Nil(t, s.findAPICall("ledger_append"),
		"gate must short-circuit before kind:evidence is appended")
	assert.Nil(t, s.findAPICall("task_update"),
		"gate must short-circuit before task_update is issued")
}

// TestParseBaseSHAFromBranch (sty_85b9ec3e) — the helper that mirrors
// renderBranchName / branchGlob on cfg.BranchTemplate. Extracts the
// `{base_sha}` suffix substring from a rendered branch given the
// original template. Errors when the template lacks the placeholder
// (the commit hot-path's empty-push gate surfaces that as a clear
// failure rather than silently skipping the check).
func TestParseBaseSHAFromBranch(t *testing.T) {
	cases := []struct {
		name     string
		template string
		branch   string
		want     string
		wantErr  string
	}{
		{
			name:     "canonical_client_prefix",
			template: "client-{task_id}-from-{base_sha}",
			branch:   "client-task_5a2d08f3-from-7ebe51f",
			want:     "7ebe51f",
		},
		{
			name:     "legacy_agent_prefix",
			template: "agent-{task_id}-from-{base_sha}",
			branch:   "agent-task_dev-from-833a28e",
			want:     "833a28e",
		},
		{
			name:     "no_from_segment",
			template: "dev-{task_id}-{base_sha}",
			branch:   "dev-task_xyz-deadbeef",
			want:     "deadbeef",
		},
		{
			name:     "base_sha_only_template",
			template: "{base_sha}",
			branch:   "1234abc",
			want:     "1234abc",
		},
		{
			name:     "template_missing_base_sha_placeholder",
			template: "static-branch",
			branch:   "static-branch",
			wantErr:  "no {base_sha} placeholder",
		},
		{
			name:     "branch_does_not_match_template",
			template: "client-{task_id}-from-{base_sha}",
			branch:   "completely-different",
			wantErr:  "does not match template",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBaseSHAFromBranch(tc.template, tc.branch)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// defaultPreMergeHEAD is the synthetic pre-merge HEAD the happy-path
// fixture returns from `git rev-parse HEAD`. Tests asserting AC3
// rollback (`git reset --hard <preMergeHEAD>`) check that this exact
// SHA appears in the captured gitCmds.
const defaultPreMergeHEAD = "premerge1234"

// seedMergeToMainHappyStubs primes the gitOutputs + ghOutputs + pprod
// converge sequence + walk response for a PASS run of the extended
// merge_to_main release runner. The pushedSHA is the SHA the final
// `rev-parse main` returns AND the converge poll expects to observe.
//
// sty_63541aed AC3: also seeds the refuse-on-dirty status check
// (clean working tree) and the pre-merge HEAD capture so the runner's
// new top-of-function gates pass on the happy path.
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
	// AC3 refuse-on-dirty: clean tree.
	s.gitOutputs["status --porcelain"] = ""
	// AC3 rollback anchor: pre-merge HEAD capture.
	s.gitOutputs["rev-parse HEAD"] = defaultPreMergeHEAD + "\n"
	// AC3 rollback target stays a no-op on the happy path.
	s.gitOutputs["reset --hard "+defaultPreMergeHEAD] = ""
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

// findAllAPICalls returns all recorded calls to the given /api/v1
// path. Tests assert post-push paths emit two task_update calls (one
// at runHotPath entry from the orchestrator-side test harness is not
// modelled here — the helper just returns whatever was recorded).
func (s *hotpathStub) findAllAPICalls(toolName string) []recordedAPICall {
	target := "/api/v1" + cliremote.ToolNameToPath(toolName)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedAPICall
	for i := range s.apiCalls {
		if s.apiCalls[i].Path == target {
			out = append(out, s.apiCalls[i])
		}
	}
	return out
}

// findLedgerAppendWithTag returns the first recorded ledger_append
// whose tags include the literal tag string, or nil.
func (s *hotpathStub) findLedgerAppendWithTag(tag string) *recordedAPICall {
	target := "/api/v1" + cliremote.ToolNameToPath("ledger_append")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.apiCalls {
		call := s.apiCalls[i]
		if call.Path != target {
			continue
		}
		tagsAny, _ := call.Body["tags"].([]any)
		for _, v := range tagsAny {
			if s, ok := v.(string); ok && s == tag {
				return &call
			}
		}
	}
	return nil
}

// gitCmdMatches returns true when any captured git command in
// s.gitCmds equals the literal expected argv (after the DIR prefix).
func (s *hotpathStub) gitCmdMatches(expected ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cmd := range s.gitCmds {
		if len(cmd) != len(expected)+1 {
			continue
		}
		ok := true
		for i, want := range expected {
			if cmd[i+1] != want {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// gitCmdSeen returns true when any captured git command's first
// argument equals the literal verb.
func (s *hotpathStub) gitCmdSeen(verb string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cmd := range s.gitCmds {
		if len(cmd) >= 2 && cmd[1] == verb {
			return true
		}
	}
	return false
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

	// gitRunner sequence covers the AC3 refuse-on-dirty + rollback-
	// anchor preamble (status, rev-parse HEAD) and the extended
	// release: fetch, rev-parse, ancestor, merge, rev-parse, push-main.
	expectedHead := []string{"status", "rev-parse", "fetch", "rev-parse", "merge-base", "merge", "rev-parse", "push"}
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

// assertTaskClosedFailure verifies that a task_update API call was
// made with status=closed and outcome=failure. Per sty_63541aed AC1
// the runner must persist this BEFORE returning so the substrate's
// view of the chain matches the CLI's exit code.
func assertTaskClosedFailure(t *testing.T, s *hotpathStub) *recordedAPICall {
	t.Helper()
	for _, call := range s.findAllAPICalls("task_update") {
		if call.Body["status"] == "closed" && call.Body["outcome"] == "failure" {
			return &call
		}
	}
	t.Fatalf("no task_update(closed,failure) recorded; saw %+v", s.findAllAPICalls("task_update"))
	return nil
}

// TestRunMergeToMainHotPath_GHTimeout_PersistsClose — sty_63541aed
// AC1. gh run watch hangs past the configured timeout (post-push
// step). The runner MUST: (1) call task_update(closed,failure)
// carrying the failure reason BEFORE returning, (2) append a
// `kind:release-evidence` row tagged `release:pushed_unverified` so
// the orchestrator can recover via chain-shape inspection.
func TestRunMergeToMainHotPath_GHTimeout_PersistsClose(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	s.ghOutputs["run watch"] = ""
	s.ghErrors["run watch"] = context.DeadlineExceeded

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "gh run watch")

	// AC1: substrate view matches CLI exit. task_update persisted
	// with outcome=failure and the reason on the wire body.
	taskUpd := assertTaskClosedFailure(t, s)
	if reason, ok := taskUpd.Body["reason"].(string); !ok || reason == "" {
		t.Errorf("task_update body should carry failure reason; got %+v", taskUpd.Body)
	} else {
		assert.Contains(t, reason, "gh run watch")
	}

	// AC1: pushed-but-unverified release-evidence row appended.
	led := s.findLedgerAppendWithTag("release:pushed_unverified")
	require.NotNil(t, led, "release:pushed_unverified evidence row missing; saw %+v", s.findAllAPICalls("ledger_append"))
	tagsAny, _ := led.Body["tags"].([]any)
	tags := make([]string, len(tagsAny))
	for i, v := range tagsAny {
		tags[i] = v.(string)
	}
	assert.Contains(t, tags, "kind:release-evidence")
	assert.Contains(t, tags, "pushed_sha:deadbeef")
}

// TestRunMergeToMainHotPath_GHFailure_PersistsClose — sty_63541aed
// AC1. gh run watch exits non-zero (conclusion=failure). Identical
// shape to the timeout case: persisted close + pushed_unverified
// release-evidence so the orchestrator can recover.
func TestRunMergeToMainHotPath_GHFailure_PersistsClose(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	s.ghOutputs["run watch"] = "✗ release deploy · failed\n12345 · failure\n"
	s.ghErrors["run watch"] = errors.New("exit status 1")

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "gh run watch")

	assertTaskClosedFailure(t, s)
	require.NotNil(t, s.findLedgerAppendWithTag("release:pushed_unverified"),
		"release:pushed_unverified evidence row missing on gh failure")
}

// TestRunMergeToMainHotPath_PprodConvergeTimeout_PersistsClose —
// sty_63541aed AC1. The pprod converge poll times out after the
// push has shipped. The runner persists close+failure and writes
// the pushed-but-unverified release-evidence row.
func TestRunMergeToMainHotPath_PprodConvergeTimeout_PersistsClose(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	s.pprodCommits = []string{"stale-only"}

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "pprod converge timeout")

	taskUpd := assertTaskClosedFailure(t, s)
	if reason, ok := taskUpd.Body["reason"].(string); !ok || reason == "" {
		t.Errorf("task_update body should carry failure reason; got %+v", taskUpd.Body)
	} else {
		assert.Contains(t, reason, "pprod converge timeout")
	}

	led := s.findLedgerAppendWithTag("release:pushed_unverified")
	require.NotNil(t, led, "release:pushed_unverified evidence row missing on converge timeout")
}

// TestRunMergeToMainHotPath_DirtyWorkingTree_Refuses — sty_63541aed
// AC3. When `git status --porcelain` returns a non-empty body the
// runner refuses to merge: no merge, no push, no gh-watch, no pprod
// poll. task_update is still persisted with outcome=failure so the
// substrate's view of the chain reflects the abort.
func TestRunMergeToMainHotPath_DirtyWorkingTree_Refuses(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	// Override the clean-tree default with a dirty body — uncommitted
	// modifications the runner is not licensed to discard.
	s.gitOutputs["status --porcelain"] = " M .gitignore\n?? scratch.tmp\n"

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "dirty working tree")
	assert.Contains(t, err.Error(), ".gitignore",
		"refusal reason should include the dirty paths so the operator can reconcile")

	// AC3: no mutating git verb ran.
	for _, forbidden := range []string{"merge", "push", "fetch", "merge-base"} {
		if s.gitCmdSeen(forbidden) {
			t.Errorf("runner ran %q after dirty-tree refuse: gitCmds=%v", forbidden, s.gitCmds)
		}
	}

	// AC1: task closed failure.
	assertTaskClosedFailure(t, s)

	// No release-evidence row (nothing was pushed).
	require.Nil(t, s.findLedgerAppendWithTag("release:pushed_unverified"),
		"release-evidence must not be appended on refuse-on-dirty (nothing shipped)")
	require.Nil(t, s.findLedgerAppendWithTag("kind:release-evidence"),
		"release-evidence must not be appended on refuse-on-dirty (nothing shipped)")
}

// TestRunMergeToMainHotPath_PostPushFailure_RollsBack — sty_63541aed
// AC3. After the push has shipped, a post-push failure (here:
// pprod converge timeout) MUST trigger
// `git reset --hard <preMergeHEAD>` so the host repo's local main
// pointer is restored to the SHA the runner captured before
// merging. Combined with the persisted-close + pushed-unverified
// evidence row, this is the full AC1+AC3 recovery shape.
func TestRunMergeToMainHotPath_PostPushFailure_RollsBack(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	// Force a post-push failure: pprod never converges.
	s.pprodCommits = []string{"stale-only"}

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)

	// AC3: rollback issued against the captured pre-merge HEAD.
	require.True(t, s.gitCmdMatches("reset", "--hard", defaultPreMergeHEAD),
		"expected `git reset --hard %s` after post-push failure; got gitCmds=%v",
		defaultPreMergeHEAD, s.gitCmds)

	// AC1: task closed failure AND pushed_unverified evidence row.
	assertTaskClosedFailure(t, s)
	require.NotNil(t, s.findLedgerAppendWithTag("release:pushed_unverified"),
		"post-push failure must append the pushed_unverified release-evidence row")
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
	// Empty-push gate (sty_85b9ec3e): HEAD distinct from base.
	s.gitOutputs["rev-parse agent-task_dev-from-833a28e"] = "feedface00112233\n"
	s.gitOutputs["rev-parse 833a28e"] = "833a28edeadbeef00\n"
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

// TestPollPprodConverge_AcceptsShortSHA — sty_1e2f7ae7 AC1. pprod's
// satellites_info returns the running commit truncated to the 8-char
// short form. The converge poll must accept the short SHA when it is
// a non-empty prefix of the pushed full SHA (>= 7 chars, git's
// collision-safe floor) on the FIRST poll without retry or timeout.
func TestPollPprodConverge_AcceptsShortSHA(t *testing.T) {
	s := newHotpathStub(t)
	pushedFull := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	s.pprodCommits = []string{pushedFull[:8]} // "ae12fe78"
	cc := s.client(config.AgentConfig{})

	samples, err := cc.pollPprodConverge(context.Background(), pushedFull)
	require.NoError(t, err)
	require.Len(t, samples, 1, "converge must accept short SHA on first poll")
	assert.Equal(t, pushedFull[:8], samples[0].Commit)
	assert.Equal(t, 1, s.pprodCalls,
		"converge must accept the short SHA on the first poll, no retry")
}

// TestPollPprodConverge_RejectsEmpty — sty_1e2f7ae7 AC1 guard. A
// pprod that returns an empty commit string MUST NOT satisfy
// convergence (HasPrefix(pushedSHA, "") is true; the empty-string
// guard prevents that false positive). The poll must loop until
// timeout and the error must mention the empty last sample.
func TestPollPprodConverge_RejectsEmpty(t *testing.T) {
	s := newHotpathStub(t)
	pushedFull := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	s.pprodCommits = []string{"", "", ""}
	cc := s.client(config.AgentConfig{})
	cc.pprodPollInterval = time.Millisecond
	cc.pprodConvergeTimeout = 5 * time.Millisecond

	_, err := cc.pollPprodConverge(context.Background(), pushedFull)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pprod converge timeout")
	assert.Contains(t, err.Error(), `last=""`,
		"timeout error must report the last observed (empty) sample")
}

// TestPollPprodConverge_RejectsTooShortPrefix — sty_1e2f7ae7 AC1
// guard. A prefix shorter than 7 chars is collision-prone and MUST
// be rejected even when it is a valid prefix of the pushed SHA.
func TestPollPprodConverge_RejectsTooShortPrefix(t *testing.T) {
	s := newHotpathStub(t)
	pushedFull := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	s.pprodCommits = []string{pushedFull[:6]} // 6 chars, below floor
	cc := s.client(config.AgentConfig{})
	cc.pprodPollInterval = time.Millisecond
	cc.pprodConvergeTimeout = 5 * time.Millisecond

	_, err := cc.pollPprodConverge(context.Background(), pushedFull)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pprod converge timeout")
}

// TestFetchPprodCommit_DecodesLiveShape — sty_7a61ae53 AC2. Locks in
// the JSON-decoder contract introduced by sty_0be97c3e: the
// /api/v1/satellites/info response is the full SatellitesInfoOutput
// envelope ({server, caller, recent_activity}), and the worker's
// satellitesInfoResp struct decodes server.commit out of it. The
// existing TestPollPprodConverge_* family injects pprodInfoFetcher
// and bypasses the decoder entirely, so it could not have caught the
// stale-binary shape-drift that motivated this story. This test
// exercises fetchPprodCommit end-to-end through the real
// internal/cliremote.Client against an httptest.Server returning the
// canonical wire shape.
func TestFetchPprodCommit_DecodesLiveShape(t *testing.T) {
	const wantShort = "983a544a"
	body := `{
		"server": {
			"version": "0.0.273",
			"build": "2026-05-15-02-50-00",
			"commit": "` + wantShort + `",
			"started_at": "2026-05-16T07:00:00Z"
		},
		"caller": {
			"user_id": "u_test",
			"email": "test@example.com",
			"auth_kind": "oauth:google"
		},
		"recent_activity": {
			"ledger_rows_last_n": []
		}
	}`

	s := newHotpathStub(t)
	cc := s.client(config.AgentConfig{})
	// Force the real decoder path; the default test wiring routes
	// fetchPprodCommit through pprodInfoFetcher and would mask any
	// drift between satellitesInfoResp and the live wire shape.
	cc.pprodInfoFetcher = nil
	s.setAPIResp("satellites_info", body)

	got, err := cc.fetchPprodCommit(context.Background())
	require.NoError(t, err)
	assert.Equal(t, wantShort, got,
		"fetchPprodCommit must decode server.commit out of the live envelope")

	// Sanity: the call was actually issued via /api/v1/satellites/info.
	require.NotNil(t, s.findAPICall("satellites_info"),
		"fetchPprodCommit must route through /api/v1/satellites/info")
}

// TestFetchPprodCommit_EmptyCommitDecodesEmpty — sty_7a61ae53 AC2
// companion. An envelope whose server.commit is the empty string
// MUST decode as ("", nil) — the fetcher does not synthesise an
// error; the converge loop owns the "treat empty as not-yet-converged"
// semantics (covered by TestPollPprodConverge_RejectsEmpty).
func TestFetchPprodCommit_EmptyCommitDecodesEmpty(t *testing.T) {
	body := `{"server":{"commit":""},"caller":{},"recent_activity":{"ledger_rows_last_n":[]}}`

	s := newHotpathStub(t)
	cc := s.client(config.AgentConfig{})
	cc.pprodInfoFetcher = nil
	s.setAPIResp("satellites_info", body)

	got, err := cc.fetchPprodCommit(context.Background())
	require.NoError(t, err,
		"empty server.commit is a poll-not-converged signal, not a transport error")
	assert.Equal(t, "", got)
}

// TestRunMergeToMainHotPath_EmptyCommitConvergeTimeout_PersistsClose
// — sty_7a61ae53 AC4. Reproduces the sty_224774f0 iter-2/iter-3
// failure mode: the merge_to_main runner pushes successfully but
// every converge sample returns an empty commit, so the poll exhausts
// its window and reports `last=""`. The sty_63541aed persistence
// helpers must still fire — task_update(closed,failure) is recorded
// AND the reason body carries the literal `last=""` substring so the
// failure-evidence ledger row is grep-able for this class of failure.
func TestRunMergeToMainHotPath_EmptyCommitConvergeTimeout_PersistsClose(t *testing.T) {
	s := newHotpathStub(t)
	seedMergeToMainHappyStubs(t, s, "deadbeef")
	// Every converge sample returns "" — the exact shape observed on
	// sty_224774f0's three failed merge_to_main iterations.
	s.pprodCommits = []string{"", "", ""}

	cc := s.client(config.AgentConfig{RepoPath: "/repo", BranchTemplate: "agent-{task_id}-from-{base_sha}"})
	outcome, err := runMergeToMainTask(t, cc)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	assert.Contains(t, err.Error(), "pprod converge timeout")
	assert.Contains(t, err.Error(), `last=""`,
		"timeout error must report the empty last sample so operators can grep for this failure class")

	taskUpd := assertTaskClosedFailure(t, s)
	reason, ok := taskUpd.Body["reason"].(string)
	require.True(t, ok && reason != "",
		"task_update body must carry a non-empty failure reason; got %+v", taskUpd.Body)
	assert.Contains(t, reason, "pprod converge timeout")
	assert.Contains(t, reason, `last=""`,
		"persisted failure reason must embed the empty-sample literal")

	// Push shipped before the converge loop, so the pushed_unverified
	// release-evidence row must still land.
	require.NotNil(t, s.findLedgerAppendWithTag("release:pushed_unverified"),
		"release:pushed_unverified evidence row missing on empty-commit converge timeout")
}

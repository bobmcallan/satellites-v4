// hotpath.go is the in-process executor for lifecycle contracts whose
// dispatch_class is "hot" (commit, merge_to_main). It bypasses the
// seven-step claude subprocess that client_claude.go's Execute uses
// for heavy phases: those steps amortise ~60-120s of setup over
// substantial LLM work, but for git-shape phases the setup dominates
// the actual work.
//
// Selector branch lives in client_claude.go:Execute. This file owns
// runHotPath (the dispatch on ci.Name) + the per-contract runners.
//
// Evidence-row shape parity. The runners emit ledger rows whose tag
// set + content section structure mirror the heavy-path rows that
// shipped earlier (commit → kind:evidence with phase:commit;
// merge_to_main → kind:release-evidence with phase:merge_to_main).
// The orchestrator-side downstream consumers (reviewer rubrics,
// portal renderers, audit dashboards) treat hot and heavy rows
// uniformly.

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// merge_to_main release-watch defaults. The runner caps `gh run
// watch` at defaultGHWatchTimeout and the pprod converge poll at
// defaultPprodConvergeTimeout. Production overrides these via the
// claudeClient fields when set (zero falls back to the defaults).
const (
	defaultGHWatchTimeout       = 10 * time.Minute
	defaultPprodConvergeTimeout = 5 * time.Minute
	defaultPprodPollInterval    = 5 * time.Second
)

// All substrate calls in this file route through c.api (the shared
// /api/v1 HTTP client, internal/cliremote). The legacy MCP tools/call
// transport was retired in sty_74e67353 work#2.

// errHotUnimplemented is the sentinel runHotPath returns when no
// in-process runner exists for the given contract. Execute callers
// MUST fall back to the heavy claude subprocess path on this
// sentinel so a contract marked dispatch_class:hot whose runner is
// missing (e.g. a typo in the seed, or an in-progress migration)
// does not regress — it degrades to the existing heavy behaviour.
var errHotUnimplemented = errors.New("hot-path runner not implemented for contract")

// runHotPath dispatches on contract name to the in-process runner.
// Callers (Execute) provide the same context bundles they would pass
// to the heavy claude subprocess so the runners can compose evidence
// rows without re-fetching anything.
//
// Return shape mirrors Execute: (Outcome, error). errHotUnimplemented
// signals "fall back to heavy"; every other error is a runner failure
// the caller surfaces verbatim.
func (c *claudeClient) runHotPath(ctx context.Context, env TaskEnvelope, ti taskInfo, ai agentInfo, ci contractInfo, si storyInfo) (Outcome, error) {
	switch ci.Name {
	case "commit":
		return c.runCommitHotPath(ctx, env, ti, ai, ci, si)
	case "merge_to_main":
		return c.runMergeToMainHotPath(ctx, env, ti, ai, ci, si)
	default:
		return OutcomeFailure, fmt.Errorf("%w %q", errHotUnimplemented, ci.Name)
	}
}

// taskWalkTask is the subset of task_walk's per-task row the runners
// read for chain inference + chain-shape gate checks.
type taskWalkTask struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Outcome     string `json:"outcome"`
	Iteration   int    `json:"iteration"`
	PriorTaskID string `json:"prior_task_id"`
}

// taskWalkResponse decodes task_walk's envelope down to the fields
// the runners need.
type taskWalkResponse struct {
	Tasks []taskWalkTask `json:"tasks"`
}

// fetchTaskWalk POSTs to /api/v1/task/walk and returns the chain. The
// route returns the full TaskWalkOutput; this decode keeps only the
// fields the hot runners read (Tasks).
func (c *claudeClient) fetchTaskWalk(ctx context.Context, storyID string) (taskWalkResponse, error) {
	if storyID == "" {
		return taskWalkResponse{}, errors.New("task_walk: story_id empty")
	}
	var tw taskWalkResponse
	if err := c.api.Call(ctx, "task_walk", map[string]any{"story_id": storyID}, &tw); err != nil {
		return taskWalkResponse{}, fmt.Errorf("task_walk: %w", err)
	}
	return tw, nil
}

// triggerPayload is the optional orchestrator-supplied JSON on
// task.Trigger. Commit + merge runners honour branch / sha when set.
// Trigger is parsed leniently — unknown keys are ignored and an
// invalid JSON blob degrades to "" / "".
type triggerPayload struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// parseTrigger decodes ti.Trigger; missing / empty / malformed → zero.
func parseTrigger(raw []byte) triggerPayload {
	if len(raw) == 0 {
		return triggerPayload{}
	}
	var tp triggerPayload
	_ = json.Unmarshal(raw, &tp)
	return tp
}

// inferDevelopTaskID returns the id of the most-recent contract:develop
// kind=work task on the chain that closed outcome=success. Errors when
// no such task exists — commit + merge both assume a successful develop
// ancestor.
func inferDevelopTaskID(tw taskWalkResponse) (string, error) {
	for i := len(tw.Tasks) - 1; i >= 0; i-- {
		t := tw.Tasks[i]
		if t.Action != "contract:develop" {
			continue
		}
		// Work tasks: Kind in ("", "work"). Review tasks (Kind="review")
		// are excluded — they don't carry the develop branch.
		if t.Kind != "" && t.Kind != "work" {
			continue
		}
		if t.Outcome != "success" {
			continue
		}
		return t.ID, nil
	}
	return "", errors.New("infer develop task: no successful contract:develop work task on chain")
}

// branchGlob renders the configured BranchTemplate with {task_id}
// substituted and {base_sha} replaced by a `*` glob, returning the
// literal pattern for `git branch --list`. Single source of truth:
// the writer (renderBranchName) and the reader (resolveLocalBranch)
// both consume cfg.BranchTemplate.
func branchGlob(template, devTaskID string) string {
	s := strings.ReplaceAll(template, "{task_id}", devTaskID)
	return strings.ReplaceAll(s, "{base_sha}", "*")
}

// parseBaseSHAFromBranch extracts the rendered `{base_sha}` substring
// from branch given the original template. Single source of truth on
// cfg.BranchTemplate: pairs with `renderBranchName` (the writer) and
// `branchGlob` (the reader) so the commit hot-path's empty-push gate
// reads the base SHA from the same template the worktree path used
// to render the branch in the first place. sty_85b9ec3e.
//
// Implementation: escape the template's literal segments via
// regexp.QuoteMeta, substitute `{task_id}` → `.+?` (non-greedy so the
// trailing `{base_sha}` capture wins), `{base_sha}` → `([^/]+)`, and
// match against branch. Returns the captured group on success.
//
// Errors when the template has no `{base_sha}` placeholder (a custom
// operator template not encoding the base SHA in the branch name).
// The commit runner surfaces this as a failure rather than silently
// skipping the gate — operators who deviate from the canonical
// template opt in to maintaining the same invariant themselves.
func parseBaseSHAFromBranch(template, branch string) (string, error) {
	if !strings.Contains(template, "{base_sha}") {
		return "", fmt.Errorf("parse base sha: template %q has no {base_sha} placeholder", template)
	}
	pattern := regexp.QuoteMeta(template)
	pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta("{task_id}"), `.+?`)
	pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta("{base_sha}"), `([^/]+)`)
	pattern = "^" + pattern + "$"
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("parse base sha: compile %q: %w", pattern, err)
	}
	m := rx.FindStringSubmatch(branch)
	if len(m) != 2 {
		return "", fmt.Errorf("parse base sha: branch %q does not match template %q", branch, template)
	}
	return m[1], nil
}

// resolveLocalBranch returns the local branch name matching the glob
// derived from cfg.BranchTemplate (with {task_id} substituted for
// devTaskID and {base_sha} replaced by `*`). Errors when none /
// multiple matches — the chain convention is one develop task = one
// branch.
//
// `git branch --list` prefixes each line with one of two markers we
// must strip: `* ` for the current branch and `+ ` for a branch that
// is checked out in another worktree. Since dispatched develop tasks
// run inside `<exe_dir>/worktree/<task_id>/`, the repo-root invocation
// always sees the work branch as `+ <branch>`.
func (c *claudeClient) resolveLocalBranch(ctx context.Context, devTaskID string) (string, error) {
	if c.cfg.RepoPath == "" {
		return "", errors.New("resolve branch: repo_path empty in agent config")
	}
	if c.cfg.BranchTemplate == "" {
		return "", errors.New("resolve branch: branch_template empty in agent config")
	}
	pattern := branchGlob(c.cfg.BranchTemplate, devTaskID)
	out, err := c.gitRunner(ctx, c.cfg.RepoPath, "branch", "--list", pattern)
	if err != nil {
		return "", fmt.Errorf("resolve branch: git branch --list %s: %w", pattern, err)
	}
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "* ")
		trimmed = strings.TrimPrefix(trimmed, "+ ")
		line = strings.TrimSpace(trimmed)
		if line == "" {
			continue
		}
		matches = append(matches, line)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("resolve branch: no local branch matching %s in %s", pattern, c.cfg.RepoPath)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("resolve branch: multiple matches for %s: %s", pattern, strings.Join(matches, ", "))
	}
}

// resolveWorkBranch returns the branch the runner should publish.
// Trigger override wins (orchestrator passes {branch, sha} explicitly);
// else the chain is walked for the prior develop task and the
// convention `agent-<devTaskID>-from-*` is resolved against the local
// repo.
func (c *claudeClient) resolveWorkBranch(ctx context.Context, ti taskInfo) (string, error) {
	if tp := parseTrigger(ti.Trigger); tp.Branch != "" {
		return tp.Branch, nil
	}
	tw, err := c.fetchTaskWalk(ctx, ti.StoryID)
	if err != nil {
		return "", err
	}
	dev, err := inferDevelopTaskID(tw)
	if err != nil {
		return "", err
	}
	return c.resolveLocalBranch(ctx, dev)
}

// runCommitHotPath publishes the develop branch to origin and emits
// an evidence row tagged kind:evidence, phase:commit. The commit
// contract is the substrate-level publish action — not a release;
// release semantics live in merge_to_main.
//
// Steps:
//
//  1. Resolve branch (trigger override → task_walk → local branch list).
//  2. Empty-push gate: `git rev-parse <branch>` against the parsed
//     `{base_sha}` suffix expanded via `git rev-parse <baseSHA>`.
//     Equal SHAs short-circuit with a substrate-level failure before
//     log / push / ls-remote run. sty_85b9ec3e.
//  3. git log -1 --pretty=fuller <branch>     — pre-push commit context.
//  4. git push origin <branch>:<branch>       — non-force publish.
//  5. git ls-remote origin <branch>           — origin SHA confirmation.
//  6. ledger_append with kind:evidence shape.
//  7. task_update closed success.
func (c *claudeClient) runCommitHotPath(ctx context.Context, env TaskEnvelope, ti taskInfo, _ agentInfo, _ contractInfo, _ storyInfo) (Outcome, error) {
	branch, err := c.resolveWorkBranch(ctx, ti)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: %w", err)
	}

	headOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: git rev-parse %s: %w", branch, err)
	}
	headSHA := strings.TrimSpace(string(headOut))

	baseSHA, err := parseBaseSHAFromBranch(c.cfg.BranchTemplate, branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: %w", err)
	}
	baseFullOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", baseSHA)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: git rev-parse %s: %w", baseSHA, err)
	}
	baseFullSHA := strings.TrimSpace(string(baseFullOut))

	if headSHA == baseFullSHA {
		return OutcomeFailure, fmt.Errorf("commit: work branch has no new commits relative to base (HEAD=%s base=%s)", headSHA, baseFullSHA)
	}

	logOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "log", "-1", "--pretty=fuller", branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: git log: %w", err)
	}

	pushOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "push", "origin", branch+":"+branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: git push: %w", err)
	}

	lsOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "ls-remote", "origin", branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("commit: git ls-remote: %w", err)
	}

	if err := c.appendHotCommitEvidence(ctx, env, ti, branch, logOut, pushOut, lsOut); err != nil {
		return OutcomeFailure, fmt.Errorf("commit: evidence: %w", err)
	}

	if err := c.closeTaskSuccess(ctx, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("commit: close task: %w", err)
	}
	return OutcomeSuccess, nil
}

// appendHotCommitEvidence writes the kind:evidence ledger row for a
// commit-contract publish. Tag set carries phase:commit.
func (c *claudeClient) appendHotCommitEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, branch string, logOut, pushOut, lsOut []byte) error {
	content := fmt.Sprintf(
		"# Commit evidence — %s\n\n"+
			"**Branch:** `%s`\n"+
			"**Phase:** commit (publish work branch to origin; review-free)\n"+
			"**Dispatch class:** hot (in-process runner).\n\n"+
			"## 1. Pre-publish commit verification\n\n"+
			"`git log -1 --pretty=fuller %s` (literal):\n\n```\n%s\n```\n\n"+
			"## 2. Push output\n\n"+
			"`git push origin %s:%s` (literal):\n\n```\n%s\n```\n\n"+
			"## 3. Origin SHA confirmation\n\n"+
			"`git ls-remote origin %s` (literal):\n\n```\n%s\n```\n\n"+
			"## 4. Constraint compliance\n\n"+
			"- Non-force update (no `+ forced update` in the push output above).\n"+
			"- No tag push, no branch deletion, no other ref touched.\n"+
			"- No source files modified by this task (commit is a publish phase only).\n"+
			"- No release semantics — release is the merge_to_main contract's atomic operation.\n\n"+
			"## Principles cited\n\n"+
			"- `pr_evidence` — every claim above is backed by the literal command output.\n"+
			"- `pr_pipeline_integrity` — commit is the contracted publish phase, executed in-process.\n",
		ti.StoryID, branch,
		branch, strings.TrimSpace(string(logOut)),
		branch, branch, strings.TrimSpace(string(pushOut)),
		branch, strings.TrimSpace(string(lsOut)),
	)
	tags := []string{
		"task_id:" + env.ID,
		"story_id:" + ti.StoryID,
		"phase:commit",
		"branch:" + branch,
		"dispatch_class:hot",
		"kind:evidence",
	}
	return c.appendEvidence(ctx, env.ProjectID, ti.StoryID, tags, content)
}

// runMergeToMainHotPath performs the **atomic release operation**:
// fast-forwards local `main` to the work branch, publishes main to
// origin, watches the GitHub Actions deploy workflow to completion,
// and polls pprod's `satellites_info` until reported `commit` matches
// the pushed SHA. Emits one `kind:release-evidence` ledger row on
// success carrying every step's literal output.
//
// sty_63541aed:
//   - AC3 refuse-on-dirty: aborts before any git mutation when the
//     host repo's working tree has uncommitted changes — the runner is
//     not licensed to discard them.
//   - AC1 persisted close + pushed-but-unverified evidence: every
//     failure path persists `task_update(closed, outcome=failure)`
//     before returning so the substrate's view of the chain matches
//     the CLI's exit code. Post-push failures additionally append a
//     `kind:release-evidence` row tagged `release:pushed_unverified`
//     so the orchestrator sees the SHA is on origin even if pprod
//     never converged.
//   - AC3 post-merge rollback: any failure after `git merge --ff-only`
//     runs `git reset --hard <preMergeHEAD>` so the host repo's local
//     main pointer is restored to where the runner found it.
func (c *claudeClient) runMergeToMainHotPath(ctx context.Context, env TaskEnvelope, ti taskInfo, _ agentInfo, _ contractInfo, _ storyInfo) (Outcome, error) {
	// AC3: refuse on dirty working tree BEFORE any mutation. The host
	// repo's working tree must be clean — uncommitted changes from a
	// prior dispatch or a concurrent operator edit are not ours to
	// discard.
	statusOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "status", "--porcelain")
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: git status --porcelain: %w", err))
	}
	if len(bytes.TrimSpace(statusOut)) > 0 {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf(
			"merge_to_main: refusing on dirty working tree:\n%s",
			strings.TrimSpace(string(statusOut))))
	}

	// AC3: capture the pre-merge HEAD as the rollback anchor for any
	// post-merge failure path.
	preHeadOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", "HEAD")
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: pre-merge rev-parse HEAD: %w", err))
	}
	preMergeHEAD := strings.TrimSpace(string(preHeadOut))

	branch, err := c.resolveWorkBranch(ctx, ti)
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: %w", err))
	}

	tw, err := c.fetchTaskWalk(ctx, ti.StoryID)
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: %w", err))
	}
	if err := verifyChainPriorWorkSuccess(tw, env.ID); err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: %w", err))
	}

	fetchOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "fetch", "origin", "--quiet")
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: git fetch: %w", err))
	}

	preMainOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", "main")
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: git rev-parse main: %w", err))
	}

	if _, err := c.gitRunner(ctx, c.cfg.RepoPath, "merge-base", "--is-ancestor", "main", branch); err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: ff-only ancestor check: %w", err))
	}

	mergeOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "merge", "--ff-only", branch)
	if err != nil {
		return c.failMergePrePush(ctx, env.ID, fmt.Errorf("merge_to_main: git merge: %w", err))
	}

	// === Watershed: merge succeeded; from here on any failure
	// rolls back the local main pointer to preMergeHEAD.

	postMainOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", "main")
	if err != nil {
		return c.failMergePostMergePrePush(ctx, env.ID, preMergeHEAD,
			fmt.Errorf("merge_to_main: post-merge rev-parse: %w", err))
	}
	pushedSHA := strings.TrimSpace(string(postMainOut))

	pushMainOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "push", "origin", "main")
	if err != nil {
		return c.failMergePostMergePrePush(ctx, env.ID, preMergeHEAD,
			fmt.Errorf("merge_to_main: git push origin main: %w", err))
	}

	// === Watershed: push to origin succeeded; from here on any
	// failure additionally writes a `release:pushed_unverified`
	// release-evidence row so the orchestrator can see the SHA is
	// on origin even though convergence is unverified.

	runListOut, err := c.runGH(ctx, "run", "list", "--branch", "main", "--limit", "1", "--json", "databaseId,headSha,status,conclusion")
	if err != nil {
		return c.failMergePostPush(ctx, env, ti, branch, preMergeHEAD, pushedSHA,
			fetchOut, preMainOut, mergeOut, postMainOut, pushMainOut,
			"", nil, nil, nil,
			fmt.Errorf("merge_to_main: gh run list: %w", err))
	}
	runID, err := parseGHRunID(runListOut)
	if err != nil {
		return c.failMergePostPush(ctx, env, ti, branch, preMergeHEAD, pushedSHA,
			fetchOut, preMainOut, mergeOut, postMainOut, pushMainOut,
			"", runListOut, nil, nil,
			fmt.Errorf("merge_to_main: parse gh run list: %w", err))
	}

	watchCtx, watchCancel := context.WithTimeout(ctx, c.effectiveGHWatchTimeout())
	defer watchCancel()
	watchOut, err := c.runGH(watchCtx, "run", "watch", runID, "--exit-status")
	if err != nil {
		return c.failMergePostPush(ctx, env, ti, branch, preMergeHEAD, pushedSHA,
			fetchOut, preMainOut, mergeOut, postMainOut, pushMainOut,
			runID, runListOut, watchOut, nil,
			fmt.Errorf("merge_to_main: gh run watch: %w", err))
	}

	convergeSamples, err := c.pollPprodConverge(ctx, pushedSHA)
	if err != nil {
		return c.failMergePostPush(ctx, env, ti, branch, preMergeHEAD, pushedSHA,
			fetchOut, preMainOut, mergeOut, postMainOut, pushMainOut,
			runID, runListOut, watchOut, convergeSamples,
			fmt.Errorf("merge_to_main: %w", err))
	}

	if err := c.appendHotMergeReleaseEvidence(ctx, env, ti, branch, pushedSHA, fetchOut, preMainOut, mergeOut, postMainOut, pushMainOut, runID, runListOut, watchOut, convergeSamples); err != nil {
		// The release converged on pprod; don't roll back local main
		// (rolling back wouldn't undo the deploy). Persist the close
		// as failure so the chain reflects the evidence-append gap.
		_ = c.closeTaskFailure(ctx, env.ID, fmt.Sprintf("merge_to_main: evidence: %v", err))
		return OutcomeFailure, fmt.Errorf("merge_to_main: evidence: %w", err)
	}

	if err := c.closeTaskSuccess(ctx, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: close task: %w", err)
	}
	return OutcomeSuccess, nil
}

// failMergePrePush persists task_update(closed, failure) for a
// failure observed BEFORE git merge ran. No rollback (the merge
// never advanced anything) and no release-evidence row (nothing was
// pushed). The close call is best-effort: a substrate error does not
// mask the original error the CLI surfaces.
func (c *claudeClient) failMergePrePush(ctx context.Context, taskID string, originalErr error) (Outcome, error) {
	_ = c.closeTaskFailure(ctx, taskID, originalErr.Error())
	return OutcomeFailure, originalErr
}

// failMergePostMergePrePush handles a failure between
// `git merge --ff-only` and the watershed push: the local main
// pointer advanced; we roll it back to preMergeHEAD (AC3) and persist
// the task close before returning. Best-effort throughout — neither
// reset nor close errors mask the original failure.
func (c *claudeClient) failMergePostMergePrePush(ctx context.Context, taskID, preMergeHEAD string, originalErr error) (Outcome, error) {
	if preMergeHEAD != "" {
		_, _ = c.gitRunner(ctx, c.cfg.RepoPath, "reset", "--hard", preMergeHEAD)
	}
	_ = c.closeTaskFailure(ctx, taskID, originalErr.Error())
	return OutcomeFailure, originalErr
}

// failMergePostPush handles a failure AFTER `git push origin main`
// has shipped the merge to origin: the release exists on origin but
// pprod-convergence is unverified. The runner rolls back local main
// to preMergeHEAD (AC3), appends a `release:pushed_unverified`
// release-evidence row carrying every captured output + the literal
// failure reason (AC1), and persists task_update(closed, failure)
// before returning the original error. All side-effects are
// best-effort — none mask the original error.
func (c *claudeClient) failMergePostPush(
	ctx context.Context, env TaskEnvelope, ti taskInfo,
	branch, preMergeHEAD, pushedSHA string,
	fetchOut, preMain, mergeOut, postMain, pushMainOut []byte,
	runID string, runListOut, watchOut []byte, samples []pprodPollSample,
	originalErr error,
) (Outcome, error) {
	if preMergeHEAD != "" {
		_, _ = c.gitRunner(ctx, c.cfg.RepoPath, "reset", "--hard", preMergeHEAD)
	}
	_ = c.appendHotMergePushedUnverifiedEvidence(ctx, env, ti, branch, preMergeHEAD, pushedSHA,
		fetchOut, preMain, mergeOut, postMain, pushMainOut,
		runID, runListOut, watchOut, samples, originalErr)
	_ = c.closeTaskFailure(ctx, env.ID, originalErr.Error())
	return OutcomeFailure, originalErr
}

// runGH dispatches to the injected ghRunner; nil falls back to the
// package-level runGH so production stays a one-line shell-out.
func (c *claudeClient) runGH(ctx context.Context, args ...string) ([]byte, error) {
	if c.ghRunner != nil {
		return c.ghRunner(ctx, args...)
	}
	return runGH(ctx, args...)
}

// effectiveGHWatchTimeout returns the configured timeout or the
// package default when unset (zero).
func (c *claudeClient) effectiveGHWatchTimeout() time.Duration {
	if c.ghWatchTimeout > 0 {
		return c.ghWatchTimeout
	}
	return defaultGHWatchTimeout
}

// effectivePprodConvergeTimeout returns the configured pprod converge
// timeout or the package default when unset (zero).
func (c *claudeClient) effectivePprodConvergeTimeout() time.Duration {
	if c.pprodConvergeTimeout > 0 {
		return c.pprodConvergeTimeout
	}
	return defaultPprodConvergeTimeout
}

// effectivePprodPollInterval returns the configured poll interval or
// the package default when unset (zero).
func (c *claudeClient) effectivePprodPollInterval() time.Duration {
	if c.pprodPollInterval > 0 {
		return c.pprodPollInterval
	}
	return defaultPprodPollInterval
}

// satellitesInfoResp decodes the subset of satellites_info the
// converge poll reads. Mirrors the {server, caller, recent_activity}
// shape (sty_0be97c3e); only the server-side commit hash is needed
// to detect when a pushed SHA has rolled onto pprod.
type satellitesInfoResp struct {
	Server struct {
		Commit string `json:"commit"`
	} `json:"server"`
}

// pprodPollSample is one converge-poll observation captured for the
// release evidence row.
type pprodPollSample struct {
	At     time.Time `json:"at"`
	Commit string    `json:"commit"`
}

// pollPprodConverge polls pprod's satellites_info until its reported
// commit matches pushedSHA or the converge timeout expires. Returns
// the ordered samples (including the converged final sample) on
// success; an error wrapping the last observed commit on timeout.
func (c *claudeClient) pollPprodConverge(ctx context.Context, pushedSHA string) ([]pprodPollSample, error) {
	timeout := c.effectivePprodConvergeTimeout()
	interval := c.effectivePprodPollInterval()
	deadline := time.Now().Add(timeout)
	var samples []pprodPollSample
	for {
		commit, err := c.fetchPprodCommit(ctx)
		if err != nil {
			return samples, fmt.Errorf("pprod converge: satellites_info: %w", err)
		}
		samples = append(samples, pprodPollSample{At: time.Now().UTC(), Commit: commit})
		if commit == pushedSHA {
			return samples, nil
		}
		if time.Now().After(deadline) {
			return samples, fmt.Errorf("pprod converge timeout: last=%q want=%q", commit, pushedSHA)
		}
		select {
		case <-ctx.Done():
			return samples, fmt.Errorf("pprod converge: ctx: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// fetchPprodCommit returns pprod's currently reported running commit.
// Tests override via pprodInfoFetcher; production routes through the
// shared /api/v1 client (c.api.Call satellites_info).
func (c *claudeClient) fetchPprodCommit(ctx context.Context) (string, error) {
	if c.pprodInfoFetcher != nil {
		return c.pprodInfoFetcher(ctx)
	}
	if c.api == nil {
		return "", errors.New("api client unset")
	}
	var info satellitesInfoResp
	if err := c.api.Call(ctx, "satellites_info", map[string]any{}, &info); err != nil {
		return "", err
	}
	return info.Server.Commit, nil
}

// parseGHRunID extracts the databaseId of the latest run from
// `gh run list --json databaseId,...` output. The JSON shape is an
// array of run objects; we read the first entry's databaseId.
func parseGHRunID(out []byte) (string, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return "", errors.New("gh run list returned empty output")
	}
	var rows []struct {
		DatabaseID int64  `json:"databaseId"`
		HeadSHA    string `json:"headSha"`
	}
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return "", fmt.Errorf("decode gh run list: %w", err)
	}
	if len(rows) == 0 {
		return "", errors.New("gh run list returned no workflow runs")
	}
	if rows[0].DatabaseID == 0 {
		return "", errors.New("gh run list: first row has empty databaseId")
	}
	return fmt.Sprintf("%d", rows[0].DatabaseID), nil
}

// appendHotMergeReleaseEvidence writes the kind:release-evidence
// ledger row covering the atomic release: local merge + main push +
// GitHub Actions deploy watch + pprod converge poll. The mechanical
// `story_close` MCP verb consults the pushedSHA stored on this row to
// surface the `deploy:behind` gap when pprod regresses.
func (c *claudeClient) appendHotMergeReleaseEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, branch, pushedSHA string, fetchOut, preMain, mergeOut, postMain, pushMainOut []byte, runID string, runListOut, watchOut []byte, samples []pprodPollSample) error {
	convergeLines := make([]string, 0, len(samples))
	for _, s := range samples {
		convergeLines = append(convergeLines, fmt.Sprintf("- %s commit=%s", s.At.Format(time.RFC3339), s.Commit))
	}
	content := fmt.Sprintf(
		"## merge_to_main release evidence — %s\n\n"+
			"**Branch merged:** `%s`\n"+
			"**Pushed SHA:** `%s`\n"+
			"**GH workflow run id:** `%s`\n"+
			"**Dispatch class:** hot (in-process runner).\n\n"+
			"## 1. Pre-merge state\n\n"+
			"```\n$ git -C <repo> fetch origin --quiet\n%s\n\n"+
			"$ git -C <repo> rev-parse main\n%s\n\n"+
			"$ git -C <repo> merge-base --is-ancestor main %s && echo OK\nOK\n```\n\n"+
			"Ancestor check returned OK — fast-forward is the only possible resolution.\n\n"+
			"## 2. Merge command output (literal)\n\n"+
			"```\n$ git -C <repo> merge --ff-only %s\n%s\n```\n\n"+
			"`Fast-forward` confirms explicit fast-forward resolution.\n\n"+
			"## 3. Post-merge state\n\n"+
			"```\n$ git -C <repo> rev-parse main\n%s\n```\n\n"+
			"## 4. Main push to origin\n\n"+
			"```\n$ git -C <repo> push origin main\n%s\n```\n\n"+
			"Non-force update.\n\n"+
			"## 5. GitHub Actions deploy watch\n\n"+
			"`gh run list --branch main --limit 1 --json databaseId,headSha,status,conclusion` (literal):\n\n"+
			"```\n%s\n```\n\n"+
			"`gh run watch %s --exit-status` (literal):\n\n```\n%s\n```\n\n"+
			"## 6. pprod converge polling\n\n"+
			"%s\n\n"+
			"## Constraints honoured\n\n"+
			"- `--ff-only` ABSOLUTE: ancestor check ran before the merge command; the merge output literally says `Fast-forward`.\n"+
			"- `git push origin main` is non-force; no tags, no branch deletion.\n"+
			"- `gh run watch` returned `conclusion=success` before the converge poll ran.\n"+
			"- pprod's reported `commit` matches the pushed SHA at the final sample.\n\n"+
			"## Principles cited\n\n"+
			"- `pr_evidence` — every claim is backed by literal command output.\n"+
			"- `pr_pipeline_integrity` — the merge ran through the contracted ff-only ancestor gate.\n"+
			"- `pr_pipeline_authority` — release is not complete until pprod reports the pushed SHA.\n",
		ti.StoryID, branch, pushedSHA, runID,
		strings.TrimSpace(string(fetchOut)),
		strings.TrimSpace(string(preMain)),
		branch,
		branch, strings.TrimSpace(string(mergeOut)),
		strings.TrimSpace(string(postMain)),
		strings.TrimSpace(string(pushMainOut)),
		strings.TrimSpace(string(runListOut)),
		runID, strings.TrimSpace(string(watchOut)),
		strings.Join(convergeLines, "\n"),
	)
	tags := []string{
		"task_id:" + env.ID,
		"story_id:" + ti.StoryID,
		"phase:merge_to_main",
		"branch:" + branch,
		"dispatch_class:hot",
		"kind:release-evidence",
		"pushed_sha:" + pushedSHA,
		"gh_run_id:" + runID,
	}
	return c.appendEvidence(ctx, env.ProjectID, ti.StoryID, tags, content)
}

// verifyChainPriorWorkSuccess fails when the chain ending at ignoreID
// contains a closed=failure kind=work task that no later task has
// superseded via prior_task_id. Accepts retries that carry
// `prior_task_id` to a closed=failure predecessor — per
// `pr_pipeline_authority`, a fresh kind=work task minted with
// `prior_task_id=<failed_predecessor>` is the documented recovery
// primitive, and the chain on `task_walk(story_id)` is the
// audit-of-record. The gate accepts the chain iff every closed=failure
// work task has at least one successor — direct or transitive — in the
// walk pointing at it via prior_task_id. Open work tasks (status !=
// closed) still fail the gate. The current retry task (ignoreID) counts
// as a valid successor when it carries the linkage.
func verifyChainPriorWorkSuccess(tw taskWalkResponse, ignoreID string) error {
	supersededBy := buildPriorIndex(tw)
	for _, t := range tw.Tasks {
		if t.ID == ignoreID {
			continue
		}
		if t.Kind != "" && t.Kind != "work" {
			continue
		}
		// Open work task on the chain → chain not consistent.
		if t.Status != "closed" {
			return fmt.Errorf("chain has open work task %q (action=%s, status=%s)", t.ID, t.Action, t.Status)
		}
		if t.Outcome != "success" {
			if hasSuccessorRetry(supersededBy, t.ID) {
				continue
			}
			return fmt.Errorf("chain has unsuccessful prior work task %q (action=%s, outcome=%s) with no retry successor", t.ID, t.Action, t.Outcome)
		}
	}
	return nil
}

// buildPriorIndex inverts the chain: for each task that carries a
// non-empty PriorTaskID, append its ID to the predecessor's successor
// list. The result is a map from predecessor-task-ID → list of tasks
// that point at it via prior_task_id.
func buildPriorIndex(tw taskWalkResponse) map[string][]string {
	idx := make(map[string][]string)
	for _, t := range tw.Tasks {
		if t.PriorTaskID == "" {
			continue
		}
		idx[t.PriorTaskID] = append(idx[t.PriorTaskID], t.ID)
	}
	return idx
}

// hasSuccessorRetry reports whether failedID has at least one
// successor pointing at it via prior_task_id. The current retry task
// (the caller's ignoreID) counts when it appears in supersededBy —
// that is exactly the documented retry shape
// [failed → retry(prior_task_id=failed)]. The outer loop in
// verifyChainPriorWorkSuccess handles transitivity: if the direct
// successor is itself a closed=failure with no successor of its own,
// the outer loop will reject when it iterates to that task.
func hasSuccessorRetry(supersededBy map[string][]string, failedID string) bool {
	return len(supersededBy[failedID]) > 0
}

// appendEvidence POSTs to /api/v1/ledger/append. Threads through
// project/story when set; leaves them off the args map when empty so
// the substrate's defaults apply.
func (c *claudeClient) appendEvidence(ctx context.Context, projectID, storyID string, tags []string, content string) error {
	args := map[string]any{
		"type":    "evidence",
		"tags":    tags,
		"content": content,
	}
	if projectID != "" {
		args["project_id"] = projectID
	}
	if storyID != "" {
		args["story_id"] = storyID
	}
	return c.api.Call(ctx, "ledger_append", args, nil)
}

// closeTaskSuccess closes the task via /api/v1/task/update. Failure
// surfaces as an error the caller wraps with phase context.
func (c *claudeClient) closeTaskSuccess(ctx context.Context, taskID string) error {
	return c.api.Call(ctx, "task_update", map[string]any{
		"id":      taskID,
		"status":  "closed",
		"outcome": "success",
	}, nil)
}

// closeTaskFailure persists task_update(status=closed, outcome=failure)
// on the runner's task before returning failure (sty_63541aed AC1).
// The reason is threaded into the request body for orchestrator-side
// visibility; the substrate ignores unknown fields, so this is safe
// to send today and forward-compatible with a future server-side
// audit field. Callers invoke this best-effort: a substrate error is
// logged via the underlying api client and does not mask the runner's
// original failure.
func (c *claudeClient) closeTaskFailure(ctx context.Context, taskID, reason string) error {
	args := map[string]any{
		"id":      taskID,
		"status":  "closed",
		"outcome": "failure",
	}
	if reason != "" {
		args["reason"] = reason
	}
	return c.api.Call(ctx, "task_update", args, nil)
}

// appendHotMergePushedUnverifiedEvidence writes the
// `kind:release-evidence` row variant emitted when the merge pushed
// to origin but a subsequent post-push step (gh watch, pprod converge,
// evidence-append) failed. Carries every captured output up to the
// failure plus the literal failure reason, tagged
// `release:pushed_unverified` so the story_close gate + downstream
// readers can distinguish a shipped-but-unverified release from a
// fully-converged one.
func (c *claudeClient) appendHotMergePushedUnverifiedEvidence(
	ctx context.Context, env TaskEnvelope, ti taskInfo,
	branch, preMergeHEAD, pushedSHA string,
	fetchOut, preMain, mergeOut, postMain, pushMainOut []byte,
	runID string, runListOut, watchOut []byte, samples []pprodPollSample,
	originalErr error,
) error {
	convergeLines := make([]string, 0, len(samples))
	for _, s := range samples {
		convergeLines = append(convergeLines, fmt.Sprintf("- %s commit=%s", s.At.Format(time.RFC3339), s.Commit))
	}
	convergeSection := strings.Join(convergeLines, "\n")
	if convergeSection == "" {
		convergeSection = "(no converge samples captured before failure)"
	}
	runWatchSection := strings.TrimSpace(string(watchOut))
	if runWatchSection == "" {
		runWatchSection = "(no gh run watch output captured before failure)"
	}
	runListSection := strings.TrimSpace(string(runListOut))
	if runListSection == "" {
		runListSection = "(no gh run list output captured before failure)"
	}
	content := fmt.Sprintf(
		"## merge_to_main pushed-but-unverified release evidence — %s\n\n"+
			"**Branch merged:** `%s`\n"+
			"**Pushed SHA:** `%s` (on origin/main)\n"+
			"**Pre-merge HEAD (rolled back to):** `%s`\n"+
			"**GH workflow run id:** `%s`\n"+
			"**Dispatch class:** hot (in-process runner).\n\n"+
			"## Failure reason\n\n"+
			"```\n%s\n```\n\n"+
			"## 1. Pre-merge state\n\n"+
			"```\n$ git -C <repo> fetch origin --quiet\n%s\n\n"+
			"$ git -C <repo> rev-parse main\n%s\n```\n\n"+
			"## 2. Merge command output (literal)\n\n"+
			"```\n$ git -C <repo> merge --ff-only %s\n%s\n```\n\n"+
			"## 3. Post-merge state\n\n"+
			"```\n$ git -C <repo> rev-parse main\n%s\n```\n\n"+
			"## 4. Main push to origin\n\n"+
			"```\n$ git -C <repo> push origin main\n%s\n```\n\n"+
			"Non-force update; the push succeeded — pushed SHA is on origin.\n\n"+
			"## 5. GitHub Actions deploy watch\n\n"+
			"`gh run list` (literal, may be partial):\n\n```\n%s\n```\n\n"+
			"`gh run watch` (literal, may be partial):\n\n```\n%s\n```\n\n"+
			"## 6. pprod converge polling\n\n"+
			"%s\n\n"+
			"## 7. Rollback\n\n"+
			"`git reset --hard %s` issued (best-effort) to restore the host repo's local main pointer to the pre-merge anchor.\n\n"+
			"## Principles cited\n\n"+
			"- `pr_evidence` — every claim above is the literal captured output up to the failure point.\n"+
			"- `pr_pipeline_authority` — the task is closed `outcome=failure` so the substrate's view of the chain matches this CLI exit. The orchestrator recovers via the standard chain-shape path (mint a fresh merge_to_main task carrying `prior_task_id`).\n",
		ti.StoryID, branch, pushedSHA, preMergeHEAD, runID,
		originalErr.Error(),
		strings.TrimSpace(string(fetchOut)),
		strings.TrimSpace(string(preMain)),
		branch, strings.TrimSpace(string(mergeOut)),
		strings.TrimSpace(string(postMain)),
		strings.TrimSpace(string(pushMainOut)),
		runListSection,
		runWatchSection,
		convergeSection,
		preMergeHEAD,
	)
	tags := []string{
		"task_id:" + env.ID,
		"story_id:" + ti.StoryID,
		"phase:merge_to_main",
		"branch:" + branch,
		"dispatch_class:hot",
		"kind:release-evidence",
		"release:pushed_unverified",
		"pushed_sha:" + pushedSHA,
	}
	if runID != "" {
		tags = append(tags, "gh_run_id:"+runID)
	}
	return c.appendEvidence(ctx, env.ProjectID, ti.StoryID, tags, content)
}

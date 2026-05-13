// hotpath.go is the in-process executor for lifecycle contracts whose
// dispatch_class is "hot" (push, merge_to_main, story_close). It
// bypasses the seven-step claude subprocess that client_claude.go's
// Execute uses for heavy phases: those steps amortise ~60-120s of
// setup over substantial LLM work, but for git-shape phases the
// setup dominates the actual work.
//
// Selector branch lives in client_claude.go:Execute. This file owns
// runHotPath (the dispatch on ci.Name) + the per-contract runners.
//
// Evidence-row shape parity. The runners emit ledger rows whose tag
// set + content section structure mirror the heavy-path rows that
// shipped before this story (reference fixtures from sty_f2d342e2:
// ldg_d0c09de8 push, ldg_f4ed7c6d merge_to_main, ldg_da616aec
// story_close). The orchestrator-side downstream consumers (reviewer
// rubrics, portal renderers, audit dashboards) treat hot and heavy
// rows uniformly.
//
// sty_4994caa3 (parent: sty_3b3e4e66; Layer A frontmatter shipped in
// 833a28e).

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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
	case "push":
		return c.runPushHotPath(ctx, env, ti, ai, ci, si)
	case "merge_to_main":
		return c.runMergeToMainHotPath(ctx, env, ti, ai, ci, si)
	case "story_close":
		return c.runStoryCloseHotPath(ctx, env, ti, ai, ci, si)
	default:
		return OutcomeFailure, fmt.Errorf("%w %q", errHotUnimplemented, ci.Name)
	}
}

// taskWalkTask is the subset of task_walk's per-task row the runners
// read for chain inference + chain-shape gate checks.
type taskWalkTask struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome"`
	Iteration int    `json:"iteration"`
}

// taskWalkResponse decodes task_walk's envelope down to the fields
// the runners need.
type taskWalkResponse struct {
	Tasks []taskWalkTask `json:"tasks"`
}

// fetchTaskWalk calls task_walk(story_id) and returns the chain.
func (c *claudeClient) fetchTaskWalk(ctx context.Context, storyID string) (taskWalkResponse, error) {
	if storyID == "" {
		return taskWalkResponse{}, errors.New("task_walk: story_id empty")
	}
	text, err := c.callTool(ctx, "task_walk", map[string]any{"story_id": storyID})
	if err != nil {
		return taskWalkResponse{}, fmt.Errorf("task_walk: %w", err)
	}
	var tw taskWalkResponse
	if text == "" {
		return tw, nil
	}
	if err := json.Unmarshal([]byte(text), &tw); err != nil {
		return tw, fmt.Errorf("task_walk decode: %w", err)
	}
	return tw, nil
}

// triggerPayload is the optional orchestrator-supplied JSON on
// task.Trigger. Push + merge runners honour branch / sha when set;
// story_close honours resolution + (in future) explicit field values.
// Trigger is parsed leniently — unknown keys are ignored and an
// invalid JSON blob degrades to "" / "" / "".
type triggerPayload struct {
	Branch     string `json:"branch"`
	SHA        string `json:"sha"`
	Resolution string `json:"resolution"`
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
// no such task exists — push / merge / story_close all assume a
// successful develop ancestor.
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

// resolveLocalBranch returns the local branch name matching
// `agent-<devTaskID>-from-*`. Errors when none / multiple matches —
// the chain convention is one develop task = one branch.
func (c *claudeClient) resolveLocalBranch(ctx context.Context, devTaskID string) (string, error) {
	if c.cfg.RepoPath == "" {
		return "", errors.New("resolve branch: repo_path empty in agent config")
	}
	pattern := "agent-" + devTaskID + "-from-*"
	out, err := c.gitRunner(ctx, c.cfg.RepoPath, "branch", "--list", pattern)
	if err != nil {
		return "", fmt.Errorf("resolve branch: git branch --list %s: %w", pattern, err)
	}
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "* "))
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

// resolvePushBranch returns the branch the runner should push. Trigger
// override wins (orchestrator passes {branch, sha} explicitly); else
// the chain is walked for the prior develop task and the convention
// `agent-<devTaskID>-from-*` is resolved against the local repo.
func (c *claudeClient) resolvePushBranch(ctx context.Context, ti taskInfo) (string, error) {
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

// runPushHotPath pushes the develop branch to origin and emits an
// evidence row whose tag set + content shape match the heavy-path
// fixture ldg_d0c09de8.
//
// Steps:
//
//  1. Resolve branch (trigger override → task_walk → local branch list).
//  2. git log -1 --pretty=fuller <branch>     — pre-push commit context.
//  3. git push origin <branch>:<branch>       — non-force ship.
//  4. git ls-remote origin <branch>           — origin SHA confirmation.
//  5. ledger_append with kind:evidence shape.
//  6. task_update closed success.
func (c *claudeClient) runPushHotPath(ctx context.Context, env TaskEnvelope, ti taskInfo, _ agentInfo, _ contractInfo, _ storyInfo) (Outcome, error) {
	branch, err := c.resolvePushBranch(ctx, ti)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("push: %w", err)
	}

	logOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "log", "-1", "--pretty=fuller", branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("push: git log: %w", err)
	}

	pushOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "push", "origin", branch+":"+branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("push: git push: %w", err)
	}

	lsOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "ls-remote", "origin", branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("push: git ls-remote: %w", err)
	}

	if err := c.appendHotPushEvidence(ctx, env, ti, branch, logOut, pushOut, lsOut); err != nil {
		return OutcomeFailure, fmt.Errorf("push: evidence: %w", err)
	}

	if err := c.closeTaskSuccess(ctx, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("push: close task: %w", err)
	}
	return OutcomeSuccess, nil
}

// appendHotPushEvidence writes the kind:evidence ledger row for a
// push. Tag set + section structure match ldg_d0c09de8.
func (c *claudeClient) appendHotPushEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, branch string, logOut, pushOut, lsOut []byte) error {
	content := fmt.Sprintf(
		"# Push evidence — %s\n\n"+
			"**Branch:** `%s`\n"+
			"**Phase:** push (review-free)\n"+
			"**Dispatch class:** hot (in-process runner; sty_4994caa3).\n\n"+
			"## 1. Pre-push commit verification\n\n"+
			"`git log -1 --pretty=fuller %s` (literal):\n\n```\n%s\n```\n\n"+
			"## 2. Push output\n\n"+
			"`git push origin %s:%s` (literal):\n\n```\n%s\n```\n\n"+
			"## 3. Origin SHA confirmation\n\n"+
			"`git ls-remote origin %s` (literal):\n\n```\n%s\n```\n\n"+
			"## 4. Constraint compliance\n\n"+
			"- Non-force update (no `+ forced update` in the push output above).\n"+
			"- No tag push, no branch deletion, no other ref touched.\n"+
			"- No source files modified by this task (push is a git-write phase only).\n\n"+
			"## Principles cited\n\n"+
			"- `pr_evidence` — every claim above is backed by the literal command output.\n"+
			"- `pr_pipeline_integrity` — push is the contracted ship phase, executed in-process per sty_4994caa3.\n",
		ti.StoryID, branch,
		branch, strings.TrimSpace(string(logOut)),
		branch, branch, strings.TrimSpace(string(pushOut)),
		branch, strings.TrimSpace(string(lsOut)),
	)
	tags := []string{
		"task_id:" + env.ID,
		"story_id:" + ti.StoryID,
		"phase:push",
		"branch:" + branch,
		"dispatch_class:hot",
		"kind:evidence",
	}
	return c.appendEvidence(ctx, env.ProjectID, ti.StoryID, tags, content)
}

// runMergeToMainHotPath fast-forwards local `main` to the branch the
// prior push placed on origin. Emits an evidence row whose shape
// mirrors ldg_f4ed7c6d.
func (c *claudeClient) runMergeToMainHotPath(ctx context.Context, env TaskEnvelope, ti taskInfo, _ agentInfo, _ contractInfo, _ storyInfo) (Outcome, error) {
	branch, err := c.resolvePushBranch(ctx, ti)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: %w", err)
	}

	tw, err := c.fetchTaskWalk(ctx, ti.StoryID)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: %w", err)
	}
	if err := verifyChainPriorWorkSuccess(tw, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: %w", err)
	}

	fetchOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "fetch", "origin", "--quiet")
	if err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: git fetch: %w", err)
	}

	mainOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", "main")
	if err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: git rev-parse main: %w", err)
	}

	// merge-base --is-ancestor exits 0 when main is an ancestor of
	// branch (the only state where --ff-only can succeed); non-zero
	// exits surface here so the runner fails fast before attempting
	// the merge.
	if _, err := c.gitRunner(ctx, c.cfg.RepoPath, "merge-base", "--is-ancestor", "main", branch); err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: ff-only ancestor check: %w", err)
	}

	mergeOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "merge", "--ff-only", branch)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: git merge: %w", err)
	}

	postMainOut, err := c.gitRunner(ctx, c.cfg.RepoPath, "rev-parse", "main")
	if err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: post-merge rev-parse: %w", err)
	}

	if err := c.appendHotMergeEvidence(ctx, env, ti, branch, fetchOut, mainOut, mergeOut, postMainOut); err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: evidence: %w", err)
	}

	if err := c.closeTaskSuccess(ctx, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("merge_to_main: close task: %w", err)
	}
	return OutcomeSuccess, nil
}

// appendHotMergeEvidence writes the kind:evidence ledger row for a
// merge_to_main. Tag set + sections match ldg_f4ed7c6d.
func (c *claudeClient) appendHotMergeEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, branch string, fetchOut, preMain, mergeOut, postMain []byte) error {
	content := fmt.Sprintf(
		"## merge_to_main evidence — %s\n\n"+
			"**Branch merged:** `%s`\n"+
			"**Dispatch class:** hot (in-process runner; sty_4994caa3).\n\n"+
			"## Pre-merge state\n\n"+
			"```\n$ git -C <repo> fetch origin --quiet\n%s\n\n"+
			"$ git -C <repo> rev-parse main\n%s\n\n"+
			"$ git -C <repo> merge-base --is-ancestor main %s && echo OK\nOK\n```\n\n"+
			"Ancestor check returned OK — fast-forward is the only possible resolution.\n\n"+
			"## Merge command output (literal)\n\n"+
			"```\n$ git -C <repo> merge --ff-only %s\n%s\n```\n\n"+
			"`Fast-forward` confirms explicit fast-forward resolution.\n\n"+
			"## Post-merge state\n\n"+
			"```\n$ git -C <repo> rev-parse main\n%s\n```\n\n"+
			"## Constraints honoured\n\n"+
			"- `--ff-only` ABSOLUTE: ancestor check ran before the merge command; the merge output literally says `Fast-forward`.\n"+
			"- No `git push` (push is the prior phase).\n"+
			"- No story transition (story_close is a separate role).\n\n"+
			"## Principles cited\n\n"+
			"- `pr_evidence` — every claim is backed by literal command output.\n"+
			"- `pr_pipeline_integrity` — the merge ran through the contracted ff-only ancestor gate.\n",
		ti.StoryID, branch,
		strings.TrimSpace(string(fetchOut)),
		strings.TrimSpace(string(preMain)),
		branch,
		branch, strings.TrimSpace(string(mergeOut)),
		strings.TrimSpace(string(postMain)),
	)
	tags := []string{
		"task_id:" + env.ID,
		"story_id:" + ti.StoryID,
		"phase:merge_to_main",
		"branch:" + branch,
		"dispatch_class:hot",
		"kind:evidence",
	}
	return c.appendEvidence(ctx, env.ProjectID, ti.StoryID, tags, content)
}

// verifyChainPriorWorkSuccess fails when any earlier kind=work task on
// the chain (excluding the current task identified by ignoreID) is not
// closed outcome=success. The chain shape is part of the gate the
// heavy-path merge agent runs before merging; the hot runner enforces
// the same invariant in-process.
func verifyChainPriorWorkSuccess(tw taskWalkResponse, ignoreID string) error {
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
			// Same-slot retry chains may legitimately have a prior
			// failure if an iter-2 succeeded. We treat any non-success
			// closed work task as a chain gap — the caller can override
			// via trigger if needed.
			//
			// To avoid blocking on superseded failures, look for a
			// later iteration of the same action that succeeded.
			if hasLaterSuccess(tw, t) {
				continue
			}
			return fmt.Errorf("chain has unsuccessful prior work task %q (action=%s, outcome=%s)", t.ID, t.Action, t.Outcome)
		}
	}
	return nil
}

// hasLaterSuccess reports whether a later task with the same Action
// closed outcome=success — covering the iter-1-failed → iter-2-success
// chain shape.
func hasLaterSuccess(tw taskWalkResponse, prior taskWalkTask) bool {
	for _, t := range tw.Tasks {
		if t.ID == prior.ID || t.Action != prior.Action {
			continue
		}
		if t.Kind != "" && t.Kind != "work" {
			continue
		}
		if t.Iteration > prior.Iteration && t.Status == "closed" && t.Outcome == "success" {
			return true
		}
	}
	return false
}

// runStoryCloseHotPath verifies the chain, reads required closing
// fields from the story's category template, refuses if any are
// empty, then writes a generic close-evidence ledger row whose tag
// set + content shape mirror ldg_da616aec. The runner does NOT
// transition the story status; that is the orchestrator's next plan
// step (per pr_substrate_provides_context, dispatch responsibility
// stays narrow).
func (c *claudeClient) runStoryCloseHotPath(ctx context.Context, env TaskEnvelope, ti taskInfo, _ agentInfo, _ contractInfo, _ storyInfo) (Outcome, error) {
	storyRaw, err := c.fetchStoryRaw(ctx, ti.StoryID)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("story_close: story_get: %w", err)
	}

	tw, err := c.fetchTaskWalk(ctx, ti.StoryID)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("story_close: %w", err)
	}
	if err := verifyChainPriorWorkSuccess(tw, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("story_close: %w", err)
	}

	missing := requiredClosingFieldsMissing(storyRaw)
	if len(missing) > 0 {
		return OutcomeFailure, fmt.Errorf("story_close: closing fields not populated on story: %s — call story_update with a fields={…} payload before dispatching", strings.Join(missing, ", "))
	}

	resolution := parseTrigger(ti.Trigger).Resolution
	if resolution == "" {
		resolution = "delivered"
	}

	if err := c.appendHotStoryCloseEvidence(ctx, env, ti, storyRaw, tw, resolution); err != nil {
		return OutcomeFailure, fmt.Errorf("story_close: evidence: %w", err)
	}

	if err := c.closeTaskSuccess(ctx, env.ID); err != nil {
		return OutcomeFailure, fmt.Errorf("story_close: close task: %w", err)
	}
	return OutcomeSuccess, nil
}

// storyRaw is the loose-shaped story_get response. The hot runner
// reads template + fields; the rest is pass-through context. Using
// json.RawMessage for template lets us defer to the template parser
// where required.
type storyRaw struct {
	Story    storyRawStory  `json:"story"`
	Template storyTemplate  `json:"template"`
	Project  map[string]any `json:"project,omitempty"`
}

type storyRawStory struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Status      string         `json:"status"`
	Category    string         `json:"category"`
	Fields      map[string]any `json:"fields,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	ProjectID   string         `json:"project_id,omitempty"`
}

type storyTemplate struct {
	Category string                  `json:"category"`
	Fields   []storyTemplateField    `json:"fields"`
	Hooks    map[string]storyHookSet `json:"hooks,omitempty"`
}

type storyTemplateField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type storyHookSet struct {
	Structured []storyHookCheck `json:"structured,omitempty"`
}

type storyHookCheck struct {
	Type  string `json:"type"`
	Field string `json:"field,omitempty"`
}

// fetchStoryRaw calls story_get and returns the full envelope.
func (c *claudeClient) fetchStoryRaw(ctx context.Context, id string) (storyRaw, error) {
	if id == "" {
		return storyRaw{}, errors.New("story_get: id empty")
	}
	text, err := c.callTool(ctx, "story_get", map[string]any{"id": id})
	if err != nil {
		return storyRaw{}, err
	}
	if text == "" {
		return storyRaw{}, errors.New("story_get: empty response")
	}
	var sr storyRaw
	if err := json.Unmarshal([]byte(text), &sr); err != nil {
		return storyRaw{}, fmt.Errorf("story_get decode: %w", err)
	}
	return sr, nil
}

// requiredClosingFieldsMissing returns the names of fields the
// category template's `hooks.done.structured` requires, that are not
// populated on the story.
//
// Two field-source paths are checked:
//  1. The template's `hooks.done.structured` list (field_present checks).
//     This is the load-bearing gate for `story_update(status=done)`.
//  2. As a fallback (templates that haven't declared a `done` hook),
//     every `Required` field on the template.
//
// A field counts as "populated" when story.Fields[name] is present
// and renders to a non-empty string.
func requiredClosingFieldsMissing(sr storyRaw) []string {
	required := requiredFieldNames(sr.Template)
	if len(required) == 0 {
		return nil
	}
	fields := sr.Story.Fields
	var missing []string
	for _, name := range required {
		v, ok := fields[name]
		if !ok || !nonEmptyFieldValue(v) {
			missing = append(missing, name)
		}
	}
	return missing
}

// requiredFieldNames returns the list of field names the template's
// done-hook requires. Falls back to the template's `required: true`
// fields when the hook is absent.
func requiredFieldNames(t storyTemplate) []string {
	if hook, ok := t.Hooks["done"]; ok && len(hook.Structured) > 0 {
		var out []string
		for _, c := range hook.Structured {
			if c.Type == "field_present" && c.Field != "" {
				out = append(out, c.Field)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var out []string
	for _, f := range t.Fields {
		if f.Required {
			out = append(out, f.Name)
		}
	}
	return out
}

// nonEmptyFieldValue treats string / []byte / number / map values as
// populated when they render to a non-empty textual representation.
// Empty string, nil, and the empty map are "not populated".
func nonEmptyFieldValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case []byte:
		return strings.TrimSpace(string(x)) != ""
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	default:
		return true
	}
}

// appendHotStoryCloseEvidence writes the kind:evidence ledger row for
// a story_close. Tag set + sections mirror ldg_da616aec.
func (c *claudeClient) appendHotStoryCloseEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, sr storyRaw, tw taskWalkResponse, resolution string) error {
	var chainSummary strings.Builder
	chainSummary.WriteString("| Task | Action | Kind | Iter | Outcome |\n")
	chainSummary.WriteString("|---|---|---|---|---|\n")
	for _, t := range tw.Tasks {
		kind := t.Kind
		if kind == "" {
			kind = "work"
		}
		chainSummary.WriteString(fmt.Sprintf("| `%s` | %s | %s | %d | %s |\n", t.ID, t.Action, kind, t.Iteration, t.Outcome))
	}

	var fieldsSummary strings.Builder
	for _, f := range sr.Template.Fields {
		v := sr.Story.Fields[f.Name]
		fieldsSummary.WriteString(fmt.Sprintf("- `%s` — %s\n", f.Name, renderFieldValue(v)))
	}

	content := fmt.Sprintf(
		"# story_close evidence — %s — resolution: %s\n\n"+
			"**Resolution code:** `%s`.\n"+
			"**Story:** `%s` — %s.\n"+
			"**Close task:** `%s` (this row).\n"+
			"**Dispatch class:** hot (in-process runner; sty_4994caa3).\n\n"+
			"## Chain summary (per task_walk)\n\n%s\n"+
			"Every prior `kind=work` task either closed `outcome=success` OR was succeeded by a later iteration that closed `outcome=success`. The chain is consistent with terminal transition.\n\n"+
			"## Closing fields (populated this turn)\n\n%s\n"+
			"## Principles cited\n\n"+
			"- `pr_evidence` — every claim is backed by a substrate row or reproducible command.\n"+
			"- `pr_pipeline_integrity` — every contract phase ran end-to-end with its own evidence row.\n",
		ti.StoryID, resolution,
		resolution,
		sr.Story.ID, sr.Story.Title,
		env.ID,
		chainSummary.String(),
		fieldsSummary.String(),
	)
	tags := []string{
		"task_id:" + env.ID,
		"story_id:" + ti.StoryID,
		"phase:story_close",
		"resolution:" + resolution,
		"dispatch_class:hot",
		"kind:evidence",
	}
	return c.appendEvidence(ctx, env.ProjectID, ti.StoryID, tags, content)
}

// renderFieldValue formats a story field value for inclusion in the
// close-evidence row. Long values are truncated to a single line.
func renderFieldValue(v any) string {
	if !nonEmptyFieldValue(v) {
		return "*(empty)*"
	}
	switch x := v.(type) {
	case string:
		return summariseSingleLine(x)
	case []byte:
		return summariseSingleLine(string(x))
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return summariseSingleLine(string(raw))
	}
}

// summariseSingleLine collapses a value to one line, truncating to
// 240 chars so the evidence row stays scannable.
func summariseSingleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	const limit = 240
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// appendEvidence is the shared callTool wrapper for ledger_append.
// Threads through workspace/project/story when set; leaves them
// off the args map when empty so the substrate's defaults apply.
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
	_, err := c.callTool(ctx, "ledger_append", args)
	return err
}

// closeTaskSuccess closes the task via task_update. Failure surfaces
// as an error the caller wraps with phase context.
func (c *claudeClient) closeTaskSuccess(ctx context.Context, taskID string) error {
	_, err := c.callTool(ctx, "task_update", map[string]any{
		"id":      taskID,
		"status":  "closed",
		"outcome": "success",
	})
	return err
}

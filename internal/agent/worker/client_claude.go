// claudeClient is the satellites-agent worker.Client that spawns a
// real `claude` subprocess per claimed task — the production shape
// for sty_a6250f92 (order:03). It composes *placeholderClient for
// Claim / Close / Heartbeat / Shutdown delegation, and overrides
// Execute with the worktree + subprocess flow.
//
// Step shape (per AC):
//
//  1. Establish a worktree at <repo>/<worktree_root>/<task_id> on
//     the rendered branch template. Existing worktrees are reused.
//  2. Compose a thin-pointer prompt from task / agent / contract /
//     story metadata. The dispatched agent fetches the rich content
//     via per-verb retrieval (pr_substrate_provides_context).
//  3. Inherit the operator's environment unmodified. The dispatched
//     subprocess sees the operator's HOME and whatever auth the
//     operator configured for claude outside satellites-client.
//     Satellites does not read, copy, override, or synthesise any
//     part of the dispatched subprocess's environment.
//  4. Launch claude with bypassPermissions + strict-mcp-config + an
//     inline mcp-config naming only the satellites server. Stream
//     stdout/stderr to <worktree>/.satellites-agent.log.
//  5. Capture an evidence ledger row tagged kind:agent-execute-evidence.
//  6. Map exit code → Outcome (success | failure | timeout).
//  7. Leave the worktree in place on terminal outcome (forensic).

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/ternarybob/arbor"
)

// claudeClient overrides Execute on top of placeholderClient.
type claudeClient struct {
	*placeholderClient

	// gitRunner runs a git command in dir. Production uses exec.Command;
	// tests inject a recorder.
	gitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

	// stdoutTee / stderrTee mirror the dispatched claude subprocess's
	// stdout+stderr to additional writers when set (alongside the
	// per-task worktree log file). The daemon path leaves these nil —
	// log-file-only is the existing behaviour. The orchestrator-invoked
	// CLI path (sty_3e27a3f5: `satellites-client task run`) wires
	// os.Stdout / os.Stderr so the operator sees live progress.
	stdoutTee io.Writer
	stderrTee io.Writer
}

// NewClaudeClient constructs a worker.Client whose Execute spawns a
// real `claude --print` subprocess in a per-task git worktree. The
// subprocess inherits the operator's environment unmodified — claude
// is configured outside satellites-client. Claim / Close / Heartbeat /
// Shutdown delegate to the placeholder client (same MCP transport).
func NewClaudeClient(cfg config.AgentConfig, logger arbor.ILogger) Client {
	return &claudeClient{
		placeholderClient: newPlaceholderClient(cfg, logger),
		gitRunner:         runGit,
	}
}

// RunDispatched executes one task synchronously via a fresh claude
// subprocess in a per-task worktree, streaming the subprocess's
// stdout+stderr to the supplied writers in addition to the standard
// worktree log file. It is the orchestrator-invoked entry point used
// by `satellites-client task run <task_id>` (sty_3e27a3f5).
//
// The daemon's NewClaudeClient + Loop / Claim / Execute / Close cycle
// is unchanged — daemon callers never construct via RunDispatched and
// the streaming tees default nil.
//
// The caller owns the task lifecycle: RunDispatched does NOT Claim
// (the task_id is supplied directly) and does NOT Close (the
// dispatched claude session writes its own task_update via the
// substrate; the returned Outcome + exit-code-derived error let the
// caller surface the result).
func RunDispatched(ctx context.Context, cfg config.AgentConfig, logger arbor.ILogger, env TaskEnvelope, stdout, stderr io.Writer) (Outcome, error) {
	c := &claudeClient{
		placeholderClient: newPlaceholderClient(cfg, logger),
		gitRunner:         runGit,
		stdoutTee:         stdout,
		stderrTee:         stderr,
	}
	return c.Execute(ctx, env)
}

// taskInfo captures the subset of task_get response the orchestrator
// reads to compose the prompt. Fields the dispatched agent re-fetches
// (full body, agent capability list, contract rubric) are not
// represented here.
type taskInfo struct {
	ID          string `json:"id"`
	StoryID     string `json:"story_id"`
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	Action      string `json:"action"`
	Description string `json:"description"`
	// Trigger is the orchestrator-supplied JSON blob the hot-path
	// runners consult when present — push / merge honour `branch` +
	// `sha`, story_close honours `resolution`. Empty when the task
	// was minted without an explicit trigger payload (the chain-
	// inference path handles that case). sty_4994caa3.
	Trigger json.RawMessage `json:"trigger,omitempty"`
}

// agentInfo is the subset of agent_get the orchestrator reads.
type agentInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// contractInfo is the subset of contract_get the orchestrator reads.
// Structured is the decoded JSON of the document's frontmatter payload
// (category, evidence_required, validation_mode, dispatch_class …);
// the wire format base64-encodes []byte fields, so json.Unmarshal of
// the contract_get response transparently decodes the base64 into the
// raw JSON bytes here. sty_3b3e4e66.
type contractInfo struct {
	Name       string `json:"name"`
	Structured []byte `json:"structured"`
}

// dispatchClass returns the contract's dispatch_class frontmatter field.
// Returns "" when the contract has no structured payload or the field
// is absent; callers MUST treat "" as the "heavy" default so unmarked
// contracts continue to dispatch the existing claude subprocess.
func (c contractInfo) dispatchClass() string {
	if len(c.Structured) == 0 {
		return ""
	}
	var payload struct {
		DispatchClass string `json:"dispatch_class"`
	}
	if err := json.Unmarshal(c.Structured, &payload); err != nil {
		return ""
	}
	return payload.DispatchClass
}

// storyInfo is the subset of story_get the orchestrator reads.
type storyInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// promptInputs bundles the four MCP fetches used to populate the
// thin-pointer prompt template.
type promptInputs struct {
	Task     taskInfo
	Agent    agentInfo
	Contract contractInfo
	Story    storyInfo
	WorkDir  string
}

// promptTemplate is the single thin-pointer prompt template all
// dispatched agents receive. It tells the agent which row identifiers
// to fetch and which lifecycle close it owes; the substantive work
// instruction lives in the task body the agent fetches via task_get.
//
// Per cli-primary order:06 (sty_9aa8c2c2), the verb syntax is the
// `satellites-client <noun> <verb>` shape, not MCP-call form. The
// dispatched Claude has bin/satellites-client on PATH; it shells
// the substrate calls directly. order:07 deletes the legacy MCP
// catalogue entries the prompt no longer references.
const promptTemplate = `You are dispatched on satellites task {{task_id}}.

Role:      {{agent_name}}
Action:    {{action}}
Story:     {{story_id}}
Project:   {{project_id}}
Workspace: {{workspace_id}}

Read your context via the satellites CLI (auto-JSON when piped):
  satellites-client task get {{task_id}}                                 — your work instruction.
  satellites-client agent get --name {{agent_name}}                      — your role, voice, capability envelope.
  satellites-client contract get --name {{contract_name}} --project-id {{project_id}}
                                                                         — close-criteria rubric.
  satellites-client story get {{story_id}}                               — parent story body + references.
  satellites-client task walk --story-id {{story_id}}                    — chain + any prior_task_id verdict.
  satellites-client principle list --active-only --project-id {{project_id}}
                                                                         — principles in force.

Execute the task body's instructions. Write evidence rows via:
  satellites-client ledger append --project-id {{project_id}} --type evidence \
    --content "..." --tags task_id:{{task_id}},kind:evidence

Close your task with:
  satellites-client task update --id {{task_id}} --status closed --outcome success
  (or --outcome failure on a failed run)

Working directory: {{work_dir}}

Begin.
`

// composePrompt renders the thin-pointer prompt for the dispatched
// agent. Pure: takes the four MCP-fetch results + the runtime paths.
// Tested directly in client_claude_test.go.
func composePrompt(in promptInputs) string {
	r := strings.NewReplacer(
		"{{task_id}}", in.Task.ID,
		"{{agent_name}}", in.Agent.Name,
		"{{action}}", in.Task.Action,
		"{{story_id}}", in.Story.ID,
		"{{project_id}}", in.Task.ProjectID,
		"{{workspace_id}}", in.Task.WorkspaceID,
		"{{contract_name}}", in.Contract.Name,
		"{{work_dir}}", in.WorkDir,
	)
	return r.Replace(promptTemplate)
}

// fetchTaskInfo calls task_get(id) and decodes the row into taskInfo.
func (c *claudeClient) fetchTaskInfo(ctx context.Context, id string) (taskInfo, error) {
	text, err := c.callTool(ctx, "task_get", map[string]any{"id": id})
	if err != nil {
		return taskInfo{}, fmt.Errorf("task_get: %w", err)
	}
	if text == "" {
		return taskInfo{}, fmt.Errorf("task_get: empty response for %s", id)
	}
	var t taskInfo
	if err := json.Unmarshal([]byte(text), &t); err != nil {
		return taskInfo{}, fmt.Errorf("task_get decode: %w", err)
	}
	return t, nil
}

// fetchAgentInfo calls agent_get(id=...) and decodes the row.
// task.AgentID is the doc id (doc_<8hex>); agent_get accepts id.
func (c *claudeClient) fetchAgentInfo(ctx context.Context, id string) (agentInfo, error) {
	if id == "" {
		return agentInfo{}, errors.New("agent_get: empty id")
	}
	text, err := c.callTool(ctx, "agent_get", map[string]any{"id": id})
	if err != nil {
		return agentInfo{}, fmt.Errorf("agent_get: %w", err)
	}
	var a agentInfo
	if text != "" {
		_ = json.Unmarshal([]byte(text), &a)
	}
	if a.ID == "" {
		a.ID = id
	}
	return a, nil
}

// fetchContractInfo calls contract_get(name=action, project_id=...).
// task.Action is shaped contract:<name>; the leading prefix is stripped.
func (c *claudeClient) fetchContractInfo(ctx context.Context, action, projectID string) (contractInfo, error) {
	name := strings.TrimPrefix(action, "contract:")
	if name == "" {
		return contractInfo{Name: action}, nil
	}
	args := map[string]any{"name": name}
	if projectID != "" {
		args["project_id"] = projectID
	}
	text, err := c.callTool(ctx, "contract_get", args)
	if err != nil {
		// A free-form action (no contract row) is valid per the agent
		// model. Surface the configured name and continue.
		return contractInfo{Name: name}, nil
	}
	var co contractInfo
	if text != "" {
		_ = json.Unmarshal([]byte(text), &co)
	}
	if co.Name == "" {
		co.Name = name
	}
	return co, nil
}

// fetchStoryInfo calls story_get(id=...).
func (c *claudeClient) fetchStoryInfo(ctx context.Context, id string) (storyInfo, error) {
	if id == "" {
		return storyInfo{}, nil
	}
	text, err := c.callTool(ctx, "story_get", map[string]any{"id": id})
	if err != nil {
		return storyInfo{ID: id}, nil
	}
	var raw struct {
		Story storyInfo `json:"story"`
	}
	if text != "" {
		_ = json.Unmarshal([]byte(text), &raw)
	}
	if raw.Story.ID == "" {
		raw.Story.ID = id
	}
	return raw.Story, nil
}

// runGit executes `git` in dir. Returned bytes are the trimmed
// combined output for transport into evidence rows. Errors include
// the captured stderr so callers can surface git's complaint.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// resolveBaseSHA returns the short-SHA at HEAD of repoPath. Used by
// renderBranchName as the {base_sha} substitution input.
func (c *claudeClient) resolveBaseSHA(ctx context.Context, repoPath string) (string, error) {
	out, err := c.gitRunner(ctx, repoPath, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// renderBranchName fills in {task_id} and {base_sha} in the configured
// branch template. Pure helper for unit-testing.
func renderBranchName(template, taskID, baseSHA string) string {
	r := strings.NewReplacer(
		"{task_id}", taskID,
		"{base_sha}", baseSHA,
	)
	return r.Replace(template)
}

// resolveWorktreePath joins repo + worktree_root + task_id. The
// worktree_root may be absolute (preferred for production) or
// relative-to-repo. The result is always absolute.
func resolveWorktreePath(repoPath, worktreeRoot, taskID string) string {
	root := worktreeRoot
	if !filepath.IsAbs(root) {
		root = filepath.Join(repoPath, root)
	}
	return filepath.Join(root, taskID)
}

// ensureWorktree creates the per-task worktree if absent, or reuses
// it if present. Reuse policy: if the path exists and contains a .git
// marker (file or dir), it is treated as a previously-created worktree
// and reused without invoking `git worktree add`. This makes resumed
// claims idempotent.
func (c *claudeClient) ensureWorktree(ctx context.Context, task TaskEnvelope) (string, string, error) {
	cfg := c.cfg
	if cfg.RepoPath == "" {
		return "", "", errors.New("worktree: repo_path empty in agent config")
	}
	worktreePath := resolveWorktreePath(cfg.RepoPath, cfg.WorktreeRoot, task.ID)

	if existing, ok := worktreeExists(worktreePath); ok {
		return worktreePath, existing, nil
	}

	baseSHA, err := c.resolveBaseSHA(ctx, cfg.RepoPath)
	if err != nil {
		return "", "", fmt.Errorf("worktree: resolve base_sha: %w", err)
	}
	branchName := renderBranchName(cfg.BranchTemplate, task.ID, baseSHA)

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return "", "", fmt.Errorf("worktree: mkdir parent: %w", err)
	}
	if _, err := c.gitRunner(ctx, cfg.RepoPath, "worktree", "add", "-b", branchName, worktreePath, baseSHA); err != nil {
		return "", "", fmt.Errorf("worktree: git worktree add: %w", err)
	}
	return worktreePath, branchName, nil
}

// worktreeExists reports whether path looks like a previously-created
// git worktree. The marker is the .git file (or dir) inside path. The
// returned string is the branch name guess from .git, or empty when
// the marker is unparseable — the branch name is only used for log
// context, never to drive decisions.
func worktreeExists(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return "", false
	}
	gitMarker := filepath.Join(path, ".git")
	if _, err := os.Stat(gitMarker); err != nil {
		return "", false
	}
	return "", true
}

// buildMCPConfigJSON returns the JSON blob passed to `claude
// --mcp-config`. The dispatched agent sees only the satellites MCP
// server — strict-mcp-config + this single-server config means no
// operator-side MCP connections (vire, jcodemunch, drive) leak in.
func buildMCPConfigJSON(cfg config.AgentConfig) (string, error) {
	server := map[string]any{
		"type": "http",
		"url":  cfg.MCPURL,
	}
	if cfg.AuthToken != "" {
		server["headers"] = map[string]string{
			"Authorization": "Bearer " + cfg.AuthToken,
		}
	}
	out, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"satellites": server,
		},
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Execute spawns claude per the seven-step shape. Errors are surfaced
// as (OutcomeFailure, err); ctx cancellation maps to OutcomeTimeout.
//
// sty_3b3e4e66 (Layer A): the resolved contract's dispatch_class is
// fetched and logged on every dispatch. The in-process hot-path
// runner that consumes this field — bypassing the claude subprocess
// for "hot" contracts (push, merge_to_main, story_close) — lands in
// the Layer B+C follow-up. This commit ships only the data plumbing
// so contracts can declare their dispatch class without changing the
// runtime selector. Layer B will branch on ci.dispatchClass() here.
func (c *claudeClient) Execute(ctx context.Context, task TaskEnvelope) (Outcome, error) {
	// Step 1 + 2: fetch the four context bundles.
	ti, err := c.fetchTaskInfo(ctx, task.ID)
	if err != nil {
		return OutcomeFailure, err
	}
	ai, err := c.fetchAgentInfo(ctx, ti.AgentID)
	if err != nil {
		return OutcomeFailure, err
	}
	ci, _ := c.fetchContractInfo(ctx, ti.Action, ti.ProjectID)
	si, _ := c.fetchStoryInfo(ctx, ti.StoryID)

	if ti.WorkspaceID == "" {
		ti.WorkspaceID = task.WorkspaceID
	}
	if ti.ProjectID == "" {
		ti.ProjectID = task.ProjectID
	}

	// sty_4994caa3 Layer B — branch on the contract's dispatch_class.
	// "hot" contracts (push, merge_to_main, story_close) execute
	// in-process via runHotPath, skipping the seven-step claude
	// subprocess that the heavy path runs. errHotUnimplemented falls
	// back to the heavy path so a misclassified contract degrades
	// gracefully rather than failing the dispatch.
	dispatchClass := ci.dispatchClass()
	if c.logger != nil {
		c.logger.Debug().
			Str("task_id", task.ID).
			Str("contract", ci.Name).
			Str("dispatch_class", dispatchClass).
			Msg("worker dispatch resolved")
	}
	if dispatchClass == "hot" {
		outcome, err := c.runHotPath(ctx, task, ti, ai, ci, si)
		if err == nil || !errors.Is(err, errHotUnimplemented) {
			return outcome, err
		}
		if c.logger != nil {
			c.logger.Warn().
				Str("task_id", task.ID).
				Str("contract", ci.Name).
				Msg("hot-path runner missing — falling back to heavy claude subprocess")
		}
	}

	// Step 3: worktree.
	worktreePath, _, err := c.ensureWorktree(ctx, task)
	if err != nil {
		return OutcomeFailure, err
	}

	// Step 5: prompt. (Step 4 is empty by design — satellites inherits
	// the operator's environment unmodified; there is no satellites-
	// managed HOME for the subprocess. Claude is configured outside
	// satellites-client.)
	prompt := composePrompt(promptInputs{
		Task: ti, Agent: ai, Contract: ci, Story: si,
		WorkDir: worktreePath,
	})

	// Step 6: launch claude. Stream stdout+stderr to .satellites-
	// agent.log inside the worktree.
	mcpConfig, err := buildMCPConfigJSON(c.cfg)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("execute: mcp config: %w", err)
	}
	logPath := filepath.Join(worktreePath, ".satellites-agent.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return OutcomeFailure, fmt.Errorf("execute: open log: %w", err)
	}
	defer logFile.Close()

	binary := c.cfg.ClaudeBinaryPath
	if binary == "" {
		binary = "claude"
	}
	args := []string{
		"--permission-mode", "bypassPermissions",
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		"-p", prompt,
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = worktreePath
	// cmd.Env left at default — exec.CommandContext inherits the operator's
	// os.Environ() automatically. Satellites does not configure the
	// dispatched subprocess's environment.
	// sty_3e27a3f5: when the orchestrator-invoked RunDispatched path
	// sets stdoutTee/stderrTee, the subprocess output is mirrored live
	// to the operator's terminal. The worktree log file is always
	// written so forensic capture parity with the daemon path holds.
	var stdoutSink io.Writer = logFile
	var stderrSink io.Writer = logFile
	if c.stdoutTee != nil {
		stdoutSink = io.MultiWriter(logFile, c.stdoutTee)
	}
	if c.stderrTee != nil {
		stderrSink = io.MultiWriter(logFile, c.stderrTee)
	}
	cmd.Stdout = stdoutSink
	cmd.Stderr = stderrSink

	runErr := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// Step 7: evidence row + outcome.
	evidenceErr := c.appendExecuteEvidence(context.Background(), task, ti, prompt, exitCode, logPath)
	if evidenceErr != nil && c.logger != nil {
		c.logger.Warn().Str("task_id", task.ID).Str("error", evidenceErr.Error()).Msg("agent-execute-evidence row failed")
	}

	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return OutcomeTimeout, ctx.Err()
	}
	if runErr != nil {
		return OutcomeFailure, runErr
	}
	return OutcomeSuccess, nil
}

// appendExecuteEvidence writes the kind:agent-execute-evidence ledger
// row that records the orchestrator-side metadata for the dispatch:
// prompt sent (literal — small), exit code, log path. The dispatched
// agent itself writes the substantive evidence rows; this row is the
// orchestrator's frame.
func (c *claudeClient) appendExecuteEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, prompt string, exitCode int, logPath string) error {
	content := fmt.Sprintf(
		"# agent-execute-evidence\n\ntask=%s\nstory=%s\nagent_id=%s\naction=%s\nworktree=%s\nlog=%s\nexit_code=%d\n\n## prompt\n\n%s\n",
		env.ID, ti.StoryID, ti.AgentID, ti.Action,
		filepath.Dir(logPath), logPath, exitCode, prompt,
	)
	args := map[string]any{
		"project_id": env.ProjectID,
		"type":       "evidence",
		"tags": []string{
			"kind:agent-execute-evidence",
			"task_id:" + env.ID,
		},
		"content": content,
	}
	if ti.StoryID != "" {
		args["story_id"] = ti.StoryID
	}
	_, err := c.callTool(ctx, "ledger_append", args)
	return err
}

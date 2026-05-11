// claudeClient is the satellites-agent worker.Client that spawns a
// real `claude` subprocess per claimed task — the production shape
// for sty_a6250f92 (order:03). It composes *placeholderClient for
// Claim / Close / Heartbeat / Shutdown delegation, and overrides
// Execute with the seven-step worktree + subprocess flow.
//
// Step shape (per AC):
//
//  1. Establish a worktree at <repo>/<worktree_root>/<task_id> on
//     the rendered branch template. Existing worktrees are reused.
//  2. Compose a thin-pointer prompt from task / agent / contract /
//     story metadata. The dispatched agent fetches the rich content
//     via per-verb retrieval (pr_substrate_provides_context).
//  3. Cleanse HOME via mktemp -d + selective copy of .credentials.json
//     + settings.json. No projects/ → no operator memory inheritance.
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

	// findHome returns the directory the cleansed HOME copies
	// .credentials.json + settings.json from. Production reads
	// os.UserHomeDir(); tests inject a fixture dir.
	findHome func() (string, error)

	// gitRunner runs a git command in dir. Production uses exec.Command;
	// tests inject a recorder.
	gitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// NewClaudeClient constructs a worker.Client whose Execute spawns a
// real `claude --print` subprocess in a per-task git worktree with a
// cleansed HOME. Claim / Close / Heartbeat / Shutdown delegate to the
// placeholder client (same MCP transport).
func NewClaudeClient(cfg config.AgentConfig, logger arbor.ILogger) Client {
	return &claudeClient{
		placeholderClient: newPlaceholderClient(cfg, logger),
		findHome:          os.UserHomeDir,
		gitRunner:         runGit,
	}
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
	HomePath string
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
HOME:              {{home_path}}

The HOME has been cleansed: only auth artefacts (credentials, settings) are
present, never the operator's projects/ memory directory. This is the
substrate guarantee codified by pr_substrate_provides_context.

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
		"{{home_path}}", in.HomePath,
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

// cleanseHome materialises a fresh tmpdir HOME with only the auth
// artefacts the dispatched agent needs (.credentials.json + settings
// .json) and returns its path. The dir is created under os.TempDir()
// and the caller is responsible for removing it on Execute return.
//
// The forbidden surface is ~/.claude/projects/ — copying that would
// inherit the operator's Claude Code memory and violate
// pr_substrate_provides_context.
func (c *claudeClient) cleanseHome() (string, error) {
	srcHome, err := c.findHome()
	if err != nil {
		return "", fmt.Errorf("cleanse home: resolve src: %w", err)
	}
	tmpHome, err := os.MkdirTemp("", "satellites-agent-home-")
	if err != nil {
		return "", fmt.Errorf("cleanse home: mktemp: %w", err)
	}
	dotClaude := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(dotClaude, 0o700); err != nil {
		_ = os.RemoveAll(tmpHome)
		return "", fmt.Errorf("cleanse home: mkdir .claude: %w", err)
	}
	for _, name := range []string{".credentials.json", "settings.json"} {
		src := filepath.Join(srcHome, ".claude", name)
		dst := filepath.Join(dotClaude, name)
		if err := copyFileIfExists(src, dst); err != nil {
			_ = os.RemoveAll(tmpHome)
			return "", fmt.Errorf("cleanse home: copy %s: %w", name, err)
		}
	}
	return tmpHome, nil
}

// copyFileIfExists copies src to dst when src is a regular file. A
// missing src is not an error — settings.json is operator-optional.
func copyFileIfExists(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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

// findSessionJSONL walks tmpHome/.claude/projects/ and returns the path
// of the most-recently-modified .jsonl file, or "" when none exists.
// Used purely for evidence-row context — a missing session file is
// not an error.
func findSessionJSONL(tmpHome string) string {
	root := filepath.Join(tmpHome, ".claude", "projects")
	var newest string
	var newestMod int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		mod := info.ModTime().UnixNano()
		if mod > newestMod {
			newest = path
			newestMod = mod
		}
		return nil
	})
	return newest
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

	// sty_3b3e4e66 Layer A — log the contract's dispatch_class so
	// operator observability is in place before the Layer B selector
	// goes live. The log line is the only consumer of dispatchClass()
	// in this commit; the Execute path remains the heavy claude
	// subprocess for every contract regardless of dispatch_class.
	if c.logger != nil {
		c.logger.Debug().
			Str("task_id", task.ID).
			Str("contract", ci.Name).
			Str("dispatch_class", ci.dispatchClass()).
			Msg("worker dispatch resolved")
	}

	// Step 3: worktree.
	if ti.WorkspaceID == "" {
		ti.WorkspaceID = task.WorkspaceID
	}
	if ti.ProjectID == "" {
		ti.ProjectID = task.ProjectID
	}
	worktreePath, _, err := c.ensureWorktree(ctx, task)
	if err != nil {
		return OutcomeFailure, err
	}

	// Step 4: HOME.
	tmpHome, err := c.cleanseHome()
	if err != nil {
		return OutcomeFailure, err
	}
	defer os.RemoveAll(tmpHome)

	// Step 5: prompt.
	prompt := composePrompt(promptInputs{
		Task: ti, Agent: ai, Contract: ci, Story: si,
		HomePath: tmpHome, WorkDir: worktreePath,
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
	cmd.Env = append(os.Environ(), "HOME="+tmpHome)
	// Replace any pre-existing HOME=... entry so the cleansed value
	// wins on every platform's exec lookup.
	cmd.Env = enforceHomeEnv(cmd.Env, tmpHome)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// Step 7: evidence row + outcome.
	sessionPath := findSessionJSONL(tmpHome)
	evidenceErr := c.appendExecuteEvidence(context.Background(), task, ti, tmpHome, prompt, exitCode, logPath, sessionPath)
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

// enforceHomeEnv guarantees HOME=tmpHome is the last (winning) entry
// when the inherited environment already contains a HOME assignment.
// Without this, exec.Cmd's env resolution can pick up the operator's
// HOME on platforms whose libc reads the first match.
func enforceHomeEnv(env []string, tmpHome string) []string {
	out := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "HOME="+tmpHome)
	return out
}

// appendExecuteEvidence writes the kind:agent-execute-evidence ledger
// row that records the orchestrator-side metadata for the dispatch:
// HOME path, prompt sent (literal — small), exit code, log path,
// session JSONL path. The dispatched agent itself writes the
// substantive evidence rows; this row is the orchestrator's frame.
func (c *claudeClient) appendExecuteEvidence(ctx context.Context, env TaskEnvelope, ti taskInfo, tmpHome, prompt string, exitCode int, logPath, sessionPath string) error {
	content := fmt.Sprintf(
		"# agent-execute-evidence\n\ntask=%s\nstory=%s\nagent_id=%s\naction=%s\nhome=%s\nworktree=%s\nlog=%s\nsession_jsonl=%s\nexit_code=%d\n\n## prompt\n\n%s\n",
		env.ID, ti.StoryID, ti.AgentID, ti.Action, tmpHome,
		filepath.Dir(logPath), logPath, sessionPath, exitCode, prompt,
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

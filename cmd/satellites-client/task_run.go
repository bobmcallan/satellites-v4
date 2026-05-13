package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/config"
)

// newTaskRunCmd registers `task run <task_id>` — the orchestrator-invoked
// dispatch entry point shipped by sty_3e27a3f5. Resolves the task via
// task_get, builds an AgentConfig from the resolved client TOML +
// credentials, spawns a fresh claude subprocess in a per-task worktree
// (the same execute flow the satellites-agent daemon uses), and streams
// the subprocess's stdout + stderr live to the operator's terminal.
//
// The orchestrator owns the subprocess lifecycle: task run does NOT
// claim the task (no task_claim_by_id verb today — operators run with
// the daemon stopped to avoid claim races, pending a follow-up). The
// dispatched claude session writes its own task_update on completion
// via the substrate.
func newTaskRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <task_id>",
		Short: "Dispatch a task synchronously: spawn a fresh claude subprocess, stream output live, return when the agent finishes.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTaskCmd,
	}
}

// runTaskCmd is the verb body. Returns a cliexit-wrapped error on
// failure; nil on OutcomeSuccess.
func runTaskCmd(cmd *cobra.Command, args []string) error {
	taskID := args[0]

	raw, err := callRemote(cmd, "task_get", map[string]any{"id": taskID})
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: task_get: %w", taskID, err))
	}
	var t struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		ProjectID   string `json:"project_id"`
		Origin      string `json:"origin"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: decode task_get: %w", taskID, err))
	}
	if t.ID == "" {
		return cliexit.Newf(cliexit.NotFound, "task run: task %s not found", taskID)
	}

	cfg := config.AgentConfig{
		RepoPath:         resolvedClientConfig.RepoPath,
		BranchTemplate:   resolvedClientConfig.BranchTemplate,
		WorktreeRoot:     resolvedClientConfig.WorktreeRoot,
		ClaudeBinaryPath: "claude",
		// SpawnMCPURL is the MCP endpoint the dispatched claude
		// subprocess connects to via its per-worktree MCP config.
		// effectiveServer() returns the base server URL (e.g.
		// https://satellites-pprod.fly.dev); the dispatched claude
		// expects the full /mcp endpoint.
		SpawnMCPURL:      effectiveServer() + "/mcp",
		AuthToken:        resolvedToken,
		ExecuteTimeout:   resolvedClientConfig.ExecuteTimeout,
		LogLevel:         resolvedClientConfig.LogLevel,
		ClientConfigPath: resolvedClientConfig.LoadedTOMLPath(),
	}
	if cfg.ExecuteTimeout == 0 {
		cfg.ExecuteTimeout = 30 * time.Minute
	}

	// Use the shared logger constructed in PersistentPreRunE
	// (sty_ef4eedaa) so task run shares the satellites-agent-shaped
	// console + bin/logs/ output. Falls back to a console-only logger
	// when called outside the cobra root (e.g. from tests that bypass
	// PersistentPreRunE).
	logger := resolvedLogger
	if logger == nil {
		logger = satarbor.New(cfg.LogLevel)
	}

	// Resolve the shared /api/v1 HTTP client. ensureRemote constructs
	// (or returns the cached) cliremote.Client built around the same
	// effectiveServer + bearer pair the CLI's own verb calls use, so
	// the dispatcher's pre-spawn fetches + the
	// kind:agent-execute-evidence row ride the same wire path as
	// `satellites-client task get` etc. (pr_mcp_cli_shared_path).
	api, err := ensureRemote()
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: api client: %w", taskID, err))
	}

	env := worker.TaskEnvelope{
		ID:          taskID,
		WorkspaceID: t.WorkspaceID,
		ProjectID:   t.ProjectID,
		Origin:      t.Origin,
	}

	fmt.Fprintf(os.Stderr, "satellites-client task run: dispatching %s (workspace=%s project=%s)\n",
		taskID, t.WorkspaceID, t.ProjectID)
	outcome, runErr := worker.RunDispatched(cmd.Context(), cfg, logger, api, env, os.Stdout, os.Stderr)
	fmt.Fprintf(os.Stderr, "\nsatellites-client task run: outcome=%s\n", outcome)
	if runErr != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: %w", taskID, runErr))
	}
	if outcome != worker.OutcomeSuccess {
		return cliexit.Newf(cliexit.Server, "task run %s: outcome=%s", taskID, outcome)
	}
	return nil
}

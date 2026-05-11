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
		MCPURL:           effectiveServer(),
		AuthToken:        resolvedToken,
		ExecuteTimeout:   resolvedClientConfig.ExecuteTimeout,
		LogLevel:         resolvedClientConfig.LogLevel,
	}
	if cfg.ExecuteTimeout == 0 {
		cfg.ExecuteTimeout = 30 * time.Minute
	}

	logger := satarbor.New(cfg.LogLevel)

	env := worker.TaskEnvelope{
		ID:          taskID,
		WorkspaceID: t.WorkspaceID,
		ProjectID:   t.ProjectID,
		Origin:      t.Origin,
	}

	fmt.Fprintf(os.Stderr, "satellites-client task run: dispatching %s (workspace=%s project=%s)\n",
		taskID, t.WorkspaceID, t.ProjectID)
	outcome, runErr := worker.RunDispatched(cmd.Context(), cfg, logger, env, os.Stdout, os.Stderr)
	fmt.Fprintf(os.Stderr, "\nsatellites-client task run: outcome=%s\n", outcome)
	if runErr != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: %w", taskID, runErr))
	}
	if outcome != worker.OutcomeSuccess {
		return cliexit.Newf(cliexit.Server, "task run %s: outcome=%s", taskID, outcome)
	}
	return nil
}

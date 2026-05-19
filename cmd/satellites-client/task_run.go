package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/agent/dispatchteam"
	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/clientdaemon"
	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/config"
)

// telemetryEnvSkip names the env-var the parent task_run sets in the
// dispatched subprocess so any nested `satellites-client task run`
// inside that subprocess skips its own lifecycle / chunk telemetry.
// The string itself lives in internal/agent/dispatchteam — both the
// CLI's set-on-subprocess branch and the daemon's worker reference
// the same constant. sty_8c17b89d / sty_5aa20f1b.
const telemetryEnvSkip = dispatchteam.TelemetryEnvSkip

// heartbeatInterval is the default cadence for the lifecycle
// `heartbeat` event. Configurable via `--heartbeat`.
const heartbeatInterval = dispatchteam.DefaultHeartbeatInterval

// newTaskRunCmd registers `task run <task_id>` — the orchestrator-
// invoked dispatch entry point shipped by sty_3e27a3f5. Resolves the
// task via task_get, builds an AgentConfig from the resolved client
// TOML + credentials, spawns a fresh claude subprocess in a per-task
// worktree, and streams the subprocess's stdout + stderr live to the
// operator's terminal AND to the task_log substrate (sty_8c17b89d).
// The lifecycle telemetry shell lives in internal/agent/dispatchteam
// so the daemon (sty_5aa20f1b) and this sync path emit identical
// frames.
func newTaskRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run <task_id>",
		Short: "Dispatch a task asynchronously by default (push-enqueue into the local serve daemon, exit within ~1s); --sync re-opts into the blocking shape that spawns a fresh claude subprocess in this process and streams output live.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTaskCmd,
	}
	c.Flags().Duration("heartbeat", heartbeatInterval, "Lifecycle heartbeat cadence (--sync mode only).")
	c.Flags().Bool("follow", false, "Subscribe to the task_log SSE stream and print lifecycle markers to stdout (--sync mode only — async dispatch surfaces lifecycle via task_walk/ledger_list polling instead).")
	c.Flags().Bool("sync", false, "Block on the dispatched subprocess: spawn claude in-process, stream stdout live, return when the agent finishes. Default is push-enqueue into the local serve daemon and return within ~1s.")
	c.Flags().String("socket", clientdaemon.DefaultSocketPath(), "Daemon unix socket (defaults to the daemon's canonical path; used by the default async path).")
	return c
}

// runTaskCmd is the verb body. Returns a cliexit-wrapped error on
// failure; nil on OutcomeSuccess.
func runTaskCmd(cmd *cobra.Command, args []string) error {
	taskID := args[0]

	if sync, _ := cmd.Flags().GetBool("sync"); !sync {
		return runTaskAsync(cmd, taskID)
	}

	disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"

	raw, err := callRemote(cmd, "task_get", map[string]any{"id": taskID})
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: task_get: %w", taskID, err))
	}
	var t struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		ProjectID   string `json:"project_id"`
		StoryID     string `json:"story_id"`
		Origin      string `json:"origin"`
		AgentID     string `json:"agent_id"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: decode task_get: %w", taskID, err))
	}
	if t.ID == "" {
		return cliexit.Newf(cliexit.NotFound, "task run: task %s not found", taskID)
	}

	// sty_056b68f6: mint a task-scoped agent api-key for the
	// dispatched subprocess. The server clamps allowed_verbs to the
	// task agent's pr_role_grid default; the response carries the
	// resolved list + cleartext key (returned ONCE). On any mint
	// failure (e.g. the server is older than sty_056b68f6 and rejects
	// `task_id`), fall back to the orchestrator's project-scoped
	// bearer — server enforcement is still in place if the server is
	// up-to-date, and the legacy path stays callable for older binaries
	// until the substrate is universally upgraded.
	taskToken := resolvedToken
	var taskAllowedVerbs []string
	if mintRaw, mintErr := callRemote(cmd, "agent_apikey_create", map[string]any{
		"name":       "run:" + taskID,
		"project_id": t.ProjectID,
		"task_id":    taskID,
	}); mintErr == nil {
		var mintOut struct {
			Key          string   `json:"key"`
			AllowedVerbs []string `json:"allowed_verbs"`
		}
		if err := json.Unmarshal(mintRaw, &mintOut); err == nil && mintOut.Key != "" {
			taskToken = mintOut.Key
			taskAllowedVerbs = mintOut.AllowedVerbs
		}
	}

	cfg := config.AgentConfig{
		RepoPath:         resolvedClientConfig.RepoPath,
		BranchTemplate:   resolvedClientConfig.BranchTemplate,
		WorktreeRoot:     resolvedClientConfig.WorktreeRoot,
		ClaudeBinaryPath: "claude",
		SpawnMCPURL:      effectiveServer() + "/mcp",
		AuthToken:        taskToken,
		AllowedVerbs:     taskAllowedVerbs,
		ExecuteTimeout:   resolvedClientConfig.ExecuteTimeout,
		LogLevel:         resolvedClientConfig.LogLevel,
		ClientConfigPath: resolvedClientConfig.LoadedTOMLPath(),
	}
	if cfg.ExecuteTimeout == 0 {
		cfg.ExecuteTimeout = 30 * time.Minute
	}

	logger := resolvedLogger
	if logger == nil {
		logger = satarbor.New(cfg.LogLevel)
	}

	api, err := ensureRemote()
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: api client: %w", taskID, err))
	}

	followEnabled, _ := cmd.Flags().GetBool("follow")
	var (
		followCancel func()
		followDone   chan struct{}
	)
	if followEnabled {
		if disableTelemetry {
			if logger != nil {
				logger.Warn().
					Str("task_id", taskID).
					Msg("task run --follow: SATELLITES_CLIENT_DISABLE_TELEMETRY=1; parent emits no telemetry, skipping SSE subscribe")
			}
		} else {
			fctx, fcancel := context.WithCancel(cmd.Context())
			followCancel = fcancel
			followDone = make(chan struct{})
			go func() {
				defer close(followDone)
				_ = followTaskLog(fctx, followerConfig{
					serverURL: effectiveServer(),
					authToken: resolvedToken,
					taskID:    taskID,
					out:       os.Stdout,
					logger:    logger,
				})
			}()
		}
	}

	env := worker.TaskEnvelope{
		ID:          taskID,
		WorkspaceID: t.WorkspaceID,
		ProjectID:   t.ProjectID,
		Origin:      t.Origin,
	}

	fmt.Fprintf(os.Stderr, "satellites-client task run: dispatching %s (workspace=%s project=%s)\n",
		taskID, t.WorkspaceID, t.ProjectID)

	// sty_64e69db8 drift-check guard: propagate to subprocess.
	_ = os.Setenv(driftEnvSkip, "1")

	// sty_8c17b89d telemetry: dispatchteam.Run wraps start/heartbeat/stop
	// + chunk uploaders around the worker.RunDispatched invocation. The
	// parent sets SATELLITES_CLIENT_DISABLE_TELEMETRY=1 so any nested
	// task run inside the dispatched subprocess passes through.
	if !disableTelemetry {
		_ = os.Setenv(telemetryEnvSkip, "1")
	}
	hbInterval, _ := cmd.Flags().GetDuration("heartbeat")
	if hbInterval <= 0 {
		hbInterval = heartbeatInterval
	}

	in := dispatchteam.Inputs{
		TaskID:           taskID,
		WorkspaceID:      t.WorkspaceID,
		ProjectID:        t.ProjectID,
		StoryID:          t.StoryID,
		Origin:           t.Origin,
		API:              api,
		Logger:           logger,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		Heartbeat:        hbInterval,
		DisableTelemetry: disableTelemetry,
	}
	outcome, runErr := dispatchteam.Run(cmd.Context(), in, func(ctx context.Context, stdout, stderr io.Writer) (worker.Outcome, error) {
		return worker.RunDispatched(ctx, cfg, logger, api, env, stdout, stderr)
	})

	if followCancel != nil {
		select {
		case <-followDone:
		case <-time.After(500 * time.Millisecond):
		}
		followCancel()
		<-followDone
	}

	fmt.Fprintf(os.Stderr, "\nsatellites-client task run: outcome=%s\n", outcome)
	if runErr != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: %w", taskID, runErr))
	}
	if outcome != worker.OutcomeSuccess {
		return cliexit.Newf(cliexit.Server, "task run %s: outcome=%s", taskID, outcome)
	}
	return nil
}

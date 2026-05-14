package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// telemetryEnvSkip is the env-var the parent task_run sets in the
// dispatched claude subprocess so any nested `satellites-client task
// run` invocation inside the spawned subprocess skips its own
// lifecycle / chunk telemetry. Mirrors the driftEnvSkip pattern
// shipped by sty_64e69db8 — same os.Setenv mechanism, same goal:
// gate against self-dispatch loops in subprocesses that re-enter the
// CLI. sty_8c17b89d.
const telemetryEnvSkip = "SATELLITES_CLIENT_DISABLE_TELEMETRY"

// heartbeatInterval is the default cadence for the lifecycle
// `heartbeat` event. Configurable via `--heartbeat`.
const heartbeatInterval = 10 * time.Second

// newTaskRunCmd registers `task run <task_id>` — the orchestrator-
// invoked dispatch entry point shipped by sty_3e27a3f5. Resolves the
// task via task_get, builds an AgentConfig from the resolved client
// TOML + credentials, spawns a fresh claude subprocess in a per-task
// worktree, and streams the subprocess's stdout + stderr live to the
// operator's terminal AND to the task_log substrate (sty_8c17b89d).
func newTaskRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run <task_id>",
		Short: "Dispatch a task synchronously: spawn a fresh claude subprocess, stream output live, return when the agent finishes.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTaskCmd,
	}
	c.Flags().Duration("heartbeat", heartbeatInterval, "Lifecycle heartbeat cadence.")
	c.Flags().Bool("follow", false, "Subscribe to the task_log SSE stream and print lifecycle markers to stdout.")
	return c
}

// runTaskCmd is the verb body. Returns a cliexit-wrapped error on
// failure; nil on OutcomeSuccess.
func runTaskCmd(cmd *cobra.Command, args []string) error {
	taskID := args[0]

	// sty_8c17b89d recursion guard: the dispatched claude subprocess
	// re-invokes `satellites-client`. Inheriting telemetryEnvSkip from
	// the parent means the nested `task run` (if any) is a pass-through
	// — it dispatches the inner task without emitting its own
	// lifecycle/chunk frames against the parent's task_log.
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
		SpawnMCPURL:      effectiveServer() + "/mcp",
		AuthToken:        resolvedToken,
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

	// sty_7fc607f5: opt-in --follow subscribes to the task_log SSE
	// stream and prints lifecycle markers (start / heartbeat / stop)
	// to stdout as they arrive. When SATELLITES_CLIENT_DISABLE_TELEMETRY=1
	// is set the parent emits no telemetry, so following would print
	// nothing — preflight-skip with a warning per plan §9 / AC9.
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

	// sty_8c17b89d telemetry: the parent run emits start/heartbeat/stop
	// and tees stdout+stderr through a chunk uploader. The dispatched
	// subprocess inherits SATELLITES_CLIENT_DISABLE_TELEMETRY=1 so a
	// nested task run (if any) stays in the pass-through branch.
	stdoutSink := io.Writer(os.Stdout)
	stderrSink := io.Writer(os.Stderr)
	var (
		seqCounter      atomic.Int64
		stdoutUploader  *taskLogUploader
		stderrUploader  *taskLogUploader
		heartbeatStop   chan struct{}
		heartbeatDone   chan struct{}
		startedAt       time.Time
		hbInterval      time.Duration
		runDisabled     = disableTelemetry
	)
	if !disableTelemetry {
		// Reserve seq=0 for the start event before any chunk uploader
		// can claim a seq; nextSeq() returns 1 first.
		startSeq := seqCounter.Add(1) - 1
		startedAt = time.Now().UTC()
		_ = os.Setenv(telemetryEnvSkip, "1")
		hbInterval, _ = cmd.Flags().GetDuration("heartbeat")
		if hbInterval <= 0 {
			hbInterval = heartbeatInterval
		}

		// Emit start event.
		emitLifecycle(cmd.Context(), api, logger, taskID, t.WorkspaceID, t.ProjectID, startSeq, "start", map[string]any{
			"worker_pid":   os.Getpid(),
			"workspace_id": t.WorkspaceID,
			"project_id":   t.ProjectID,
			"story_id":     t.StoryID,
			"origin":       t.Origin,
		})

		// Chunk uploaders for stdout + stderr. nextSeq returns the
		// post-increment value MINUS one so the first call yields the
		// next available slot — single shared counter across kinds.
		nextSeq := func() int64 { return seqCounter.Add(1) - 1 }
		stdoutUploader = newTaskLogUploader(api, logger, taskID, t.WorkspaceID, t.ProjectID, "stdout", nextSeq)
		stderrUploader = newTaskLogUploader(api, logger, taskID, t.WorkspaceID, t.ProjectID, "stderr", nextSeq)
		stdoutSink = io.MultiWriter(os.Stdout, stdoutUploader)
		stderrSink = io.MultiWriter(os.Stderr, stderrUploader)

		// Heartbeat goroutine.
		heartbeatStop = make(chan struct{})
		heartbeatDone = make(chan struct{})
		go runHeartbeat(api, logger, taskID, t.WorkspaceID, t.ProjectID, &seqCounter, startedAt, hbInterval, heartbeatStop, heartbeatDone)
	}

	outcome, runErr := worker.RunDispatched(cmd.Context(), cfg, logger, api, env, stdoutSink, stderrSink)

	if !runDisabled {
		// Stop heartbeat first so it doesn't race with stop emission.
		close(heartbeatStop)
		<-heartbeatDone

		stdoutUploader.Close()
		stderrUploader.Close()

		stopSeq := seqCounter.Add(1) - 1
		exitCode := 0
		if runErr != nil {
			exitCode = 1
		}
		emitLifecycle(context.Background(), api, logger, taskID, t.WorkspaceID, t.ProjectID, stopSeq, "stop", map[string]any{
			"outcome":     string(outcome),
			"exit_code":   exitCode,
			"duration_ms": time.Since(startedAt).Milliseconds(),
		})

		// Single ledger pointer row anchoring the task_log range.
		chunksStdout, bytesStdout := stdoutUploader.stats()
		chunksStderr, bytesStderr := stderrUploader.stats()
		totalChunks := chunksStdout + chunksStderr + 2 // start + stop
		appendTaskLogPointer(context.Background(), api, logger, taskID, t.ProjectID, t.StoryID, totalChunks, bytesStdout+bytesStderr)
	}

	// sty_7fc607f5: stop the follower (if running) before printing the
	// final outcome line. The stop frame normally ends the goroutine
	// already; the cancel is the catch-all when the run errored
	// before lifecycle finalisation (e.g. RunDispatched returned
	// before emit-stop completed).
	if followCancel != nil {
		// Give the follower a brief window to drain the stop frame
		// it emitted above before cancelling its context.
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

// emitLifecycle issues one task_log_append for a lifecycle event.
// Errors log at warn and never abort the dispatched task.
func emitLifecycle(ctx context.Context, api *cliremote.Client, logger arbor.ILogger, taskID, wsID, projID string, seq int64, kind string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		if logger != nil {
			logger.Warn().Str("error", err.Error()).Str("kind", kind).Msg("task_log lifecycle payload marshal failed")
		}
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in := cliremote.TaskLogAppendInput{
		TaskID:      taskID,
		WorkspaceID: wsID,
		ProjectID:   projID,
		Seq:         seq,
		TS:          time.Now().UTC(),
		Kind:        kind,
		Payload:     body,
	}
	if _, err := api.TaskLogAppend(cctx, in); err != nil && logger != nil {
		logger.Warn().
			Str("task_id", taskID).
			Str("kind", kind).
			Int64("seq", seq).
			Str("error", err.Error()).
			Msg("task_log lifecycle append failed")
	}
}

// runHeartbeat fires heartbeat events every interval until stopCh
// closes. Signals done so the parent can sync before emitting stop.
func runHeartbeat(api *cliremote.Client, logger arbor.ILogger, taskID, wsID, projID string, seqCounter *atomic.Int64, startedAt time.Time, interval time.Duration, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-t.C:
			seq := seqCounter.Add(1) - 1
			payload, _ := json.Marshal(map[string]any{
				"elapsed_ms": time.Since(startedAt).Milliseconds(),
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = api.TaskLogAppend(ctx, cliremote.TaskLogAppendInput{
				TaskID:      taskID,
				WorkspaceID: wsID,
				ProjectID:   projID,
				Seq:         seq,
				TS:          time.Now().UTC(),
				Kind:        "heartbeat",
				Payload:     payload,
			})
			cancel()
		}
	}
}

// appendTaskLogPointer writes the single kind:task-log ledger row that
// anchors the task_log chunks to the audit chain (AC5).
func appendTaskLogPointer(ctx context.Context, api *cliremote.Client, logger arbor.ILogger, taskID, projectID, storyID string, totalChunks int, byteCount int64) {
	structured, err := json.Marshal(map[string]any{
		"task_log_id":  taskID, // task id anchors the (task_id, seq) replay key
		"total_chunks": totalChunks,
		"byte_count":   byteCount,
	})
	if err != nil {
		return
	}
	args := map[string]any{
		"project_id": projectID,
		"type":       "evidence",
		"tags": []string{
			"task_id:" + taskID,
			"kind:task-log",
		},
		"content":    fmt.Sprintf("task-log: task=%s chunks=%d bytes=%d", taskID, totalChunks, byteCount),
		"structured": json.RawMessage(structured),
	}
	if storyID != "" {
		args["story_id"] = storyID
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = api.Call(cctx, "ledger_append", args, nil)
}

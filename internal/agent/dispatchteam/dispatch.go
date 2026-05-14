// Package dispatchteam wraps a single satellites-task dispatch with
// the lifecycle telemetry shell — start / heartbeat / stop frames plus
// stdout+stderr chunk uploaders — so both the synchronous CLI path
// (`cmd/satellites-client task run`) and the long-running daemon
// goroutine (`internal/clientdaemon`) emit identical-shape frames.
//
// The package is dispatch plumbing only. It does not own subprocess
// spawning, claude-binary resolution, worktree setup, or any LLM
// behaviour — those stay inside `internal/agent/worker.RunDispatched`.
// Substrate calls travel through `*cliremote.Client` so this package
// remains layering-clean (pr_mcp_cli_shared_path).
//
// sty_5aa20f1b extracted these primitives from
// cmd/satellites-client/task_run.go where they previously lived as
// package-local helpers.
package dispatchteam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// TelemetryEnvSkip is the env-var the parent task_run sets in the
// dispatched claude subprocess so any nested `satellites-client task
// run` inside the spawned subprocess skips its own lifecycle / chunk
// telemetry. Mirrors driftEnvSkip — same os.Setenv mechanism, same
// goal: gate against self-dispatch loops in subprocesses that re-enter
// the CLI. sty_8c17b89d.
const TelemetryEnvSkip = "SATELLITES_CLIENT_DISABLE_TELEMETRY"

// DefaultHeartbeatInterval is the default cadence for the lifecycle
// `heartbeat` event. Callers may override via Inputs.Heartbeat.
const DefaultHeartbeatInterval = 10 * time.Second

// Inputs is the per-run configuration the caller supplies to Run.
type Inputs struct {
	TaskID      string
	WorkspaceID string
	ProjectID   string
	StoryID     string
	Origin      string

	API    *cliremote.Client
	Logger arbor.ILogger

	// Stdout / Stderr are the local-side sinks (operator terminal for
	// the CLI; per-task log file for the daemon). The chunk uploader
	// tees through MultiWriter to these PLUS the substrate task_log.
	Stdout io.Writer
	Stderr io.Writer

	// Heartbeat is the cadence for the heartbeat ticker; <=0 falls
	// back to DefaultHeartbeatInterval.
	Heartbeat time.Duration

	// DisableTelemetry skips ALL emission (start / heartbeat / stop
	// frames + chunk uploaders + ledger pointer). The dispatch
	// closure still runs with the supplied Stdout / Stderr writers.
	// Used by the recursion-guard branch when the parent task_run
	// has set TelemetryEnvSkip=1 on the subprocess.
	DisableTelemetry bool

	// StopExtra is merged into the stop event JSON payload before
	// emission. Used by the daemon to set {"daemon_initiated":true}
	// on drain-initiated stops; nil for the CLI path.
	StopExtra map[string]any

	// SuppressStop, if non-nil and observed true at finalisation,
	// causes Run to skip emitting its own stop frame and ledger
	// pointer — the caller is responsible for the stop emission.
	// The daemon's drain path uses this so its daemon-initiated
	// stop frame is the only stop row written for the task. nil
	// for the CLI path.
	SuppressStop *atomic.Bool
}

// Run wires the lifecycle shell (start + heartbeat + chunk uploaders +
// stop + ledger pointer) around the supplied dispatch closure. The
// closure receives the wrapped stdout/stderr writers and is expected
// to invoke `worker.RunDispatched` (or an equivalent that respects
// the io.Writer contract). Returns the closure's outcome + error
// verbatim — the lifecycle wrapper never converts a runtime error
// into a different error.
func Run(ctx context.Context, in Inputs, dispatch func(ctx context.Context, stdout, stderr io.Writer) (worker.Outcome, error)) (worker.Outcome, error) {
	if dispatch == nil {
		return worker.OutcomeFailure, fmt.Errorf("dispatchteam: dispatch closure is nil")
	}

	if in.DisableTelemetry {
		return dispatch(ctx, in.Stdout, in.Stderr)
	}

	hb := in.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeatInterval
	}

	var seqCounter atomic.Int64
	startSeq := seqCounter.Add(1) - 1
	startedAt := time.Now().UTC()

	EmitLifecycle(ctx, in.API, in.Logger, in.TaskID, in.WorkspaceID, in.ProjectID, startSeq, "start", map[string]any{
		"worker_pid":   os.Getpid(),
		"workspace_id": in.WorkspaceID,
		"project_id":   in.ProjectID,
		"story_id":     in.StoryID,
		"origin":       in.Origin,
	})

	nextSeq := func() int64 { return seqCounter.Add(1) - 1 }
	stdoutUploader := NewTaskLogUploader(in.API, in.Logger, in.TaskID, in.WorkspaceID, in.ProjectID, "stdout", nextSeq)
	stderrUploader := NewTaskLogUploader(in.API, in.Logger, in.TaskID, in.WorkspaceID, in.ProjectID, "stderr", nextSeq)
	stdoutSink := io.MultiWriter(in.Stdout, stdoutUploader)
	stderrSink := io.MultiWriter(in.Stderr, stderrUploader)

	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go RunHeartbeat(in.API, in.Logger, in.TaskID, in.WorkspaceID, in.ProjectID, &seqCounter, startedAt, hb, hbStop, hbDone)

	outcome, runErr := dispatch(ctx, stdoutSink, stderrSink)

	close(hbStop)
	<-hbDone

	stdoutUploader.Close()
	stderrUploader.Close()

	if in.SuppressStop != nil && in.SuppressStop.Load() {
		return outcome, runErr
	}

	stopSeq := seqCounter.Add(1) - 1
	exitCode := 0
	if runErr != nil {
		exitCode = 1
	}
	stopPayload := map[string]any{
		"outcome":     string(outcome),
		"exit_code":   exitCode,
		"duration_ms": time.Since(startedAt).Milliseconds(),
	}
	for k, v := range in.StopExtra {
		stopPayload[k] = v
	}
	// Use a fresh background context: the caller's ctx may already
	// have been cancelled by the time we land here (drain / SIGTERM).
	EmitLifecycle(context.Background(), in.API, in.Logger, in.TaskID, in.WorkspaceID, in.ProjectID, stopSeq, "stop", stopPayload)

	chunksStdout, bytesStdout := stdoutUploader.Stats()
	chunksStderr, bytesStderr := stderrUploader.Stats()
	totalChunks := chunksStdout + chunksStderr + 2 // start + stop
	AppendTaskLogPointer(context.Background(), in.API, in.Logger, in.TaskID, in.ProjectID, in.StoryID, totalChunks, bytesStdout+bytesStderr)

	return outcome, runErr
}

// EmitLifecycle issues one task_log_append for a lifecycle event.
// Errors log at warn and never abort the dispatched task.
func EmitLifecycle(ctx context.Context, api *cliremote.Client, logger arbor.ILogger, taskID, wsID, projID string, seq int64, kind string, payload map[string]any) {
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

// RunHeartbeat fires heartbeat events every interval until stopCh
// closes. Signals doneCh on exit so the parent can sync before
// emitting stop.
func RunHeartbeat(api *cliremote.Client, logger arbor.ILogger, taskID, wsID, projID string, seqCounter *atomic.Int64, startedAt time.Time, interval time.Duration, stopCh, doneCh chan struct{}) {
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
			_ = logger // logger reserved for future per-frame warn logging
		}
	}
}

// AppendTaskLogPointer writes the single kind:task-log ledger row that
// anchors the task_log chunk range to the audit chain (sty_8c17b89d AC5).
func AppendTaskLogPointer(ctx context.Context, api *cliremote.Client, logger arbor.ILogger, taskID, projectID, storyID string, totalChunks int, byteCount int64) {
	structured, err := json.Marshal(map[string]any{
		"task_log_id":  taskID,
		"total_chunks": totalChunks,
		"byte_count":   byteCount,
	})
	if err != nil {
		if logger != nil {
			logger.Warn().Str("error", err.Error()).Msg("task_log pointer payload marshal failed")
		}
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

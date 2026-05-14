package clientdaemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bobmcallan/satellites/internal/agent/dispatchteam"
	"github.com/bobmcallan/satellites/internal/agent/worker"
)

// runOne is the per-task body executed inside a scheduler-spawned
// goroutine. It opens the per-task log file, fetches the task
// envelope, builds the daemon's dispatch closure, and invokes
// dispatchteam.Run — the same shell the synchronous CLI path uses
// (sty_5aa20f1b AC4 / AC6).
func (d *Daemon) runOne(ctx context.Context, entry QueuedEntry, handle *runningHandle) {
	logPath := filepath.Join(d.opts.LogsDir, entry.TaskID+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		d.warn("logs dir", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		d.warn("open per-task log "+logPath, err)
		// Fall back to discard sinks rather than abort — telemetry
		// remains useful even if local log capture fails.
	}
	var stdoutSink, stderrSink io.Writer = io.Discard, io.Discard
	if logFile != nil {
		defer logFile.Close()
		stdoutSink = logFile
		stderrSink = logFile
	}

	tEnv, err := d.fetchEnvelope(ctx, entry.TaskID)
	if err != nil {
		d.warn("task_get for daemon dispatch", err)
		// Without a project_id we cannot route lifecycle frames
		// usefully; bail before dispatch.
		return
	}

	// Stamp the project_id onto the running handle so reconcileBoot
	// can route an orphan evidence row if the daemon dies mid-flight.
	d.mu.Lock()
	handle.PID = os.Getpid()
	d.mu.Unlock()
	d.persistRunningWithProject(entry.TaskID, tEnv.ProjectID)

	in := dispatchteam.Inputs{
		TaskID:           tEnv.TaskID,
		WorkspaceID:      tEnv.WorkspaceID,
		ProjectID:        tEnv.ProjectID,
		StoryID:          tEnv.StoryID,
		Origin:           tEnv.Origin,
		API:              d.opts.API,
		Logger:           d.opts.Logger,
		Stdout:           stdoutSink,
		Stderr:           stderrSink,
		Heartbeat:        d.opts.Heartbeat,
		DisableTelemetry: false,
		SuppressStop:     &handle.suppressStop,
	}

	env := worker.TaskEnvelope{
		ID:          tEnv.TaskID,
		WorkspaceID: tEnv.WorkspaceID,
		ProjectID:   tEnv.ProjectID,
		Origin:      tEnv.Origin,
	}

	d.info("dispatching task", "task_id", entry.TaskID, "project_id", tEnv.ProjectID, "log", logPath)

	outcome, runErr := dispatchteam.Run(ctx, in, func(ctx context.Context, stdout, stderr io.Writer) (worker.Outcome, error) {
		// Daemon-spawned subprocesses inherit the same parent-side env
		// guards the synchronous CLI sets so any nested
		// `satellites-client task run` inside the agent's claude
		// subprocess passes through (sty_8c17b89d / sty_64e69db8).
		_ = os.Setenv(dispatchteam.TelemetryEnvSkip, "1")
		_ = os.Setenv("SATELLITES_CLIENT_SKIP_UPDATE_CHECK", "1")
		return d.opts.Dispatch(ctx, d.opts.AgentConfig, d.opts.Logger, d.opts.API, env, stdout, stderr)
	})
	d.info("dispatch outcome", "task_id", entry.TaskID, "outcome", string(outcome), "err", runErr)
}

// dispatchEnvelope is the subset of task_get the daemon consults to
// build the dispatchteam.Inputs and worker.TaskEnvelope.
type dispatchEnvelope struct {
	TaskID      string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	StoryID     string `json:"story_id"`
	Origin      string `json:"origin"`
}

func (d *Daemon) fetchEnvelope(ctx context.Context, taskID string) (dispatchEnvelope, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := d.opts.API.Call(cctx, "task_get", map[string]any{"id": taskID}, &raw); err != nil {
		return dispatchEnvelope{}, fmt.Errorf("task_get %s: %w", taskID, err)
	}
	var env dispatchEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return dispatchEnvelope{}, fmt.Errorf("decode task_get %s: %w", taskID, err)
	}
	if env.TaskID == "" {
		env.TaskID = taskID
	}
	return env, nil
}

// persistRunningWithProject re-stamps the in-memory running entry with
// the resolved project_id and persists state. Called once per task
// after fetchEnvelope succeeds so the persisted state.json carries
// enough context for reconcileBoot.
func (d *Daemon) persistRunningWithProject(taskID, projectID string) {
	d.mu.Lock()
	if h, ok := d.running[taskID]; ok {
		_ = h
	}
	// We extend the persisted RunningEntry shape with project_id by
	// snapshotting from running map + project lookup at write time.
	d.mu.Unlock()
	if err := d.persistStateWithProject(taskID, projectID); err != nil {
		d.warn("persistState (with project)", err)
	}
}

// persistStateWithProject is identical to persistState but stamps the
// project_id onto the matching running entry before serialisation.
func (d *Daemon) persistStateWithProject(taskID, projectID string) error {
	d.mu.Lock()
	state := State{
		Running: make([]RunningEntry, 0, len(d.running)),
		Queued:  make([]QueuedEntry, len(d.queue)),
	}
	for _, h := range d.running {
		entry := RunningEntry{TaskID: h.TaskID, PID: h.PID, StartedAt: h.StartedAt}
		if h.TaskID == taskID {
			entry.ProjectID = projectID
		}
		state.Running = append(state.Running, entry)
	}
	for i, q := range d.queue {
		state.Queued[i] = q
		state.Queued[i].QueuePosition = i + 1
	}
	d.mu.Unlock()
	return WriteState(d.opts.StatePath, state)
}

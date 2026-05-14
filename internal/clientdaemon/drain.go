package clientdaemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bobmcallan/satellites/internal/agent/dispatchteam"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// stopFrameSeq is a daemon-wide counter for the daemon-initiated stop
// frame seq. The dispatchteam.Run path uses its per-task counter, so
// daemon_initiated stops use a separate seq pulled from this counter
// to avoid collision with seq=0 (start) when dispatchteam.Run hasn't
// finished initialising. Each per-task drain pulls one seq.
var stopFrameSeq atomic.Int64

// Drain stops accepting new enqueues and waits for in-flight tasks
// to exit, capping at deadline. After deadline:
//
//  1. emits one daemon_initiated `stop` frame per still-running task
//     with payload {"daemon_initiated":true,"outcome":"timeout",
//     "exit_code":-1,"duration_ms":N},
//  2. signals each per-task context to cancel (which causes the
//     dispatched subprocess to receive SIGTERM via the existing
//     worker.RunDispatched ctx-cancellation path),
//  3. waits an additional grace window for the cancelled goroutines
//     to actually exit and remove themselves from the running map.
//
// The daemon's own stop frame is emitted under a high seq value so
// the dispatchteam.Run path inside the worker goroutine does NOT
// also emit a stop (SuppressStop is set on the per-task handle).
func (d *Daemon) Drain(ctx context.Context, deadline time.Duration) error {
	d.draining.Store(true)
	now := d.opts.Now()
	d.drainStart.Store(&now)

	// Signal scheduler so it stops promoting (the draining flag is
	// the gate; the signal just wakes the loop).
	d.signalScheduler()

	// Phase 1: wait up to deadline for natural exits.
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// All tasks exited naturally; nothing more to do.
		return nil
	case <-time.After(deadline):
	case <-ctx.Done():
	}

	// Phase 2: enumerate still-running tasks; emit daemon_initiated
	// stop frame; suppress the per-task Run finalisation; cancel.
	d.mu.Lock()
	stragglers := make([]*runningHandle, 0, len(d.running))
	for _, h := range d.running {
		stragglers = append(stragglers, h)
	}
	d.mu.Unlock()

	for _, h := range stragglers {
		// Mark suppression FIRST so when the worker goroutine wakes
		// from the cancel, dispatchteam.Run skips its own stop
		// emission.
		h.suppressStop.Store(true)

		// Emit the daemon-initiated stop frame BEFORE cancelling.
		d.emitDaemonInitiatedStop(ctx, h, now)

		if h.cancel != nil {
			h.cancel()
		}
	}

	// Phase 3: brief grace window for cancelled goroutines to exit
	// + de-register from the running map.
	graceDone := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(graceDone)
	}()
	select {
	case <-graceDone:
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	return nil
}

func (d *Daemon) emitDaemonInitiatedStop(ctx context.Context, h *runningHandle, drainStartedAt time.Time) {
	if d.opts.API == nil {
		return
	}
	d.mu.Lock()
	wsID, projID := "", ""
	// We rely on the persisted state.json's project_id (set by
	// persistRunningWithProject after task_get). For an in-memory
	// snapshot we re-derive via task_get if the running entry has
	// no cached project_id — but cheap path: skip emission when
	// neither field is known. The worker.runOne path always populates
	// project_id before the dispatch closure begins emitting frames.
	d.mu.Unlock()
	// Re-fetch envelope to get workspace_id / project_id at drain
	// time (state.json carries project_id but not workspace_id).
	if env, err := d.fetchEnvelope(ctx, h.TaskID); err == nil {
		wsID = env.WorkspaceID
		projID = env.ProjectID
	} else {
		d.warn("drain envelope refetch", err)
	}
	durationMs := time.Since(h.StartedAt).Milliseconds()
	if !drainStartedAt.IsZero() {
		// duration_ms reflects how long the task ran before the
		// daemon called it cooked — clamp to drain start when the
		// task's started_at is bogusly stale.
		if drainStartedAt.After(h.StartedAt) {
			durationMs = drainStartedAt.Sub(h.StartedAt).Milliseconds()
		}
	}
	payload := map[string]any{
		"outcome":          "timeout",
		"exit_code":        -1,
		"duration_ms":      durationMs,
		"daemon_initiated": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		d.warn("drain stop marshal", err)
		return
	}
	seq := stopFrameSeq.Add(1) + 1_000_000_000 // billion-offset to dodge per-task counter collisions
	in := cliremote.TaskLogAppendInput{
		TaskID:      h.TaskID,
		WorkspaceID: wsID,
		ProjectID:   projID,
		Seq:         seq,
		TS:          time.Now().UTC(),
		Kind:        "stop",
		Payload:     body,
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := d.opts.API.TaskLogAppend(cctx, in); err != nil {
		d.warn("drain stop frame", err)
	}
	_ = dispatchteam.TelemetryEnvSkip // import-binding so linter doesn't strip the import
	_ = fmt.Sprintf
}

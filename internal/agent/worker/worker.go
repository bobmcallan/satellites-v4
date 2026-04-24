// Package worker is the satellites-agent worker loop per
// docs/architecture.md §3 "Satellites-agent" and §9 "Worker ↔ server
// protocol". A Worker pulls a task, runs the contract's
// agent_instruction (the scaffolded version in 9.4; LLM-driven
// execution is downstream), emits a heartbeat, closes the task, and
// repeats.
//
// Principles: pr_f81f60ca (agent is worker; orchestration elsewhere),
// pr_20440c77 (process-order invariant).
//
// The Worker is abstracted over a Client interface so unit tests can
// inject a fake without a live satellites server. The production
// binary at cmd/satellites-agent wires an HTTP client that translates
// to MCP calls.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ternarybob/arbor"
)

// DefaultIdleBackoff governs how long the loop sleeps when task_claim
// returns null. Overridable via Config.
const DefaultIdleBackoff = 5 * time.Second

// DefaultHeartbeatInterval governs how often the worker writes
// kind:worker-heartbeat ledger rows during an in-flight task.
const DefaultHeartbeatInterval = 60 * time.Second

// DefaultExecuteTimeout caps a single task's execute phase. The
// server's reclaim watchdog is the primary expiry mechanism; this is a
// defensive bound so a buggy execute can't hang the worker.
const DefaultExecuteTimeout = 10 * time.Minute

// TaskEnvelope is the subset of the server-side Task that the worker
// consumes. Agent-side package to avoid a cross-layer import cycle.
type TaskEnvelope struct {
	ID           string
	WorkspaceID  string
	ProjectID    string
	Origin       string
	LedgerRootID string
	Payload      []byte
}

// Outcome mirrors the task package's outcome enum strings.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeTimeout Outcome = "timeout"
)

// Client is the worker's interface to the satellites server. Real
// implementation issues MCP HTTP calls; tests inject a fake.
type Client interface {
	// Claim attempts to claim a task. Returns nil TaskEnvelope when the
	// queue is empty (not an error).
	Claim(ctx context.Context, workerID string, workspaceIDs []string) (*TaskEnvelope, error)

	// Execute runs the contract's agent_instruction for the task. In
	// 9.4's scaffold, Execute writes an evidence ledger row + returns a
	// success outcome; the real LLM-driven implementation is downstream.
	Execute(ctx context.Context, task TaskEnvelope) (Outcome, error)

	// Close transitions the task to the given outcome. workerID is
	// passed through for the server's stale-claim check (story_b4513c8c).
	Close(ctx context.Context, taskID, workerID string, outcome Outcome) error

	// Heartbeat writes a kind:worker-heartbeat ledger row. Called on
	// HeartbeatInterval cadence during an in-flight task.
	Heartbeat(ctx context.Context, workerID, taskID, workspaceID string) error

	// Shutdown writes a kind:worker-shutdown ledger row. Called once on
	// Worker.Stop to announce graceful termination.
	Shutdown(ctx context.Context, workerID, reason string) error
}

// Config bundles the worker's operational settings.
type Config struct {
	WorkerID          string
	WorkspaceIDs      []string
	IdleBackoff       time.Duration
	HeartbeatInterval time.Duration
	ExecuteTimeout    time.Duration
}

// Worker runs the claim → execute → close loop against a Client.
type Worker struct {
	cfg    Config
	client Client
	logger arbor.ILogger

	mu      sync.Mutex
	running bool
	done    chan struct{}
}

// New constructs a Worker. Zero-valued config durations fall back to
// documented defaults. Logger is optional (nil safe).
func New(client Client, cfg Config, logger arbor.ILogger) *Worker {
	if cfg.IdleBackoff <= 0 {
		cfg.IdleBackoff = DefaultIdleBackoff
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.ExecuteTimeout <= 0 {
		cfg.ExecuteTimeout = DefaultExecuteTimeout
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("worker_%d", time.Now().UnixNano())
	}
	return &Worker{
		cfg:    cfg,
		client: client,
		logger: logger,
	}
}

// Loop runs the worker until ctx is cancelled. Returns when the loop
// exits (either because ctx is done or an unrecoverable error fires).
// On ctx cancel, in-flight tasks finish naturally within ExecuteTimeout
// or get closed with outcome=timeout; a kind:worker-shutdown row is
// written before return.
func (w *Worker) Loop(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return errors.New("worker: already running")
	}
	w.running = true
	w.done = make(chan struct{})
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running = false
		close(w.done)
		w.mu.Unlock()
	}()

	for {
		if ctx.Err() != nil {
			w.emitShutdown(context.Background(), "ctx canceled")
			return ctx.Err()
		}
		task, err := w.client.Claim(ctx, w.cfg.WorkerID, w.cfg.WorkspaceIDs)
		if err != nil {
			if w.logger != nil {
				w.logger.Warn().Str("error", err.Error()).Msg("worker claim failed")
			}
			if sleepInterruptible(ctx, w.cfg.IdleBackoff) {
				w.emitShutdown(context.Background(), "ctx canceled during backoff")
				return ctx.Err()
			}
			continue
		}
		if task == nil {
			if sleepInterruptible(ctx, w.cfg.IdleBackoff) {
				w.emitShutdown(context.Background(), "ctx canceled during idle")
				return ctx.Err()
			}
			continue
		}
		w.runOne(ctx, *task)
	}
}

// Done returns a channel that closes when Loop exits. Useful for tests
// that want to wait for graceful shutdown.
func (w *Worker) Done() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done
}

// runOne executes one task end-to-end: start heartbeat goroutine,
// Execute (bounded by ExecuteTimeout), stop heartbeat, Close with the
// execute outcome (or timeout on ctx cancel mid-execute).
func (w *Worker) runOne(parent context.Context, task TaskEnvelope) {
	hbCtx, hbCancel := context.WithCancel(parent)
	defer hbCancel()
	go w.heartbeatLoop(hbCtx, task)

	execCtx, execCancel := context.WithTimeout(parent, w.cfg.ExecuteTimeout)
	defer execCancel()

	outcome, err := w.client.Execute(execCtx, task)
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) || errors.Is(parent.Err(), context.Canceled) {
		outcome = OutcomeTimeout
	}
	if err != nil && outcome == "" {
		outcome = OutcomeFailure
		if w.logger != nil {
			w.logger.Warn().Str("task_id", task.ID).Str("error", err.Error()).Msg("worker execute failed")
		}
	}

	// Close always runs — use a background context with a short bound so
	// the final close lands even when parent is canceled.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer closeCancel()
	if err := w.client.Close(closeCtx, task.ID, w.cfg.WorkerID, outcome); err != nil {
		if w.logger != nil {
			w.logger.Warn().Str("task_id", task.ID).Str("error", err.Error()).Msg("worker close failed")
		}
	}
}

// heartbeatLoop fires every HeartbeatInterval while the task is in
// flight. Writes a kind:worker-heartbeat ledger row via the Client. On
// ctx cancel, returns cleanly without firing a final heartbeat.
func (w *Worker) heartbeatLoop(ctx context.Context, task TaskEnvelope) {
	t := time.NewTicker(w.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.client.Heartbeat(ctx, w.cfg.WorkerID, task.ID, task.WorkspaceID); err != nil {
				if w.logger != nil {
					w.logger.Warn().Str("task_id", task.ID).Str("error", err.Error()).Msg("worker heartbeat failed")
				}
			}
		}
	}
}

// emitShutdown writes the kind:worker-shutdown ledger row on graceful
// exit. Uses a fresh context so the row lands even after the loop's
// ctx has canceled.
func (w *Worker) emitShutdown(ctx context.Context, reason string) {
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := w.client.Shutdown(shutdownCtx, w.cfg.WorkerID, reason); err != nil && w.logger != nil {
		w.logger.Warn().Str("error", err.Error()).Msg("worker shutdown row failed")
	}
}

// sleepInterruptible sleeps for d or until ctx is canceled. Returns
// true when ctx was canceled.
func sleepInterruptible(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

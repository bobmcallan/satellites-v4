package clientdaemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// runWatchdog is the queue-stall observer goroutine launched alongside
// runScheduler. It ticks every Options.WatchdogInterval, inspects the
// head of the FIFO, and emits a `kind:queue-stalled` ledger evidence
// row when the head has been waiting longer than
// Options.WatchdogThreshold. Each stall episode is reported at most
// once: a task_id stays in the emitted-set until it leaves the head
// (promotion, cancel, or a new head taking its place).
func (d *Daemon) runWatchdog(ctx context.Context) {
	interval := d.opts.WatchdogInterval
	if interval <= 0 {
		interval = DefaultWatchdogInterval
	}
	threshold := d.opts.WatchdogThreshold
	if threshold <= 0 {
		threshold = DefaultWatchdogThreshold
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	var mu sync.Mutex
	emitted := map[string]time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		d.watchdogTick(ctx, threshold, &mu, emitted)
	}
}

// watchdogTick snapshots the head of the queue and emits a stall row
// when threshold is exceeded. emitted tracks which task ids have
// already produced a row in their current head-of-queue episode.
func (d *Daemon) watchdogTick(ctx context.Context, threshold time.Duration, mu *sync.Mutex, emitted map[string]time.Time) {
	d.mu.Lock()
	if len(d.queue) == 0 {
		d.mu.Unlock()
		// Queue is empty — clear the emitted set so any future stall
		// of a re-enqueued task fires again.
		mu.Lock()
		for k := range emitted {
			delete(emitted, k)
		}
		mu.Unlock()
		return
	}
	head := d.queue[0]
	queueDepth := len(d.queue)
	runningCount := len(d.running)
	d.mu.Unlock()

	// Garbage-collect emitted entries that are no longer at the head
	// (the previous-head's stall episode is over once a different task
	// occupies the head).
	mu.Lock()
	for k := range emitted {
		if k != head.TaskID {
			delete(emitted, k)
		}
	}
	if _, already := emitted[head.TaskID]; already {
		mu.Unlock()
		return
	}
	mu.Unlock()

	age := d.opts.Now().Sub(head.EnqueuedAt)
	if age < threshold {
		return
	}

	if !d.emitQueueStalled(ctx, head, age, queueDepth, runningCount) {
		return
	}

	mu.Lock()
	emitted[head.TaskID] = d.opts.Now()
	mu.Unlock()
}

// emitQueueStalled writes the `kind:queue-stalled` ledger evidence row
// for the supplied head-of-queue entry. Returns true when the row was
// successfully appended (or the row's project_id is resolvable);
// false when project_id resolution fails and the emission is deferred
// to a future tick. Mirrors appendOrphanEvidence's defensive shape.
func (d *Daemon) emitQueueStalled(ctx context.Context, head QueuedEntry, age time.Duration, queueDepth, runningCount int) bool {
	if d.opts.API == nil {
		return false
	}

	envCtx, envCancel := context.WithTimeout(ctx, 5*time.Second)
	defer envCancel()
	env, err := d.fetchEnvelope(envCtx, head.TaskID)
	if err != nil {
		d.warn("watchdog envelope fetch", err)
		return false
	}
	if env.ProjectID == "" {
		d.warn("watchdog: missing project_id", fmt.Errorf("task_id=%s", head.TaskID))
		return false
	}

	structured, err := json.Marshal(map[string]any{
		"task_id":       head.TaskID,
		"age_ms":        age.Milliseconds(),
		"queue_depth":   queueDepth,
		"running_count": runningCount,
		"head_task_id":  head.TaskID,
		"daemon_pid":    os.Getpid(),
		"enqueued_at":   head.EnqueuedAt.UTC().Format(time.RFC3339Nano),
		"observed_at":   d.opts.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		d.warn("watchdog stall marshal", err)
		return false
	}

	args := map[string]any{
		"project_id": env.ProjectID,
		"type":       "evidence",
		"tags": []string{
			"task_id:" + head.TaskID,
			"kind:queue-stalled",
		},
		"content":    fmt.Sprintf("queue stalled: task=%s age=%s queue_depth=%d running_count=%d", head.TaskID, age, queueDepth, runningCount),
		"structured": json.RawMessage(structured),
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.opts.API.Call(cctx, "ledger_append", args, nil); err != nil {
		d.warn("watchdog ledger_append", err)
		return false
	}
	d.info("queue stalled", "task_id", head.TaskID, "age_ms", age.Milliseconds(), "queue_depth", queueDepth, "running_count", runningCount)
	return true
}

package clientdaemon

import (
	"context"
	"time"
)

// runScheduler is the main goroutine that drains the FIFO queue
// whenever a slot is free. It blocks on `notify` and a per-tick
// fallback so a slow scheduler-trigger path cannot stall promotion.
func (d *Daemon) runScheduler(ctx context.Context, slots chan struct{}) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.notify:
		case <-tick.C:
		}
		d.promoteOne(ctx, slots)
	}
}

// promoteOne tries to start one queued task if a slot is available
// and the daemon is not draining. The slot is acquired BEFORE the
// goroutine spawns so the cap is binding even under burst.
func (d *Daemon) promoteOne(ctx context.Context, slots chan struct{}) {
	if d.draining.Load() {
		return
	}
	d.mu.Lock()
	if len(d.queue) == 0 {
		d.mu.Unlock()
		return
	}
	headID := d.queue[0].TaskID
	queueDepth := len(d.queue)
	runningCount := len(d.running)
	d.mu.Unlock()

	select {
	case slots <- struct{}{}:
	default:
		d.debug("no slot free", "queue_head", headID, "queue_depth", queueDepth, "running_count", runningCount)
		return // no slot free
	}

	d.mu.Lock()
	if len(d.queue) == 0 {
		<-slots
		d.mu.Unlock()
		return
	}
	entry := d.queue[0]
	d.queue = d.queue[1:]
	taskCtx, cancel := context.WithCancel(ctx)
	handle := &runningHandle{
		TaskID:    entry.TaskID,
		StartedAt: d.opts.Now(),
		PID:       0, // populated by runOne when the worker PID is known
		cancel:    cancel,
	}
	d.running[entry.TaskID] = handle
	promotedQueueDepth := len(d.queue)
	promotedRunningCount := len(d.running)
	d.mu.Unlock()

	d.info("task promoted", "task_id", entry.TaskID, "queue_depth", promotedQueueDepth, "running_count", promotedRunningCount)

	if err := d.persistState(); err != nil {
		d.warn("persistState (promote)", err)
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.running, entry.TaskID)
			completedQueueDepth := len(d.queue)
			completedRunningCount := len(d.running)
			d.mu.Unlock()
			if err := d.persistState(); err != nil {
				d.warn("persistState (complete)", err)
			}
			<-slots
			d.info("task completed", "task_id", entry.TaskID, "queue_depth_after", completedQueueDepth, "running_count_after", completedRunningCount)
			d.signalScheduler()
		}()
		d.runOne(taskCtx, entry, handle)
	}()
}

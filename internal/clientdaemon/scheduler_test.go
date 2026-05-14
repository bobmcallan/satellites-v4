package clientdaemon

import (
	"context"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/agent/worker"
)

// TestSchedulerCap is sty_5aa20f1b AC5: at parallelism=2, the third
// enqueued task must wait for one of the first two to finish, then
// promote within 100ms.
func TestSchedulerCap(t *testing.T) {
	stub := newStub(t)
	for _, id := range []string{"task_a", "task_b", "task_c"} {
		stub.addTask(id, map[string]any{"workspace_id": "w", "project_id": "p", "story_id": "s", "origin": "story_stage"})
	}

	calls := make(chan dispatchCall, 3)
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 2
		o.Heartbeat = time.Hour // suppress heartbeats so we don't pollute the test stub
		o.Dispatch = captureDispatch(t, calls, 200*time.Millisecond, worker.OutcomeSuccess)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.WaitInflight() }()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)

	if _, err := d.Enqueue(ctx, "task_a"); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if _, err := d.Enqueue(ctx, "task_b"); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if _, err := d.Enqueue(ctx, "task_c"); err != nil {
		t.Fatalf("enqueue c: %v", err)
	}

	// At t=50ms expect 2 running + 1 queued.
	time.Sleep(50 * time.Millisecond)
	st := d.Status()
	if len(st.Running) != 2 {
		t.Errorf("at t=50ms: running=%d want=2 (%+v)", len(st.Running), st.Running)
	}
	if len(st.Queued) != 1 {
		t.Errorf("at t=50ms: queued=%d want=1 (%+v)", len(st.Queued), st.Queued)
	}

	// Wait for one of the first two to exit (~200ms) plus 80ms grace.
	time.Sleep(280 * time.Millisecond)
	st = d.Status()
	if len(st.Queued) != 0 {
		t.Errorf("after first exits: queued=%d want=0 (%+v)", len(st.Queued), st.Queued)
	}

	// Drain the dispatch-call channel so the test cleanly exits when
	// the goroutines complete.
	deadline := time.Now().Add(2 * time.Second)
	got := 0
	for time.Now().Before(deadline) && got < 3 {
		select {
		case <-calls:
			got++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if got < 3 {
		t.Errorf("captured %d dispatch calls, want 3", got)
	}
}

// TestEnqueueIdempotent: re-enqueue of a queued task returns the same
// position; re-enqueue of a running task returns position=0.
func TestEnqueueIdempotent(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_a", map[string]any{"workspace_id": "w", "project_id": "p"})
	stub.addTask("task_b", map[string]any{"workspace_id": "w", "project_id": "p"})
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.Dispatch = captureDispatch(t, make(chan dispatchCall, 8), 5*time.Second, worker.OutcomeSuccess)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.WaitInflight() }()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)

	if _, err := d.Enqueue(ctx, "task_a"); err != nil {
		t.Fatalf("first enqueue a: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // a promotes
	resp, err := d.Enqueue(ctx, "task_a")
	if err != nil {
		t.Fatalf("re-enqueue running: %v", err)
	}
	if resp.QueuePosition != 0 {
		t.Errorf("running re-enqueue: position=%d want=0", resp.QueuePosition)
	}

	if _, err := d.Enqueue(ctx, "task_b"); err != nil {
		t.Fatalf("first enqueue b: %v", err)
	}
	resp2, err := d.Enqueue(ctx, "task_b")
	if err != nil {
		t.Fatalf("re-enqueue queued: %v", err)
	}
	if resp2.QueuePosition != 1 {
		t.Errorf("queued re-enqueue: position=%d want=1 (idempotent)", resp2.QueuePosition)
	}
}

// TestEnqueueQueueFull returns 503 when the cap is reached.
func TestEnqueueQueueFull(t *testing.T) {
	stub := newStub(t)
	for _, id := range []string{"q1", "q2", "q3"} {
		stub.addTask(id, map[string]any{"workspace_id": "w", "project_id": "p"})
	}
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 0 // forces default; we want only the queue exercised
		o.MaxQueue = 2
		o.Heartbeat = time.Hour
	})
	if _, err := d.Enqueue(context.Background(), "q1"); err != nil {
		t.Fatalf("q1: %v", err)
	}
	if _, err := d.Enqueue(context.Background(), "q2"); err != nil {
		t.Fatalf("q2: %v", err)
	}
	_, err := d.Enqueue(context.Background(), "q3")
	if err == nil {
		t.Fatalf("q3: expected queue-full error")
	}
	coded, ok := err.(errCoded)
	if !ok {
		t.Fatalf("q3 error type: %T", err)
	}
	if coded.HTTPStatus() != 503 {
		t.Errorf("q3 HTTP status = %d, want 503", coded.HTTPStatus())
	}
}

// TestCancelQueuedRemovesEntry covers the queued-cancel branch.
func TestCancelQueuedRemovesEntry(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_a", map[string]any{"workspace_id": "w", "project_id": "p"})
	d := newTestDaemon(t, stub, func(o *Options) { o.Parallelism = 0 })
	if _, err := d.Enqueue(context.Background(), "task_a"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	resp, err := d.Cancel(context.Background(), "task_a")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if resp.PrevState != "queued" || resp.Action != "removed_from_queue" {
		t.Errorf("cancel response = %+v", resp)
	}
	if len(d.Status().Queued) != 0 {
		t.Errorf("queued not empty after cancel")
	}
}

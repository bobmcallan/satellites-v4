package clientdaemon

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phuslu/log"
	"github.com/ternarybob/arbor"
	arborwriters "github.com/ternarybob/arbor/writers"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
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

// TestSchedulerNoSilentDrop_StuckTaskBehindHead — sty_f60233ef AC1.
// At parallelism=1, when task_stuck holds the only slot, task_followup
// must be queued at position 1 within 100ms. When task_stuck releases,
// task_followup must promote within 200ms. Validates that an
// in-flight (stuck) task does not cause the scheduler to silently
// drop subsequent enqueues.
func TestSchedulerNoSilentDrop_StuckTaskBehindHead(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_stuck", map[string]any{"workspace_id": "w", "project_id": "p"})
	stub.addTask("task_followup", map[string]any{"workspace_id": "w", "project_id": "p"})

	releaseStuck := make(chan struct{})
	releaseFollowup := make(chan struct{})
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.Dispatch = func(ctx context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, env worker.TaskEnvelope, _, _ io.Writer) (worker.Outcome, error) {
			switch env.ID {
			case "task_stuck":
				select {
				case <-releaseStuck:
				case <-ctx.Done():
				}
			case "task_followup":
				select {
				case <-releaseFollowup:
				case <-ctx.Done():
				}
			}
			return worker.OutcomeSuccess, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		close(releaseStuck)
		close(releaseFollowup)
		cancel()
		d.WaitInflight()
	}()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)

	if _, err := d.Enqueue(ctx, "task_stuck"); err != nil {
		t.Fatalf("enqueue stuck: %v", err)
	}
	if _, err := d.Enqueue(ctx, "task_followup"); err != nil {
		t.Fatalf("enqueue followup: %v", err)
	}

	// Within 100ms task_stuck running, task_followup queued at pos 1.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := d.Status()
		if len(st.Running) == 1 && len(st.Queued) == 1 && st.Queued[0].TaskID == "task_followup" && st.Queued[0].QueuePosition == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := d.Status()
	if len(st.Running) != 1 || st.Running[0].TaskID != "task_stuck" {
		t.Fatalf("task_stuck not running: %+v", st.Running)
	}
	if len(st.Queued) != 1 || st.Queued[0].TaskID != "task_followup" || st.Queued[0].QueuePosition != 1 {
		t.Fatalf("task_followup not queued at position 1: %+v", st.Queued)
	}

	// Release task_stuck → task_followup must promote within 200ms.
	releaseStuck <- struct{}{}
	deadline = time.Now().Add(200 * time.Millisecond)
	promoted := false
	for time.Now().Before(deadline) {
		st := d.Status()
		for _, r := range st.Running {
			if r.TaskID == "task_followup" {
				promoted = true
				break
			}
		}
		if promoted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !promoted {
		t.Errorf("task_followup not promoted within 200ms of stuck release: status=%+v", d.Status())
	}
}

// TestSchedulerWatchdog_StalledQueueEmitsEvidence — sty_f60233ef AC1.
// With WatchdogInterval=50ms and WatchdogThreshold=150ms, an
// always-stuck head task causes the watchdog to emit one
// `kind:queue-stalled` ledger evidence row tagged with the followup's
// task_id once the head's age exceeds the threshold.
func TestSchedulerWatchdog_StalledQueueEmitsEvidence(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_stuck", map[string]any{"workspace_id": "w", "project_id": "p"})
	stub.addTask("task_followup", map[string]any{"workspace_id": "w", "project_id": "p"})

	release := make(chan struct{})
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.WatchdogInterval = 50 * time.Millisecond
		o.WatchdogThreshold = 150 * time.Millisecond
		o.Dispatch = func(ctx context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, env worker.TaskEnvelope, _, _ io.Writer) (worker.Outcome, error) {
			if env.ID == "task_stuck" {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return worker.OutcomeSuccess, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		close(release)
		cancel()
		d.WaitInflight()
	}()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)
	go d.runWatchdog(ctx)

	if _, err := d.Enqueue(ctx, "task_stuck"); err != nil {
		t.Fatalf("enqueue stuck: %v", err)
	}
	if _, err := d.Enqueue(ctx, "task_followup"); err != nil {
		t.Fatalf("enqueue followup: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	rows := stub.ledger()
	found := 0
	for _, r := range rows {
		tags, _ := r["tags"].([]any)
		hasStalled := false
		hasFollowup := false
		for _, t := range tags {
			s, _ := t.(string)
			if s == "kind:queue-stalled" {
				hasStalled = true
			}
			if s == "task_id:task_followup" {
				hasFollowup = true
			}
		}
		if hasStalled && hasFollowup {
			found++
		}
	}
	if found < 1 {
		t.Fatalf("expected at least one queue-stalled row for task_followup; ledger=%+v", rows)
	}
}

// TestSchedulerEmitsLifecycleEvents — sty_f60233ef AC1. Dispatch one
// task end-to-end through a daemon backed by a buffer-backed console
// writer (which renders structured fields inline) and assert the
// buffer contains every lifecycle marker in order.
func TestSchedulerEmitsLifecycleEvents(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_lifecycle", map[string]any{"workspace_id": "w", "project_id": "p"})

	capture := &capturingWriter{}
	// WithWriters attaches a per-logger writer slice that bypasses the
	// global writer registry (arbor's emit path consults logger.writers
	// first when set). The capturingWriter records the raw arbor JSON
	// envelopes, preserving the structured `label` field that the
	// production code attaches to each lifecycle event.
	logger := arbor.NewLogger().
		WithWriters([]arborwriters.IWriter{capture}).
		WithLevelFromString("info")

	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.Logger = logger
		o.Dispatch = func(_ context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, _ worker.TaskEnvelope, _, _ io.Writer) (worker.Outcome, error) {
			return worker.OutcomeSuccess, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.WaitInflight() }()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)

	if _, err := d.Enqueue(ctx, "task_lifecycle"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	want := []string{"task enqueued", "task promoted", "dispatching task", "task completed"}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if matchOrderedInBuffer(capture.String(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("lifecycle markers %v not found in order; buf=%s", want, capture.String())
}

// capturingWriter is a minimal arbor IWriter that buffers the raw
// JSON envelopes arbor's emit path passes to Write. Each envelope is
// a single LogEvent (structured fields preserved), so tests can assert
// on field values directly via substring match.
type capturingWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if err == nil {
		_ = w.buf.WriteByte('\n')
	}
	return n, err
}

func (w *capturingWriter) WithLevel(_ log.Level) arborwriters.IWriter { return w }
func (w *capturingWriter) GetFilePath() string                        { return "" }
func (w *capturingWriter) Close() error                               { return nil }

func (w *capturingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// matchOrderedInBuffer reports whether every needle in `want` appears
// in `s` in order, advancing the cursor on each match.
func matchOrderedInBuffer(s string, want []string) bool {
	cursor := 0
	for _, n := range want {
		idx := strings.Index(s[cursor:], n)
		if idx < 0 {
			return false
		}
		cursor += idx + len(n)
	}
	return true
}

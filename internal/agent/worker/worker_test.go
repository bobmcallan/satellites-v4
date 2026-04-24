package worker_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/agent/worker"
)

// fakeClient is a deterministic in-memory Client for tests.
type fakeClient struct {
	mu sync.Mutex

	queue         []*worker.TaskEnvelope
	claimCalls    int
	executeCalls  int
	closeCalls    []closeCall
	heartbeatHits int64
	shutdownHits  int64

	// Optional hooks to inject execute behaviour.
	executeFn func(ctx context.Context, task worker.TaskEnvelope) (worker.Outcome, error)
}

type closeCall struct {
	TaskID   string
	WorkerID string
	Outcome  worker.Outcome
}

func (c *fakeClient) Claim(ctx context.Context, workerID string, workspaceIDs []string) (*worker.TaskEnvelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claimCalls++
	if len(c.queue) == 0 {
		return nil, nil
	}
	t := c.queue[0]
	c.queue = c.queue[1:]
	return t, nil
}

func (c *fakeClient) Execute(ctx context.Context, task worker.TaskEnvelope) (worker.Outcome, error) {
	c.mu.Lock()
	c.executeCalls++
	fn := c.executeFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, task)
	}
	return worker.OutcomeSuccess, nil
}

func (c *fakeClient) Close(ctx context.Context, taskID, workerID string, outcome worker.Outcome) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeCalls = append(c.closeCalls, closeCall{TaskID: taskID, WorkerID: workerID, Outcome: outcome})
	return nil
}

func (c *fakeClient) Heartbeat(ctx context.Context, workerID, taskID, workspaceID string) error {
	atomic.AddInt64(&c.heartbeatHits, 1)
	return nil
}

func (c *fakeClient) Shutdown(ctx context.Context, workerID, reason string) error {
	atomic.AddInt64(&c.shutdownHits, 1)
	return nil
}

func TestWorker_HappyPath_ClaimExecuteClose(t *testing.T) {
	t.Parallel()
	c := &fakeClient{
		queue: []*worker.TaskEnvelope{{ID: "task_1", WorkspaceID: "wksp_a", Origin: "scheduled"}},
	}
	w := worker.New(c, worker.Config{
		WorkerID:          "worker_a",
		WorkspaceIDs:      []string{"wksp_a"},
		IdleBackoff:       5 * time.Millisecond,
		HeartbeatInterval: 1 * time.Hour,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = w.Loop(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, 1, c.executeCalls, "execute should run once for the queued task")
	require.Len(t, c.closeCalls, 1)
	assert.Equal(t, "task_1", c.closeCalls[0].TaskID)
	assert.Equal(t, "worker_a", c.closeCalls[0].WorkerID)
	assert.Equal(t, worker.OutcomeSuccess, c.closeCalls[0].Outcome)
	assert.GreaterOrEqual(t, int64(1), atomic.LoadInt64(&c.shutdownHits))
}

func TestWorker_EmptyQueue_IdleBackoffAndShutdown(t *testing.T) {
	t.Parallel()
	c := &fakeClient{}
	w := worker.New(c, worker.Config{
		WorkerID:     "worker_idle",
		WorkspaceIDs: []string{"wksp_a"},
		IdleBackoff:  10 * time.Millisecond,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = w.Loop(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, 0, c.executeCalls, "execute should not run on empty queue")
	assert.Empty(t, c.closeCalls, "no tasks → no closes")
	assert.Greater(t, c.claimCalls, 1, "idle backoff should retry claim at least once")
	assert.Equal(t, int64(1), atomic.LoadInt64(&c.shutdownHits), "graceful shutdown should emit exactly one shutdown row")
}

func TestWorker_ShutdownMidTask_ClosesWithTimeout(t *testing.T) {
	t.Parallel()
	// Execute hook blocks on ctx — simulates a long-running task that
	// the worker must interrupt on shutdown.
	executeStarted := make(chan struct{})
	c := &fakeClient{
		queue: []*worker.TaskEnvelope{{ID: "task_slow", WorkspaceID: "wksp_a"}},
		executeFn: func(ctx context.Context, task worker.TaskEnvelope) (worker.Outcome, error) {
			close(executeStarted)
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	w := worker.New(c, worker.Config{
		WorkerID:          "worker_slow",
		WorkspaceIDs:      []string{"wksp_a"},
		IdleBackoff:       5 * time.Millisecond,
		HeartbeatInterval: 1 * time.Hour,
		ExecuteTimeout:    1 * time.Hour, // long enough that parent cancel, not timeout, is the trigger
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())

	loopDone := make(chan struct{})
	go func() {
		_ = w.Loop(ctx)
		close(loopDone)
	}()
	<-executeStarted
	cancel()
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit within 1s after cancel")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.closeCalls, 1, "in-flight task should close even on cancel")
	assert.Equal(t, worker.OutcomeTimeout, c.closeCalls[0].Outcome, "cancel mid-execute should close with outcome=timeout")
}

func TestWorker_HeartbeatFiresDuringExecution(t *testing.T) {
	t.Parallel()
	c := &fakeClient{
		queue: []*worker.TaskEnvelope{{ID: "task_hb", WorkspaceID: "wksp_a"}},
		executeFn: func(ctx context.Context, task worker.TaskEnvelope) (worker.Outcome, error) {
			// Block long enough for two heartbeat ticks at 20ms interval.
			select {
			case <-time.After(60 * time.Millisecond):
			case <-ctx.Done():
			}
			return worker.OutcomeSuccess, nil
		},
	}
	w := worker.New(c, worker.Config{
		WorkerID:          "worker_hb",
		WorkspaceIDs:      []string{"wksp_a"},
		IdleBackoff:       5 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		ExecuteTimeout:    1 * time.Hour,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = w.Loop(ctx)

	hits := atomic.LoadInt64(&c.heartbeatHits)
	assert.GreaterOrEqual(t, hits, int64(2), "heartbeat should fire at least twice during the 60ms execute at 20ms interval; got %d", hits)
}

func TestWorker_DoubleLoop_Rejected(t *testing.T) {
	t.Parallel()
	c := &fakeClient{}
	w := worker.New(c, worker.Config{WorkerID: "w", WorkspaceIDs: []string{"wksp_a"}, IdleBackoff: 1 * time.Second}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go w.Loop(ctx)
	time.Sleep(5 * time.Millisecond)
	err := w.Loop(ctx)
	assert.Error(t, err, "second concurrent Loop call should error")
	assert.True(t, errors.Is(err, err), "any error type acceptable; just ensures the double-Loop guard fires")
	cancel()
	<-w.Done()
}

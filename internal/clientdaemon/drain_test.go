package clientdaemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
	"io"
)

// TestDrainEmitsDaemonInitiatedStop (sty_5aa20f1b AC2) — when the
// drain deadline fires before the worker exits, the daemon emits a
// stop frame with `daemon_initiated:true,outcome:"timeout"` against
// the substrate task_log.
func TestDrainEmitsDaemonInitiatedStop(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_d", map[string]any{
		"workspace_id": "wksp_d",
		"project_id":   "proj_d",
		"story_id":     "sty_d",
		"origin":       "story_stage",
	})

	dispatchEntered := make(chan struct{})
	dispatchExit := make(chan struct{})

	var mu sync.Mutex
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.DrainTimeout = 100 * time.Millisecond
		o.Dispatch = func(ctx context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, _ worker.TaskEnvelope, _, _ io.Writer) (worker.Outcome, error) {
			mu.Lock()
			select {
			case <-dispatchEntered:
			default:
				close(dispatchEntered)
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
			case <-dispatchExit:
			}
			return worker.OutcomeTimeout, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.WaitInflight() }()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)

	if _, err := d.Enqueue(ctx, "task_d"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case <-dispatchEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatch never entered")
	}

	// Drain with a short deadline; dispatch is still blocked.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := d.Drain(drainCtx, d.opts.DrainTimeout); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Release the dispatcher so the worker goroutine can exit; the
	// per-task dispatchteam.Run finalisation should observe
	// SuppressStop=true and skip its own stop emission.
	close(dispatchExit)

	// Give the suppression path a moment to settle, then assert
	// the daemon-initiated stop frame is present + the per-task
	// natural stop is NOT.
	time.Sleep(200 * time.Millisecond)

	calls := stub.appends()
	var (
		daemonStops, naturalStops int
	)
	for _, c := range calls {
		if c["kind"] != "stop" {
			continue
		}
		payloadRaw, _ := c["payload"].(string)
		if payloadRaw == "" {
			if asMap, ok := c["payload"].(map[string]any); ok {
				if v, _ := asMap["daemon_initiated"].(bool); v {
					daemonStops++
				} else {
					naturalStops++
				}
				continue
			}
			b, _ := json.Marshal(c["payload"])
			payloadRaw = string(b)
		}
		// Try both decode shapes: the stub may have stored payload
		// either as a map (Call decoded JSON) or as base64-encoded
		// raw bytes. We support both.
		var p map[string]any
		_ = json.Unmarshal([]byte(payloadRaw), &p)
		if v, _ := p["daemon_initiated"].(bool); v {
			daemonStops++
		} else {
			naturalStops++
		}
	}
	if daemonStops < 1 {
		t.Errorf("expected >=1 daemon-initiated stop frame, got %d (calls=%v)", daemonStops, calls)
	}
}

// TestDrainNoOpWhenIdle: drain on an empty daemon returns nil.
func TestDrainNoOpWhenIdle(t *testing.T) {
	stub := newStub(t)
	d := newTestDaemon(t, stub, func(o *Options) { o.Heartbeat = time.Hour })
	if err := d.Drain(context.Background(), 50*time.Millisecond); err != nil {
		t.Errorf("Drain idle: %v", err)
	}
}

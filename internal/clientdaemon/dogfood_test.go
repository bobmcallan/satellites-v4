package clientdaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
	"io"
)

// TestDogfoodEndToEnd is the AC6 in-process dogfood: it boots the
// real Daemon.Run loop on a temp unix socket, POSTs /v1/enqueue with
// a synthetic task_id (whose task_get is pre-seeded on the stub
// substrate), watches the task_log_append calls land, and asserts
// the canonical start / heartbeat / stop wire shape — exactly what
// `satellites-client task run --follow` would surface to an operator
// over SSE.
//
// Equivalent to the operator-driven check named in plan §1.AC6:
//
//	satellites-client serve start --parallelism=1
//	curl --unix-socket ~/.satellites/daemon/satellites-client.sock \
//	  -X POST http://d/v1/enqueue -d '{"task_id":"<real_task>"}'
//	satellites-client task run --follow <real_task> 2>&1 | tee follow.log
//	# Assert follow.log contains "kind=start", "kind=heartbeat", "kind=stop".
//
// Replacing the real claude subprocess with an injectable Dispatch
// closure satisfies AC11's independent-ship rule and keeps the test
// hermetic.
func TestDogfoodEndToEnd(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_dogfood", map[string]any{
		"workspace_id": "wksp_dogfood",
		"project_id":   "proj_dogfood",
		"story_id":     "sty_5aa20f1b",
		"origin":       "story_stage",
	})

	// Dispatch closure mimics a real claude subprocess: writes a
	// few stdout/stderr bytes, sleeps long enough for the heartbeat
	// ticker to fire, then returns success.
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = 30 * time.Millisecond
		o.Dispatch = func(ctx context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, _ worker.TaskEnvelope, stdout, stderr io.Writer) (worker.Outcome, error) {
			_, _ = stdout.Write([]byte("dogfood claude line one\n"))
			_, _ = stderr.Write([]byte("dogfood claude warn\n"))
			select {
			case <-ctx.Done():
				return worker.OutcomeTimeout, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
			_, _ = stdout.Write([]byte("dogfood claude line two\n"))
			return worker.OutcomeSuccess, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.WaitInflight() }()
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	defer func() {
		select {
		case <-runDone:
		case <-time.After(3 * time.Second):
		}
	}()

	// Wait for the socket to bind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(d.opts.SocketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cli := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", d.opts.SocketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	// POST /v1/enqueue (the operator's curl equivalent).
	body, _ := json.Marshal(EnqueueRequest{TaskID: "task_dogfood"})
	resp, err := cli.Post("http://daemon/v1/enqueue", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enqueue status=%d, want 200", resp.StatusCode)
	}

	// Wait for the dispatch to complete and lifecycle frames to flush.
	deadline = time.Now().Add(4 * time.Second)
	var sawStart, sawHeartbeat, sawStop bool
	for time.Now().Before(deadline) {
		sawStart, sawHeartbeat, sawStop = false, false, false
		for _, c := range stub.appends() {
			kind, _ := c["kind"].(string)
			switch kind {
			case "start":
				sawStart = true
			case "heartbeat":
				sawHeartbeat = true
			case "stop":
				sawStop = true
			}
		}
		if sawStart && sawHeartbeat && sawStop {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !sawStart {
		t.Errorf("dogfood: no start frame")
	}
	if !sawHeartbeat {
		t.Errorf("dogfood: no heartbeat frame")
	}
	if !sawStop {
		t.Errorf("dogfood: no stop frame")
	}

	// Per-task log file shape: contains both stdout + stderr writes.
	logBody, err := os.ReadFile(d.opts.LogsDir + "/task_dogfood.log")
	if err != nil {
		t.Fatalf("dogfood: per-task log missing: %v", err)
	}
	if !bytes.Contains(logBody, []byte("dogfood claude line one")) || !bytes.Contains(logBody, []byte("dogfood claude warn")) {
		t.Errorf("dogfood: per-task log missing expected lines: %s", logBody)
	}
}

package clientdaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/agent/worker"
)

// unixClient builds a net/http.Client whose dialer dials the supplied
// unix socket path. URLs use "http://daemon" + path semantics.
func unixClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// TestSocketBindModeAndEndpoints (sty_5aa20f1b AC3) brings up a real
// daemon Run loop on a temp unix socket and exercises every endpoint.
func TestSocketBindModeAndEndpoints(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_e", map[string]any{"workspace_id": "w", "project_id": "p"})
	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.Dispatch = captureDispatch(t, make(chan dispatchCall, 4), 5*time.Second, worker.OutcomeSuccess)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(3 * time.Second):
		}
		d.WaitInflight()
	}()

	// Wait for the socket to bind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(d.opts.SocketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Mode 0600 check.
	st, err := os.Stat(d.opts.SocketPath)
	if err != nil {
		t.Fatalf("socket file not bound: %v", err)
	}
	mode := st.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", mode)
	}

	cli := unixClient(d.opts.SocketPath)

	// /v1/enqueue
	body, _ := json.Marshal(EnqueueRequest{TaskID: "task_e"})
	resp := mustPost(t, cli, "http://daemon/v1/enqueue", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/enqueue status=%d, want 200", resp.StatusCode)
	}
	var enqResp EnqueueResponse
	_ = json.NewDecoder(resp.Body).Decode(&enqResp)
	if enqResp.TaskID != "task_e" {
		t.Errorf("enqueue response task_id=%q", enqResp.TaskID)
	}

	// /v1/queued
	time.Sleep(100 * time.Millisecond)
	resp = mustGet(t, cli, "http://daemon/v1/queued")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/queued status=%d, want 200", resp.StatusCode)
	}
	var queued []QueuedEntry
	_ = json.NewDecoder(resp.Body).Decode(&queued)
	// May be either queued or running depending on scheduler timing.

	// /v1/status (daemon-wide)
	resp = mustGet(t, cli, "http://daemon/v1/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/status status=%d, want 200", resp.StatusCode)
	}
	var stResp StatusResponse
	_ = json.NewDecoder(resp.Body).Decode(&stResp)
	if stResp.Parallelism != 1 {
		t.Errorf("/v1/status parallelism=%d, want 1", stResp.Parallelism)
	}
	if stResp.DaemonPID == 0 {
		t.Errorf("/v1/status daemon_pid = 0")
	}

	// /v1/status?task_id=task_e
	resp = mustGet(t, cli, "http://daemon/v1/status?task_id=task_e")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/status?task_id=task_e status=%d, want 200", resp.StatusCode)
	}
	var perTask TaskStatusResponse
	_ = json.NewDecoder(resp.Body).Decode(&perTask)
	if perTask.State != "queued" && perTask.State != "running" {
		t.Errorf("per-task state=%q, want queued|running", perTask.State)
	}

	// /v1/status?task_id=absent → 404
	resp = mustGet(t, cli, "http://daemon/v1/status?task_id=task_absent")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("absent task status=%d, want 404", resp.StatusCode)
	}

	// /v1/cancel
	body, _ = json.Marshal(CancelRequest{TaskID: "task_e"})
	resp = mustPost(t, cli, "http://daemon/v1/cancel", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/cancel status=%d, want 200", resp.StatusCode)
	}

	// Cancelling a never-enqueued task → 404 (deterministic check
	// against /v1/cancel's not-found branch; repeated cancel of
	// task_e is timing-dependent on the worker goroutine's
	// de-registration, which is covered by TestCancelQueuedRemovesEntry).
	body, _ = json.Marshal(CancelRequest{TaskID: "task_never"})
	resp = mustPost(t, cli, "http://daemon/v1/cancel", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/cancel of unknown task status=%d, want 404", resp.StatusCode)
	}
}

// TestSocketParallelClients fires two concurrent /v1/status requests
// to confirm the unix listener accepts parallel connections.
func TestSocketParallelClients(t *testing.T) {
	stub := newStub(t)
	d := newTestDaemon(t, stub, func(o *Options) { o.Heartbeat = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(d.opts.SocketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cli := unixClient(d.opts.SocketPath)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := cli.Get("http://daemon/v1/status")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- nil
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("parallel client: %v", e)
		}
	}
}

func mustPost(t *testing.T, cli *http.Client, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return resp
}

func mustGet(t *testing.T, cli *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := cli.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

// silence vet
var _ = strings.Contains

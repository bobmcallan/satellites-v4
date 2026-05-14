// task_log_follower_test.go — sty_7fc607f5 AC4 verification.
//
// In-process httptest.Server emits hand-rolled SSE frames over two
// connections: the first closes mid-stream after frame 2, the
// follower reconnects with Last-Event-ID: 2, the server emits
// frames 3, 4, 5 on the second connection. Asserts:
//
//   - Exactly 5 lifecycle lines printed (one per frame).
//   - No duplicate prints across the reconnect boundary.
//   - The second request carried Last-Event-ID: 2.
//   - Print format matches `^\[<RFC3339>\] (start|heartbeat|stop) `.

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskLogFollowerReconnects(t *testing.T) {
	var (
		mu              sync.Mutex
		requestCount    atomic.Int32
		lastEventIDsSeen []string
	)

	// SSE frames the server emits. The first connection delivers
	// frames 1-2 then closes; the second delivers 3-5.
	frame := func(seq int64, kind, payload string) string {
		return fmt.Sprintf("id: %d\ndata: {\"seq\":%d,\"kind\":%q,\"ts\":\"2026-05-14T03:00:00Z\",\"payload\":%s}\n\n",
			seq, seq, kind, payload)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		mu.Lock()
		lastEventIDsSeen = append(lastEventIDsSeen, r.Header.Get("Last-Event-ID"))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer does not support Flusher")
			return
		}

		switch count {
		case 1:
			// Frames 1 (start), 2 (heartbeat); then close mid-stream
			// without a stop frame.
			_, _ = w.Write([]byte(frame(1, "start", `{"worker_pid":4242,"origin":"test"}`)))
			flusher.Flush()
			_, _ = w.Write([]byte(frame(2, "heartbeat", `{"elapsed_ms":1000}`)))
			flusher.Flush()
			// Returning closes the connection — the follower sees EOF.
		case 2:
			// Reconnect should carry Last-Event-ID: 2 (the seq of the
			// last printed frame). Server resumes with seq=3.
			_, _ = w.Write([]byte(frame(3, "heartbeat", `{"elapsed_ms":2000}`)))
			flusher.Flush()
			_, _ = w.Write([]byte(frame(4, "heartbeat", `{"elapsed_ms":3000}`)))
			flusher.Flush()
			_, _ = w.Write([]byte(frame(5, "stop", `{"outcome":"success","exit_code":0,"duration_ms":4000}`)))
			flusher.Flush()
		default:
			t.Errorf("unexpected request count %d", count)
		}
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := followTaskLog(ctx, followerConfig{
		serverURL: srv.URL,
		authToken: "test-bearer",
		taskID:    "task_test",
		out:       &out,
	})
	if err != nil {
		t.Fatalf("followTaskLog returned error: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lifecycle lines, got %d:\n%s", len(lines), out.String())
	}

	// Format check — every line matches the contract regex.
	lineRE := regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z\] (start|heartbeat|stop) `)
	for i, line := range lines {
		if !lineRE.MatchString(line) {
			t.Errorf("line %d does not match format regex: %q", i, line)
		}
	}

	// Kind sequence: start, heartbeat, heartbeat, heartbeat, stop.
	wantKinds := []string{"start", "heartbeat", "heartbeat", "heartbeat", "stop"}
	for i, want := range wantKinds {
		if !strings.Contains(lines[i], "] "+want+" ") {
			t.Errorf("line %d kind mismatch: %q (want %s)", i, lines[i], want)
		}
	}

	// Reconnect assertions.
	if got := requestCount.Load(); got != 2 {
		t.Errorf("expected 2 HTTP connections, got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastEventIDsSeen) != 2 {
		t.Fatalf("expected 2 Last-Event-ID observations, got %d", len(lastEventIDsSeen))
	}
	if lastEventIDsSeen[0] != "" {
		t.Errorf("first request Last-Event-ID = %q, want empty", lastEventIDsSeen[0])
	}
	if lastEventIDsSeen[1] != "2" {
		t.Errorf("second request Last-Event-ID = %q, want 2", lastEventIDsSeen[1])
	}
}

// TestTaskLogFollower_SkipsChunkKinds verifies stdout/stderr chunk
// frames do NOT produce a printed line — the follower prints only
// the three lifecycle kinds (start, heartbeat, stop) per AC2 + the
// scope-mismatch ruling in plan §6.
func TestTaskLogFollower_SkipsChunkKinds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			flusher.Flush()
		}
		write(`id: 1` + "\n" + `data: {"seq":1,"kind":"start","ts":"2026-05-14T03:00:00Z","payload":{"worker_pid":1}}` + "\n\n")
		write(`id: 2` + "\n" + `data: {"seq":2,"kind":"stdout","ts":"2026-05-14T03:00:01Z","payload":{"lines":["A"]}}` + "\n\n")
		write(`id: 3` + "\n" + `data: {"seq":3,"kind":"stderr","ts":"2026-05-14T03:00:02Z","payload":{"lines":["B"]}}` + "\n\n")
		write(`id: 4` + "\n" + `data: {"seq":4,"kind":"stop","ts":"2026-05-14T03:00:03Z","payload":{"outcome":"success","exit_code":0,"duration_ms":3000}}` + "\n\n")
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := followTaskLog(ctx, followerConfig{
		serverURL: srv.URL,
		authToken: "test",
		taskID:    "task_skip",
		out:       &out,
	}); err != nil {
		t.Fatalf("followTaskLog: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "] stdout ") {
		t.Errorf("follower printed a stdout chunk line; should be skipped: %s", got)
	}
	if strings.Contains(got, "] stderr ") {
		t.Errorf("follower printed a stderr chunk line; should be skipped: %s", got)
	}
	for _, want := range []string{"] start ", "] heartbeat", "] stop "} {
		if want == "] heartbeat" {
			continue // not emitted in this fixture
		}
		if !strings.Contains(got, want) {
			t.Errorf("missing line marker %q in output: %s", want, got)
		}
	}
}

// TestTaskLogFollower_StopClosesStream verifies a clean stop frame
// terminates the loop with nil error (no reconnect attempt).
func TestTaskLogFollower_StopClosesStream(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`id: 1` + "\n" + `data: {"seq":1,"kind":"start","ts":"2026-05-14T03:00:00Z","payload":{"worker_pid":1}}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte(`id: 2` + "\n" + `data: {"seq":2,"kind":"stop","ts":"2026-05-14T03:00:01Z","payload":{"outcome":"success","exit_code":0,"duration_ms":1000}}` + "\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := followTaskLog(ctx, followerConfig{
		serverURL: srv.URL,
		authToken: "test",
		taskID:    "task_stop",
		out:       &out,
	}); err != nil {
		t.Fatalf("followTaskLog returned non-nil error: %v", err)
	}
	if got := reqCount.Load(); got != 1 {
		t.Errorf("expected 1 HTTP request (no reconnect after stop), got %d", got)
	}
}

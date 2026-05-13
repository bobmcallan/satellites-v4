package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// TestTaskRunSetsSkipUpdateCheckEnv asserts the literal env wiring
// task_run.go uses to suppress the boot drift check in dispatched
// subprocesses (sty_64e69db8). The dispatcher reads os.Environ()
// when composing the spawned cmd.Env (see
// internal/agent/worker/client_claude.go), so setting the env on
// the parent before RunDispatched is the canonical mechanism.
func TestTaskRunSetsSkipUpdateCheckEnv(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, `os.Setenv(driftEnvSkip, "1")`) {
		t.Fatalf(`task_run.go does not set the SATELLITES_CLIENT_SKIP_UPDATE_CHECK env via os.Setenv(driftEnvSkip, "1")`)
	}
}

// TestTaskRunSetsDisableTelemetryEnv asserts task_run sets the
// SATELLITES_CLIENT_DISABLE_TELEMETRY env before dispatch so the
// spawned subprocess inherits it (sty_8c17b89d recursion guard).
func TestTaskRunSetsDisableTelemetryEnv(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, `os.Setenv(telemetryEnvSkip, "1")`) {
		t.Fatalf(`task_run.go does not set the SATELLITES_CLIENT_DISABLE_TELEMETRY env via os.Setenv(telemetryEnvSkip, "1")`)
	}
	if !strings.Contains(src, `disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"`) {
		t.Fatalf(`task_run.go does not read os.Getenv(telemetryEnvSkip) at the top of runTaskCmd`)
	}
}

// recordedAppend captures one task_log_append request the uploader
// sent against the test stub server.
type recordedAppend struct {
	Path    string
	TaskID  string
	Seq     int64
	Kind    string
	Payload json.RawMessage
}

// newTaskLogStub stands up an httptest.Server that records every
// /api/v1/task/log/append POST. Returns the URL + a snapshot accessor.
func newTaskLogStub(t *testing.T) (*httptest.Server, func() []recordedAppend) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedAppend
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/task/log/append" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			TaskID  string          `json:"task_id"`
			Seq     int64           `json:"seq"`
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		calls = append(calls, recordedAppend{
			Path:    r.URL.Path,
			TaskID:  req.TaskID,
			Seq:     req.Seq,
			Kind:    req.Kind,
			Payload: req.Payload,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"tlog_x","task_id":"` + req.TaskID + `","seq":0,"kind":"` + req.Kind + `","created_at":"2026-05-13T12:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedAppend {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedAppend, len(calls))
		copy(out, calls)
		return out
	}
}

// TestTaskRun_EmitsStartHeartbeatStop exercises the lifecycle emit
// path directly. The dispatch-claude flow is too heavy for a unit
// test, so we call emitLifecycle + runHeartbeat against a stub server
// to assert the wire shape and event sequencing.
func TestTaskRun_EmitsStartHeartbeatStop(t *testing.T) {
	srv, snapshot := newTaskLogStub(t)
	api := cliremote.New(srv.URL, "test-bearer", nil)
	logger := satarbor.New("info")

	var seq atomic.Int64
	startSeq := seq.Add(1) - 1
	startedAt := time.Now().UTC()
	emitLifecycle(context.Background(), api, logger, "task_x", "wksp_x", "proj_x", startSeq, "start", map[string]any{"worker_pid": 42})

	stop := make(chan struct{})
	done := make(chan struct{})
	go runHeartbeat(api, logger, "task_x", "wksp_x", "proj_x", &seq, startedAt, 30*time.Millisecond, stop, done)
	time.Sleep(120 * time.Millisecond)
	close(stop)
	<-done

	stopSeq := seq.Add(1) - 1
	emitLifecycle(context.Background(), api, logger, "task_x", "wksp_x", "proj_x", stopSeq, "stop", map[string]any{"outcome": "success"})

	calls := snapshot()
	kinds := []string{}
	for _, c := range calls {
		kinds = append(kinds, c.Kind)
	}
	if len(calls) < 3 {
		t.Fatalf("expected ≥3 calls (start + ≥1 heartbeat + stop), got %d: %v", len(calls), kinds)
	}
	if calls[0].Kind != "start" {
		t.Errorf("first call kind = %q, want start", calls[0].Kind)
	}
	if calls[len(calls)-1].Kind != "stop" {
		t.Errorf("last call kind = %q, want stop", calls[len(calls)-1].Kind)
	}
	sawHeartbeat := false
	for _, k := range kinds[1 : len(kinds)-1] {
		if k == "heartbeat" {
			sawHeartbeat = true
			break
		}
	}
	if !sawHeartbeat {
		t.Errorf("no heartbeat seen between start + stop. kinds=%v", kinds)
	}
}

// TestTaskRun_UploadsStdoutStderrChunksInOrder feeds N lines through
// the uploader and verifies the resulting task_log_append calls carry
// every line in order with strictly-monotonic seq.
func TestTaskRun_UploadsStdoutStderrChunksInOrder(t *testing.T) {
	srv, snapshot := newTaskLogStub(t)
	api := cliremote.New(srv.URL, "test-bearer", nil)
	logger := satarbor.New("info")
	var seq atomic.Int64
	nextSeq := func() int64 { return seq.Add(1) - 1 }
	u := newTaskLogUploader(api, logger, "task_x", "wksp_x", "proj_x", "stdout", nextSeq)

	const N = 250
	var b strings.Builder
	expected := make([]string, 0, N)
	for i := 0; i < N; i++ {
		line := "stdout-line-" + strings.Repeat("a", 5) + "-" + intToStr(i)
		expected = append(expected, line)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if _, err := u.Write([]byte(b.String())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	u.Close()

	calls := snapshot()
	if len(calls) == 0 {
		t.Fatalf("no chunks uploaded")
	}
	// Validate seq strictly monotonic + concatenated lines reproduce
	// the fixture stdout.
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Seq < calls[j].Seq })
	got := make([]string, 0, N)
	prevSeq := int64(-1)
	for _, c := range calls {
		if c.Kind != "stdout" {
			t.Errorf("unexpected kind %q (call seq=%d)", c.Kind, c.Seq)
		}
		if c.Seq <= prevSeq {
			t.Errorf("seq not monotonic: %d <= %d", c.Seq, prevSeq)
		}
		prevSeq = c.Seq
		var payload struct {
			Lines []string `json:"lines"`
		}
		if err := json.Unmarshal(c.Payload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		got = append(got, payload.Lines...)
	}
	if len(got) != N {
		t.Fatalf("got %d lines, want %d", len(got), N)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Errorf("line[%d] = %q, want %q", i, got[i], expected[i])
			break
		}
	}
	// Threshold check: 250 lines @ flush=100 produces ≥2 flushes (the
	// time tick will pick up the trailing 50).
	if len(calls) < 2 {
		t.Errorf("expected ≥2 chunks, got %d", len(calls))
	}
}

// TestTaskRun_RecursionGuardDisablesTelemetryWhenEnvSet asserts that
// runTaskCmd's first line is the env-var check and the parent setter
// uses the literal value "1" — the recursion guard from sty_8c17b89d.
func TestTaskRun_RecursionGuardDisablesTelemetryWhenEnvSet(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)

	// Constant is named telemetryEnvSkip.
	if !strings.Contains(src, `telemetryEnvSkip = "SATELLITES_CLIENT_DISABLE_TELEMETRY"`) {
		t.Fatalf(`task_run.go missing constant telemetryEnvSkip = "SATELLITES_CLIENT_DISABLE_TELEMETRY"`)
	}
	// Parent setter — passed through os.Setenv.
	if !strings.Contains(src, `os.Setenv(telemetryEnvSkip, "1")`) {
		t.Fatalf(`task_run.go missing os.Setenv(telemetryEnvSkip, "1") inside the parent branch`)
	}
	// Child branch — disableTelemetry gate.
	if !strings.Contains(src, `disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"`) {
		t.Fatalf(`task_run.go missing disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"`)
	}
	// And uses the disableTelemetry flag to skip the lifecycle hooks.
	if !strings.Contains(src, "if !disableTelemetry") && !strings.Contains(src, "if !runDisabled") {
		t.Fatalf(`task_run.go does not branch on the disableTelemetry flag`)
	}
}

// intToStr is a stdlib-light helper (strconv would also do).
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	out := []byte{}
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

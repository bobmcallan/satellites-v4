package dispatchteam

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/agent/worker"
)

type recordedAppend struct {
	Path    string
	TaskID  string
	Seq     int64
	Kind    string
	Payload json.RawMessage
}

type recordedLedger struct {
	Path string
	Body json.RawMessage
}

func newStub(t *testing.T) (*httptest.Server, func() []recordedAppend, func() []recordedLedger) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedAppend
	var ledgers []recordedLedger
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/v1/task/log/append":
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
		case "/api/v1/ledger/append":
			mu.Lock()
			ledgers = append(ledgers, recordedLedger{Path: r.URL.Path, Body: append(json.RawMessage(nil), raw...)})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ldg_x"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv,
		func() []recordedAppend {
			mu.Lock()
			defer mu.Unlock()
			out := make([]recordedAppend, len(calls))
			copy(out, calls)
			return out
		},
		func() []recordedLedger {
			mu.Lock()
			defer mu.Unlock()
			out := make([]recordedLedger, len(ledgers))
			copy(out, ledgers)
			return out
		}
}

// TestEmitsStartHeartbeatStop covers the wire shape for start, heartbeat, and stop frames.
func TestEmitsStartHeartbeatStop(t *testing.T) {
	srv, snapshot, _ := newStub(t)
	api := cliremote.New(srv.URL, "test-bearer", nil)
	logger := satarbor.New("info")

	var seq atomic.Int64
	startSeq := seq.Add(1) - 1
	startedAt := time.Now().UTC()
	EmitLifecycle(context.Background(), api, logger, "task_x", "wksp_x", "proj_x", startSeq, "start", map[string]any{
		"worker_pid":   42,
		"workspace_id": "wksp_x",
		"project_id":   "proj_x",
		"story_id":     "sty_x",
		"origin":       "story_stage",
	})

	stop := make(chan struct{})
	done := make(chan struct{})
	go RunHeartbeat(api, logger, "task_x", "wksp_x", "proj_x", &seq, startedAt, 30*time.Millisecond, stop, done)
	time.Sleep(120 * time.Millisecond)
	close(stop)
	<-done

	stopSeq := seq.Add(1) - 1
	EmitLifecycle(context.Background(), api, logger, "task_x", "wksp_x", "proj_x", stopSeq, "stop", map[string]any{
		"outcome":     "success",
		"exit_code":   0,
		"duration_ms": int64(123),
	})

	calls := snapshot()
	if len(calls) < 3 {
		t.Fatalf("want >=3 calls (start+>=1 heartbeat+stop), got %d", len(calls))
	}
	if calls[0].Kind != "start" {
		t.Errorf("first call kind=%q, want start", calls[0].Kind)
	}
	if calls[len(calls)-1].Kind != "stop" {
		t.Errorf("last call kind=%q, want stop", calls[len(calls)-1].Kind)
	}

	var startPayload map[string]any
	if err := json.Unmarshal(calls[0].Payload, &startPayload); err != nil {
		t.Fatalf("decode start payload: %v", err)
	}
	for _, k := range []string{"worker_pid", "workspace_id", "project_id", "story_id", "origin"} {
		if _, ok := startPayload[k]; !ok {
			t.Errorf("start payload missing key %q (got %v)", k, startPayload)
		}
	}

	var sawHeartbeat bool
	for _, c := range calls[1 : len(calls)-1] {
		if c.Kind != "heartbeat" {
			continue
		}
		sawHeartbeat = true
		var p map[string]any
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			t.Fatalf("decode heartbeat payload: %v", err)
		}
		if _, ok := p["elapsed_ms"]; !ok {
			t.Errorf("heartbeat payload missing elapsed_ms (got %v)", p)
		}
	}
	if !sawHeartbeat {
		t.Errorf("no heartbeat seen between start+stop")
	}

	var stopPayload map[string]any
	if err := json.Unmarshal(calls[len(calls)-1].Payload, &stopPayload); err != nil {
		t.Fatalf("decode stop payload: %v", err)
	}
	for _, k := range []string{"outcome", "exit_code", "duration_ms"} {
		if _, ok := stopPayload[k]; !ok {
			t.Errorf("stop payload missing key %q (got %v)", k, stopPayload)
		}
	}
}

// TestUploaderUploadsChunksInOrder reproduces sty_8c17b89d's chunk-shape coverage on the new package.
func TestUploaderUploadsChunksInOrder(t *testing.T) {
	srv, snapshot, _ := newStub(t)
	api := cliremote.New(srv.URL, "test-bearer", nil)
	logger := satarbor.New("info")
	var seq atomic.Int64
	nextSeq := func() int64 { return seq.Add(1) - 1 }
	u := NewTaskLogUploader(api, logger, "task_x", "wksp_x", "proj_x", "stdout", nextSeq)

	const N = 250
	var b strings.Builder
	expected := make([]string, 0, N)
	for i := 0; i < N; i++ {
		line := "stdout-line-" + strings.Repeat("a", 5) + "-" + strconv.Itoa(i)
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
	if len(calls) < 2 {
		t.Errorf("want >=2 chunks, got %d", len(calls))
	}
}

// TestRunWiresFullLifecycle runs the full Run() shell against the stub
// and asserts: start present, heartbeat present (cadence honoured),
// stop present with outcome+duration, ledger pointer row present.
func TestRunWiresFullLifecycle(t *testing.T) {
	srv, snapshot, ledgerSnap := newStub(t)
	api := cliremote.New(srv.URL, "test-bearer", nil)
	logger := satarbor.New("info")

	in := Inputs{
		TaskID:      "task_run",
		WorkspaceID: "wksp_x",
		ProjectID:   "proj_x",
		StoryID:     "sty_x",
		Origin:      "story_stage",
		API:         api,
		Logger:      logger,
		Stdout:      io.Discard,
		Stderr:      io.Discard,
		Heartbeat:   25 * time.Millisecond,
	}
	dispatch := func(ctx context.Context, stdout, stderr io.Writer) (worker.Outcome, error) {
		_, _ = stdout.Write([]byte("hello\n"))
		_, _ = stderr.Write([]byte("warn\n"))
		time.Sleep(80 * time.Millisecond)
		return worker.OutcomeSuccess, nil
	}
	out, err := Run(context.Background(), in, dispatch)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != worker.OutcomeSuccess {
		t.Fatalf("outcome = %q, want success", out)
	}

	calls := snapshot()
	if len(calls) < 3 {
		t.Fatalf("want >=3 lifecycle calls, got %d", len(calls))
	}
	if calls[0].Kind != "start" {
		t.Errorf("first call kind=%q, want start", calls[0].Kind)
	}

	var sawStop, sawHeartbeat bool
	for _, c := range calls {
		if c.Kind == "stop" {
			sawStop = true
			var p map[string]any
			if err := json.Unmarshal(c.Payload, &p); err != nil {
				t.Fatalf("decode stop: %v", err)
			}
			if p["outcome"] != "success" {
				t.Errorf("stop outcome=%v, want success", p["outcome"])
			}
		}
		if c.Kind == "heartbeat" {
			sawHeartbeat = true
		}
	}
	if !sawStop {
		t.Errorf("no stop frame emitted")
	}
	if !sawHeartbeat {
		t.Errorf("no heartbeat frame emitted (cadence too slow vs sleep?)")
	}

	if len(ledgerSnap()) == 0 {
		t.Errorf("expected at least one ledger_append (task-log pointer)")
	}
}

// TestRunStopExtraMergedAndSuppressionRespected asserts the daemon-side
// shape: StopExtra merged into the stop payload; SuppressStop=true
// causes the stop frame + ledger pointer to be skipped.
func TestRunStopExtraMergedAndSuppressionRespected(t *testing.T) {
	t.Run("StopExtraMerged", func(t *testing.T) {
		srv, snapshot, _ := newStub(t)
		api := cliremote.New(srv.URL, "test-bearer", nil)
		in := Inputs{
			TaskID: "task_extra", WorkspaceID: "w", ProjectID: "p",
			API: api, Stdout: io.Discard, Stderr: io.Discard,
			Heartbeat: time.Hour, // suppress heartbeats
			StopExtra: map[string]any{"daemon_initiated": true},
		}
		dispatch := func(ctx context.Context, _, _ io.Writer) (worker.Outcome, error) {
			return worker.OutcomeTimeout, errors.New("simulated timeout")
		}
		_, _ = Run(context.Background(), in, dispatch)
		calls := snapshot()
		var stop *recordedAppend
		for i := range calls {
			if calls[i].Kind == "stop" {
				c := calls[i]
				stop = &c
				break
			}
		}
		if stop == nil {
			t.Fatalf("no stop frame")
		}
		var p map[string]any
		if err := json.Unmarshal(stop.Payload, &p); err != nil {
			t.Fatalf("decode stop: %v", err)
		}
		if v, _ := p["daemon_initiated"].(bool); !v {
			t.Errorf("daemon_initiated not merged into stop payload (got %v)", p)
		}
		if p["outcome"] != "timeout" {
			t.Errorf("outcome=%v, want timeout", p["outcome"])
		}
	})

	t.Run("SuppressStop", func(t *testing.T) {
		srv, snapshot, ledgerSnap := newStub(t)
		api := cliremote.New(srv.URL, "test-bearer", nil)
		var suppress atomic.Bool
		suppress.Store(true)
		in := Inputs{
			TaskID: "task_suppress", WorkspaceID: "w", ProjectID: "p",
			API: api, Stdout: io.Discard, Stderr: io.Discard,
			Heartbeat:    time.Hour,
			SuppressStop: &suppress,
		}
		_, _ = Run(context.Background(), in, func(ctx context.Context, _, _ io.Writer) (worker.Outcome, error) {
			return worker.OutcomeSuccess, nil
		})
		for _, c := range snapshot() {
			if c.Kind == "stop" {
				t.Errorf("SuppressStop set but stop frame emitted (seq=%d)", c.Seq)
			}
		}
		if len(ledgerSnap()) > 0 {
			t.Errorf("SuppressStop set but task-log pointer ledger row emitted (n=%d)", len(ledgerSnap()))
		}
	})
}

// TestRunDisableTelemetrySkipsAllEmission covers the recursion-guard
// branch: when DisableTelemetry is true, the dispatch closure runs
// against the raw Stdout/Stderr writers and no task_log_append calls
// are made.
func TestRunDisableTelemetrySkipsAllEmission(t *testing.T) {
	srv, snapshot, ledgerSnap := newStub(t)
	api := cliremote.New(srv.URL, "test-bearer", nil)
	in := Inputs{
		TaskID: "task_silent", WorkspaceID: "w", ProjectID: "p",
		API: api, Stdout: io.Discard, Stderr: io.Discard,
		DisableTelemetry: true,
	}
	_, _ = Run(context.Background(), in, func(ctx context.Context, stdout, stderr io.Writer) (worker.Outcome, error) {
		_, _ = stdout.Write([]byte("noisy\n"))
		return worker.OutcomeSuccess, nil
	})
	if n := len(snapshot()); n != 0 {
		t.Errorf("DisableTelemetry: want 0 task_log_append calls, got %d", n)
	}
	if n := len(ledgerSnap()); n != 0 {
		t.Errorf("DisableTelemetry: want 0 ledger_append calls, got %d", n)
	}
}

// TestTelemetryEnvSkipConst guards the literal env-var name so the
// CLI's env-set-on-subprocess branch and the dispatched child's
// env-read branch refer to the same string.
func TestTelemetryEnvSkipConst(t *testing.T) {
	if TelemetryEnvSkip != "SATELLITES_CLIENT_DISABLE_TELEMETRY" {
		t.Errorf("TelemetryEnvSkip = %q, want SATELLITES_CLIENT_DISABLE_TELEMETRY", TelemetryEnvSkip)
	}
}

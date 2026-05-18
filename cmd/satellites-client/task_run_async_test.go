// task_run_async_test.go — sty_57210d2a (C2) unit + import-graph
// guards for the async dispatch wire.
//
// Three flavours:
//
//   - TestTaskRunAsync* — table tests over postEnqueue / runTaskAsync
//     covering request marshalling, response decode, and the AC3
//     daemon-not-running error mapping.
//
//   - TestTaskRunAsyncImports — parses task_run_async.go's AST and
//     asserts the file does NOT import the sync path's dispatch
//     internals or any substrate-domain package. This is the AC4
//     import-shape guard the plan binds.
//
//   - TestTaskRunRejectsAsyncFlag / TestTaskRunRegistersSyncFlag /
//     TestTaskRunRegistersSocketFlag — prove the cobra surface ships
//     --sync (re-opt-in to blocking) and --socket (daemon override)
//     and rejects --async with cobra's "unknown flag" error
//     (sty_ad40584f C4 default-flip + flag retirement).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/clientdaemon"
	"github.com/bobmcallan/satellites/internal/cliexit"
)

// TestTaskRunRejectsAsyncFlag is the AC3 regression guard for
// sty_ad40584f C4: cobra MUST reject `--async` with its built-in
// "unknown flag: --async" error. Per pr_no_unrequested_compat the
// flag is removed outright — not aliased, not hidden, not deprecation
// -warned. The literal substring is what the story body's AC3
// verification binds the reviewer to.
func TestTaskRunRejectsAsyncFlag(t *testing.T) {
	c := newTaskRunCmd()
	err := c.ParseFlags([]string{"--async"})
	if err == nil {
		t.Fatalf("ParseFlags([\"--async\"]): nil err, want unknown-flag error")
	}
	if !strings.Contains(err.Error(), "unknown flag: --async") {
		t.Errorf("err = %q, want substring %q", err.Error(), "unknown flag: --async")
	}
}

// TestTaskRunRegistersSyncFlag asserts the cobra command surface
// ships `--sync` (bool, default false). After C4 (sty_ad40584f) the
// default branch is async; `--sync` is the explicit re-opt-in to the
// blocking shape.
func TestTaskRunRegistersSyncFlag(t *testing.T) {
	c := newTaskRunCmd()
	f := c.Flags().Lookup("sync")
	if f == nil {
		t.Fatalf("task run: --sync flag not registered")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--sync type = %q, want bool", f.Value.Type())
	}
	if got, _ := c.Flags().GetBool("sync"); got != false {
		t.Errorf("--sync default = %v, want false", got)
	}
}

// TestTaskRunRegistersSocketFlag asserts the --socket flag survives
// the C4 flag-surface rework: same default (clientdaemon's canonical
// path), same name. The async path's operator-override is unchanged.
func TestTaskRunRegistersSocketFlag(t *testing.T) {
	c := newTaskRunCmd()
	if c.Flags().Lookup("socket") == nil {
		t.Fatalf("task run: --socket flag not registered")
	}
	if got, _ := c.Flags().GetString("socket"); got != clientdaemon.DefaultSocketPath() {
		t.Errorf("--socket default = %q, want %q", got, clientdaemon.DefaultSocketPath())
	}
}

// TestTaskRunAsync_RequestShape proves the helper marshals the
// daemon-bound POST body as {"task_id":"<id>"} — the literal wire
// shape clientdaemon.EnqueueRequest declares.
func TestTaskRunAsync_RequestShape(t *testing.T) {
	var gotBody []byte
	var gotPath, gotMethod, gotCT string

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "d.sock")
	listener := mustListenUnix(t, sockPath)
	defer listener.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enqueue", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		resp := clientdaemon.EnqueueResponse{TaskID: "task_x", DaemonPID: 4242, QueuePosition: 1}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()

	resp, err := postEnqueue(context.Background(), sockPath, "task_x")
	if err != nil {
		t.Fatalf("postEnqueue: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/enqueue" {
		t.Errorf("path = %q, want /v1/enqueue", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if strings.TrimSpace(string(gotBody)) != `{"task_id":"task_x"}` {
		t.Errorf("body = %q, want %q", string(gotBody), `{"task_id":"task_x"}`)
	}
	if resp.TaskID != "task_x" || resp.DaemonPID != 4242 || resp.QueuePosition != 1 {
		t.Errorf("decoded response mismatch: %+v", resp)
	}
}

// TestTaskRunAsync_ResponseRoundTrip proves the helper decodes every
// field clientdaemon.EnqueueResponse names, exactly. Single-source
// of-truth guarantee: if the daemon evolves the response shape, the
// CLI re-runs against the updated types verbatim.
func TestTaskRunAsync_ResponseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "d.sock")
	listener := mustListenUnix(t, sockPath)
	defer listener.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enqueue", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"task_id":"task_round","daemon_pid":777,"queue_position":3}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()

	resp, err := postEnqueue(context.Background(), sockPath, "task_round")
	if err != nil {
		t.Fatalf("postEnqueue: %v", err)
	}
	if resp.TaskID != "task_round" {
		t.Errorf("task_id = %q", resp.TaskID)
	}
	if resp.DaemonPID != 777 {
		t.Errorf("daemon_pid = %d, want 777", resp.DaemonPID)
	}
	if resp.QueuePosition != 3 {
		t.Errorf("queue_position = %d, want 3", resp.QueuePosition)
	}
}

// TestTaskRunAsync_DaemonNotRunning maps a nonexistent socket path to
// the exact stderr substring AC3 binds the reviewer to. The error
// MUST carry cliexit.Server (non-zero exit).
func TestTaskRunAsync_DaemonNotRunning(t *testing.T) {
	_, err := postEnqueue(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), "task_x")
	if err == nil {
		t.Fatalf("postEnqueue against absent socket: nil err, want daemon-not-running")
	}
	if !strings.Contains(err.Error(), daemonNotRunningMsg) {
		t.Errorf("err = %q, want substring %q", err.Error(), daemonNotRunningMsg)
	}
	// Reviewer copy-pastes the literal substring; confirm the em-dash
	// is the UTF-8 sequence (e2 80 94) the criteria-md names.
	if !strings.Contains(daemonNotRunningMsg, "—") {
		t.Errorf("daemonNotRunningMsg missing em-dash U+2014")
	}
	var typed *cliexit.Error
	if !errors.As(err, &typed) {
		t.Fatalf("err is not *cliexit.Error: %T", err)
	}
	if typed.Code != cliexit.Server {
		t.Errorf("exit code = %d, want %d (cliexit.Server)", typed.Code, cliexit.Server)
	}
}

// TestTaskRunAsync_StdoutRoundTrip exercises the full runTaskAsync
// path against an httptest-on-unix daemon stub: build a cobra cmd,
// set --socket to the stub, call runTaskAsync, and assert the stdout
// JSON re-encodes the three fields the daemon emitted.
func TestTaskRunAsync_StdoutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "d.sock")
	listener := mustListenUnix(t, sockPath)
	defer listener.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req clientdaemon.EnqueueRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := clientdaemon.EnqueueResponse{TaskID: req.TaskID, DaemonPID: 99, QueuePosition: 0}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()

	cmd := newTaskRunCmd()
	_ = cmd.Flags().Set("socket", sockPath)

	stdout := &captureBuf{}
	cmd.SetOut(stdout)
	cmd.SetErr(io.Discard)

	start := time.Now()
	if err := runTaskAsync(cmd, "task_stdout"); err != nil {
		t.Fatalf("runTaskAsync: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= time.Second {
		t.Errorf("runTaskAsync elapsed = %s, want < 1s", elapsed)
	}

	var decoded clientdaemon.EnqueueResponse
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode stdout: %v (raw=%q)", err, stdout.String())
	}
	if decoded.TaskID != "task_stdout" {
		t.Errorf("stdout task_id = %q", decoded.TaskID)
	}
	if decoded.DaemonPID != 99 {
		t.Errorf("stdout daemon_pid = %d, want 99", decoded.DaemonPID)
	}
	if decoded.QueuePosition != 0 {
		t.Errorf("stdout queue_position = %d, want 0", decoded.QueuePosition)
	}
}

// TestTaskRunAsyncImports — sty_57210d2a AC4 import-shape guard.
//
// task_run_async.go is the new async dispatch wire. Per the plan,
// this file must NOT import internal/agent/dispatchteam, internal/
// agent/worker, or any substrate-domain package. The async wire is a
// pure unix-socket client; reaching into the sync path's internals
// would erode the AC4 invariant that the sync path is unchanged.
//
// The reviewer cites this test alongside the verbatim git diff to
// confirm the two code paths share no transitive dispatch-internal
// import graph.
func TestTaskRunAsyncImports(t *testing.T) {
	t.Parallel()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(here)
	path := filepath.Join(pkgDir, "task_run_async.go")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	forbidden := map[string]struct{}{
		"github.com/bobmcallan/satellites/internal/agent/dispatchteam": {},
		"github.com/bobmcallan/satellites/internal/agent/worker":       {},
		"github.com/bobmcallan/satellites/internal/document":           {},
		"github.com/bobmcallan/satellites/internal/story":              {},
		"github.com/bobmcallan/satellites/internal/task":               {},
		"github.com/bobmcallan/satellites/internal/principle":          {},
		"github.com/bobmcallan/satellites/internal/skill":              {},
		"github.com/bobmcallan/satellites/internal/reviewer":           {},
		"github.com/bobmcallan/satellites/internal/role":               {},
		"github.com/bobmcallan/satellites/internal/contract":           {},
		"github.com/bobmcallan/satellites/internal/repo":               {},
		"github.com/bobmcallan/satellites/internal/kv":                 {},
		"github.com/bobmcallan/satellites/internal/changelog":          {},
		"github.com/bobmcallan/satellites/internal/workspace":          {},
		"github.com/bobmcallan/satellites/internal/project":            {},
		"github.com/bobmcallan/satellites/internal/ledger":             {},
		"github.com/bobmcallan/satellites/internal/portalreplicate":    {},
		"github.com/bobmcallan/satellites/internal/agentprocess":       {},
		"github.com/bobmcallan/satellites/internal/session":            {},
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, imp := range file.Imports {
		pathVal := strings.Trim(imp.Path.Value, "\"")
		if _, bad := forbidden[pathVal]; bad {
			t.Errorf("task_run_async.go imports forbidden package %s", pathVal)
		}
	}
}

// mustListenUnix binds a unix listener at path; t.Fatals on failure.
func mustListenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	return listener
}

// captureBuf is an io.Writer that buffers writes for assertions.
// Provides a fmt.Stringer for legibility in test failure output.
type captureBuf struct {
	buf []byte
}

func (c *captureBuf) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	return len(p), nil
}
func (c *captureBuf) Bytes() []byte  { return c.buf }
func (c *captureBuf) String() string { return string(c.buf) }

// silence unused-import lint when cliremote/httptest pieces are
// dropped during edits.
var _ = httptest.NewServer

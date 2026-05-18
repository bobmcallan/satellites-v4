package clientdaemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// stubServer records HTTP calls and returns canned responses for the
// substrate verbs the daemon invokes during a unit test.
type stubServer struct {
	mu sync.Mutex

	taskGet        map[string]map[string]any // task_id -> response payload
	taskLogAppends []map[string]any
	ledgerAppends  []map[string]any

	srv *httptest.Server
}

func newStub(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{taskGet: map[string]map[string]any{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubServer) URL() string { return s.srv.URL }

func (s *stubServer) addTask(taskID string, env map[string]any) {
	if env == nil {
		env = map[string]any{}
	}
	if _, ok := env["id"]; !ok {
		env["id"] = taskID
	}
	s.mu.Lock()
	s.taskGet[taskID] = env
	s.mu.Unlock()
}

func (s *stubServer) appends() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.taskLogAppends))
	copy(out, s.taskLogAppends)
	return out
}

func (s *stubServer) ledger() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.ledgerAppends))
	copy(out, s.ledgerAppends)
	return out
}

func (s *stubServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var args map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &args)
	}
	switch r.URL.Path {
	case "/api/v1/task/get":
		taskID, _ := args["id"].(string)
		s.mu.Lock()
		env, ok := s.taskGet[taskID]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(env)
	case "/api/v1/task/log/append":
		s.mu.Lock()
		s.taskLogAppends = append(s.taskLogAppends, args)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"tlog_x","task_id":"x","seq":0,"kind":"x","created_at":"2026-05-13T12:00:00Z"}`))
	case "/api/v1/ledger/append":
		s.mu.Lock()
		s.ledgerAppends = append(s.ledgerAppends, args)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ldg_x"}`))
	default:
		http.NotFound(w, r)
	}
}

// captureDispatch returns a DispatchFunc that records every invocation
// into the supplied channel + sleeps for `runFor` before returning the
// supplied outcome.
type dispatchCall struct {
	Cfg    config.AgentConfig
	Env    worker.TaskEnvelope
	Stdout string
	Stderr string
}

func captureDispatch(t *testing.T, calls chan<- dispatchCall, runFor time.Duration, outcome worker.Outcome) DispatchFunc {
	t.Helper()
	return func(ctx context.Context, cfg config.AgentConfig, _ arbor.ILogger, api *cliremote.Client, env worker.TaskEnvelope, stdout, stderr io.Writer) (worker.Outcome, error) {
		_, _ = stdout.Write([]byte("dispatch-stdout\n"))
		_, _ = stderr.Write([]byte("dispatch-stderr\n"))
		select {
		case <-ctx.Done():
		case <-time.After(runFor):
		}
		select {
		case calls <- dispatchCall{Cfg: cfg, Env: env}:
		default:
		}
		return outcome, nil
	}
}

func newTestDaemon(t *testing.T, stub *stubServer, opts func(*Options)) *Daemon {
	t.Helper()
	tmp := t.TempDir()
	api := cliremote.New(stub.URL(), "test-bearer", nil)
	o := Options{
		Parallelism:  2,
		MaxQueue:     10,
		Heartbeat:    50 * time.Millisecond,
		DrainTimeout: 200 * time.Millisecond,
		SocketPath:   filepath.Join(tmp, "daemon.sock"),
		PidfilePath:  filepath.Join(tmp, "daemon.pid"),
		StatePath:    filepath.Join(tmp, "state.json"),
		LogfilePath:  filepath.Join(tmp, "daemon.log"),
		LogsDir:      filepath.Join(tmp, "logs"),
		AgentConfig: config.AgentConfig{
			RepoPath:         tmp,
			BranchTemplate:   "client-{task_id}-from-{base_sha}",
			WorktreeRoot:     filepath.Join(tmp, "worktree"),
			ClaudeBinaryPath: "/bin/true",
			SpawnMCPURL:      stub.URL() + "/mcp",
			AuthToken:        "test-bearer",
			ExecuteTimeout:   30 * time.Second,
			LogLevel:         "info",
		},
		API:    api,
		Logger: satarbor.New("info"),
		Dispatch: func(_ context.Context, _ config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, _ worker.TaskEnvelope, _, _ io.Writer) (worker.Outcome, error) {
			return worker.OutcomeSuccess, nil
		},
	}
	if opts != nil {
		opts(&o)
	}
	d, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

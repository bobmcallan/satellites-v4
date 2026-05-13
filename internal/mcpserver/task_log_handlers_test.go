package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/tasklog"
)

// newTaskLogTestServer wires the minimal Server surface the
// task_log_* handlers consume — an in-memory TaskLogs store.
func newTaskLogTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			TaskLogs: tasklog.NewMemoryStore(),
		},
		NowFunc: func() time.Time { return now },
	})
}

func TestHandleTaskLogAppend_HappyPath(t *testing.T) {
	t.Parallel()
	s := newTaskLogTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})

	res, err := s.handleTaskLogAppend(ctx, newCallToolReq("task_log_append", map[string]any{
		"task_id":      "task_x",
		"workspace_id": "wksp_x",
		"seq":          float64(0),
		"kind":         "start",
		"payload":      `{"worker_pid":42}`,
	}))
	if err != nil {
		t.Fatalf("handleTaskLogAppend err: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleTaskLogAppend isError: %+v", res.Content)
	}
	body := res.Content[0].(mcpgo.TextContent).Text
	var out struct {
		ID   string `json:"id"`
		Seq  int64  `json:"seq"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if out.ID == "" {
		t.Fatalf("empty id in resp: %s", body)
	}
	if out.Kind != "start" {
		t.Errorf("Kind = %q, want start", out.Kind)
	}
}

func TestHandleTaskLogAppend_MissingTaskID(t *testing.T) {
	t.Parallel()
	s := newTaskLogTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	res, err := s.handleTaskLogAppend(ctx, newCallToolReq("task_log_append", map[string]any{
		"workspace_id": "wksp_x",
		"seq":          float64(0),
		"kind":         "start",
	}))
	if err != nil {
		t.Fatalf("handleTaskLogAppend err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError envelope, got: %+v", res.Content)
	}
}

func TestHandleTaskLogList_ReturnsOrderedRows(t *testing.T) {
	t.Parallel()
	s := newTaskLogTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})

	for i := 0; i < 3; i++ {
		if _, err := s.handleTaskLogAppend(ctx, newCallToolReq("task_log_append", map[string]any{
			"task_id":      "task_y",
			"workspace_id": "wksp_y",
			"seq":          float64(i),
			"kind":         "stdout",
		})); err != nil {
			t.Fatalf("seed seq=%d: %v", i, err)
		}
	}

	res, err := s.handleTaskLogList(ctx, newCallToolReq("task_log_list", map[string]any{
		"task_id": "task_y",
	}))
	if err != nil {
		t.Fatalf("handleTaskLogList err: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleTaskLogList isError: %+v", res.Content)
	}
	body := res.Content[0].(mcpgo.TextContent).Text
	var out struct {
		Entries []struct {
			Seq int64 `json:"seq"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if len(out.Entries) != 3 {
		t.Fatalf("got %d entries, want 3 (body=%s)", len(out.Entries), body)
	}
	for i, e := range out.Entries {
		if e.Seq != int64(i) {
			t.Errorf("entries[%d].Seq = %d, want %d", i, e.Seq, i)
		}
	}
}

func TestHandleTaskLogList_MissingTaskID(t *testing.T) {
	t.Parallel()
	s := newTaskLogTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	res, err := s.handleTaskLogList(ctx, newCallToolReq("task_log_list", map[string]any{}))
	if err != nil {
		t.Fatalf("handleTaskLogList err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError envelope, got: %+v", res.Content)
	}
}

package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/tasklog"
)

func newTaskLogFixture() *Client {
	return New(Deps{TaskLogs: tasklog.NewMemoryStore()})
}

func TestTaskLogAppend_RequiresStore(t *testing.T) {
	c := New(Deps{})
	_, err := c.TaskLogAppend(context.Background(), Caller{}, TaskLogAppendInput{
		TaskID:      "task_x",
		WorkspaceID: "wksp_x",
		Kind:        tasklog.KindStart,
	})
	if !errors.Is(err, ErrTaskLogStoreNotConfigured) {
		t.Fatalf("err = %v, want ErrTaskLogStoreNotConfigured", err)
	}
}

func TestTaskLogAppend_RequiresTaskID(t *testing.T) {
	c := newTaskLogFixture()
	_, err := c.TaskLogAppend(context.Background(), Caller{}, TaskLogAppendInput{
		WorkspaceID: "wksp_x",
		Kind:        tasklog.KindStart,
	})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestTaskLogAppend_RequiresWorkspaceID(t *testing.T) {
	c := newTaskLogFixture()
	_, err := c.TaskLogAppend(context.Background(), Caller{}, TaskLogAppendInput{
		TaskID: "task_x",
		Kind:   tasklog.KindStart,
	})
	if err == nil {
		t.Fatalf("expected workspace_id required, got nil")
	}
}

func TestTaskLogAppend_HappyPath(t *testing.T) {
	c := newTaskLogFixture()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	out, err := c.TaskLogAppend(context.Background(), Caller{}, TaskLogAppendInput{
		TaskID:      "task_x",
		WorkspaceID: "wksp_x",
		Seq:         0,
		Kind:        tasklog.KindStart,
		Payload:     json.RawMessage(`{"worker_pid":4242}`),
		Now:         now,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if out.ID == "" {
		t.Fatalf("Append returned empty id")
	}
	if out.Kind != tasklog.KindStart {
		t.Fatalf("Kind = %q, want %q", out.Kind, tasklog.KindStart)
	}
	if !out.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", out.CreatedAt, now)
	}
}

func TestTaskLogAppend_StorePropagatesError(t *testing.T) {
	c := newTaskLogFixture()
	_, err := c.TaskLogAppend(context.Background(), Caller{}, TaskLogAppendInput{
		TaskID:      "task_x",
		WorkspaceID: "wksp_x",
		Kind:        "made-up-kind",
	})
	if err == nil {
		t.Fatalf("expected error from invalid kind, got nil")
	}
}

func TestTaskLogList_RequiresStore(t *testing.T) {
	c := New(Deps{})
	_, err := c.TaskLogList(context.Background(), Caller{}, TaskLogListInput{TaskID: "task_x"})
	if !errors.Is(err, ErrTaskLogStoreNotConfigured) {
		t.Fatalf("err = %v, want ErrTaskLogStoreNotConfigured", err)
	}
}

func TestTaskLogList_RequiresTaskID(t *testing.T) {
	c := newTaskLogFixture()
	_, err := c.TaskLogList(context.Background(), Caller{}, TaskLogListInput{})
	if err == nil {
		t.Fatalf("expected task_id required, got nil")
	}
}

func TestTaskLogList_ReturnsRowsInSeqOrder(t *testing.T) {
	c := newTaskLogFixture()
	now := time.Now().UTC()
	for i := int64(0); i < 4; i++ {
		_, _ = c.TaskLogAppend(context.Background(), Caller{}, TaskLogAppendInput{
			TaskID:      "task_x",
			WorkspaceID: "wksp_x",
			Seq:         i,
			Kind:        tasklog.KindStdout,
			Now:         now,
		})
	}
	out, err := c.TaskLogList(context.Background(), Caller{}, TaskLogListInput{TaskID: "task_x"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out.Entries) != 4 {
		t.Fatalf("len(entries) = %d, want 4", len(out.Entries))
	}
	for i, e := range out.Entries {
		if e.Seq != int64(i) {
			t.Errorf("entries[%d].Seq = %d, want %d", i, e.Seq, i)
		}
	}
}

func TestTaskLogSubscribe_RequiresStore(t *testing.T) {
	c := New(Deps{})
	_, _, err := c.TaskLogSubscribe(context.Background(), Caller{}, TaskLogSubscribeInput{TaskID: "task_x"})
	if !errors.Is(err, ErrTaskLogStoreNotConfigured) {
		t.Fatalf("err = %v, want ErrTaskLogStoreNotConfigured", err)
	}
}

func TestTaskLogSubscribe_RequiresTaskID(t *testing.T) {
	c := newTaskLogFixture()
	_, _, err := c.TaskLogSubscribe(context.Background(), Caller{}, TaskLogSubscribeInput{})
	if err == nil {
		t.Fatalf("expected task_id required, got nil")
	}
}

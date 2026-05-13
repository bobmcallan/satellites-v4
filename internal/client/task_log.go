package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/tasklog"
)

// ErrTaskLogStoreNotConfigured is returned when the typed task_log
// surface is called against a Client whose Deps.TaskLogs is nil.
// Wire-layer callers map this to a per-verb "unavailable" envelope.
var ErrTaskLogStoreNotConfigured = errors.New("task_log store not configured")

// TaskLogAppendInput is the typed input for task_log_append. seq is
// producer-supplied; ts may be left zero and the typed method fills it
// with the caller-supplied Now.
type TaskLogAppendInput struct {
	TaskID      string
	WorkspaceID string
	ProjectID   string
	Seq         int64
	TS          time.Time
	Kind        string
	Payload     json.RawMessage
	Memberships []string
	Now         time.Time
}

// TaskLogAppendOutput names the inserted row.
type TaskLogAppendOutput struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskLogAppend writes one telemetry row. Validates input, scopes
// workspace via memberships, delegates to the configured store.
func (c *Client) TaskLogAppend(ctx context.Context, caller Caller, in TaskLogAppendInput) (TaskLogAppendOutput, error) {
	if c.deps.TaskLogs == nil {
		return TaskLogAppendOutput{}, ErrTaskLogStoreNotConfigured
	}
	taskID := strings.TrimSpace(in.TaskID)
	if taskID == "" {
		return TaskLogAppendOutput{}, errors.New("task_log_append: task_id required")
	}
	if strings.TrimSpace(in.Kind) == "" {
		return TaskLogAppendOutput{}, errors.New("task_log_append: kind required")
	}
	workspaceID := strings.TrimSpace(in.WorkspaceID)
	// Resolve workspace_id from the task row when the producer didn't
	// supply one. Keeps the CLI call site terse and matches how the
	// task_log rows must land on the same workspace as the task.
	if workspaceID == "" && c.deps.Tasks != nil {
		if t, terr := c.deps.Tasks.GetByID(ctx, taskID, in.Memberships); terr == nil {
			workspaceID = t.WorkspaceID
		}
	}
	if workspaceID == "" {
		return TaskLogAppendOutput{}, errors.New("task_log_append: workspace_id required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ts := in.TS
	if ts.IsZero() {
		ts = now
	}
	e := tasklog.Entry{
		TaskID:      taskID,
		WorkspaceID: workspaceID,
		ProjectID:   strings.TrimSpace(in.ProjectID),
		Seq:         in.Seq,
		TS:          ts,
		Kind:        strings.TrimSpace(in.Kind),
		Payload:     []byte(in.Payload),
	}
	row, err := c.deps.TaskLogs.Append(ctx, e, now)
	if err != nil {
		return TaskLogAppendOutput{}, err
	}
	return TaskLogAppendOutput{
		ID:        row.ID,
		TaskID:    row.TaskID,
		Seq:       row.Seq,
		Kind:      row.Kind,
		CreatedAt: row.CreatedAt,
	}, nil
}

// TaskLogListInput selects rows for task_log_list. FromSeq is inclusive.
type TaskLogListInput struct {
	TaskID      string
	FromSeq     int64
	Limit       int
	Memberships []string
}

// TaskLogListEntry is the wire-shape projection of a tasklog.Entry.
// Payload is left as a JSON raw message so renderers can re-parse.
type TaskLogListEntry struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	WorkspaceID string          `json:"workspace_id"`
	ProjectID   string          `json:"project_id,omitempty"`
	Seq         int64           `json:"seq"`
	TS          time.Time       `json:"ts"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// TaskLogListOutput wraps the row slice so the wire payload is a single
// object (matches the task_walk shape).
type TaskLogListOutput struct {
	Entries []TaskLogListEntry `json:"entries"`
}

// TaskLogList returns rows for in.TaskID ordered by seq ASC.
func (c *Client) TaskLogList(ctx context.Context, caller Caller, in TaskLogListInput) (TaskLogListOutput, error) {
	if c.deps.TaskLogs == nil {
		return TaskLogListOutput{}, ErrTaskLogStoreNotConfigured
	}
	taskID := strings.TrimSpace(in.TaskID)
	if taskID == "" {
		return TaskLogListOutput{}, errors.New("task_log_list: task_id required")
	}
	rows, err := c.deps.TaskLogs.List(ctx, tasklog.ListOptions{
		TaskID:      taskID,
		FromSeq:     in.FromSeq,
		Limit:       in.Limit,
		Memberships: in.Memberships,
	})
	if err != nil {
		return TaskLogListOutput{}, err
	}
	out := TaskLogListOutput{Entries: make([]TaskLogListEntry, 0, len(rows))}
	for _, r := range rows {
		out.Entries = append(out.Entries, TaskLogListEntry{
			ID:          r.ID,
			TaskID:      r.TaskID,
			WorkspaceID: r.WorkspaceID,
			ProjectID:   r.ProjectID,
			Seq:         r.Seq,
			TS:          r.TS,
			Kind:        r.Kind,
			Payload:     json.RawMessage(r.Payload),
			CreatedAt:   r.CreatedAt,
		}, )
	}
	return out, nil
}

// TaskLogSubscribeInput names the subscription target. Memberships
// scope the underlying store call so cross-workspace subscriptions
// fail closed.
type TaskLogSubscribeInput struct {
	TaskID      string
	Memberships []string
}

// TaskLogSubscribe is the long-lived-connection surface the SSE
// handler consumes. Returns a channel of typed `TaskLogListEntry` rows
// and a cancel func the caller MUST defer. The carve-out from
// pr_mcp_cli_shared_path (one HTTP-only handler) is sanctioned by
// sty_8c17b89d's prose; layering stays clean because the SSE handler
// imports only this typed surface, never internal/tasklog directly.
//
// The returned channel buffers 256 frames. The underlying store-level
// channel runs through a pump goroutine so callers receive the same
// json-friendly shape as TaskLogList.
func (c *Client) TaskLogSubscribe(ctx context.Context, caller Caller, in TaskLogSubscribeInput) (<-chan TaskLogListEntry, func(), error) {
	if c.deps.TaskLogs == nil {
		return nil, nil, ErrTaskLogStoreNotConfigured
	}
	taskID := strings.TrimSpace(in.TaskID)
	if taskID == "" {
		return nil, nil, errors.New("task_log_subscribe: task_id required")
	}
	raw, rawCancel, err := c.deps.TaskLogs.Subscribe(ctx, taskID, in.Memberships)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan TaskLogListEntry, 256)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case e, ok := <-raw:
				if !ok {
					return
				}
				select {
				case out <- toListEntry(e):
				case <-done:
					return
				default:
					// Consumer is slow; drop frame rather than block
					// the producer (matches the underlying store's
					// fan-out shape — reconnect via Last-Event-ID
					// recovers any dropped frames).
				}
			}
		}
	}()
	cancel := func() {
		select {
		case <-done:
		default:
			close(done)
		}
		rawCancel()
	}
	return out, cancel, nil
}

// toListEntry projects a tasklog.Entry into the typed wire shape.
func toListEntry(e tasklog.Entry) TaskLogListEntry {
	return TaskLogListEntry{
		ID:          e.ID,
		TaskID:      e.TaskID,
		WorkspaceID: e.WorkspaceID,
		ProjectID:   e.ProjectID,
		Seq:         e.Seq,
		TS:          e.TS,
		Kind:        e.Kind,
		Payload:     json.RawMessage(e.Payload),
		CreatedAt:   e.CreatedAt,
	}
}

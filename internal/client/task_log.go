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

// Re-exported task_log kind constants. The wire layer (httpserver,
// mcpserver) and CLI consumer call PublishTaskLog with these strings;
// re-exporting here keeps the wire layer free of internal/tasklog
// imports (pr_mcp_cli_shared_path).
const (
	TaskLogKindClaim         = tasklog.KindClaim
	TaskLogKindToolCallStart = tasklog.KindToolCallStart
	TaskLogKindToolCallEnd   = tasklog.KindToolCallEnd
	TaskLogKindLedgerAppend  = tasklog.KindLedgerAppend
	TaskLogKindStatusChange  = tasklog.KindStatusChange
	TaskLogKindClose         = tasklog.KindClose
)

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
		})
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

// publishTaskLog is the shared emitter the server-side semantic-event
// kinds (sty_090f6183: claim / tool_call_* / ledger_append /
// status_change / close) call into. It allocates a per-task seq via
// Store.NextSeq, marshals the supplied payload, and appends the row.
// Errors are swallowed after a warn-log so a telemetry hiccup never
// fails the underlying typed-method call.
//
// resolveProjectID, when non-empty, is stamped on the row directly;
// when empty + the task store is configured the helper resolves the
// project from the task row. Same shape for resolveWorkspaceID.
func (c *Client) publishTaskLog(ctx context.Context, taskID, kind string, payload any) {
	if c == nil || c.deps.TaskLogs == nil || taskID == "" || kind == "" {
		return
	}
	workspaceID, projectID := c.resolveTaskWorkspaceProject(ctx, taskID)
	if workspaceID == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		if c.deps.Logger != nil {
			c.deps.Logger.Warn().Str("task_id", taskID).Str("kind", kind).Str("error", err.Error()).Msg("task_log publish: marshal failed")
		}
		return
	}
	seq, err := c.deps.TaskLogs.NextSeq(ctx, taskID)
	if err != nil {
		if c.deps.Logger != nil {
			c.deps.Logger.Warn().Str("task_id", taskID).Str("kind", kind).Str("error", err.Error()).Msg("task_log publish: next_seq failed")
		}
		return
	}
	now := time.Now().UTC()
	entry := tasklog.Entry{
		TaskID:      taskID,
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Seq:         seq,
		TS:          now,
		Kind:        kind,
		Payload:     body,
	}
	if _, err := c.deps.TaskLogs.Append(ctx, entry, now); err != nil {
		if c.deps.Logger != nil {
			c.deps.Logger.Warn().Str("task_id", taskID).Str("kind", kind).Int64("seq", seq).Str("error", err.Error()).Msg("task_log publish: append failed")
		}
	}
}

// resolveTaskWorkspaceProject returns (workspace_id, project_id) for
// the given task_id by looking up the task row server-internally
// (memberships=nil bypasses workspace scoping; safe because the helper
// is only called from inside an already-authorised typed method).
// Returns ("", "") when the task store is missing or lookup fails so
// the caller short-circuits emission.
func (c *Client) resolveTaskWorkspaceProject(ctx context.Context, taskID string) (string, string) {
	if c.deps.Tasks == nil {
		return "", ""
	}
	t, err := c.deps.Tasks.GetByID(ctx, taskID, nil)
	if err != nil {
		return "", ""
	}
	return t.WorkspaceID, t.ProjectID
}

// PublishTaskLog is the exported shim the httpserver tool-call
// telemetry middleware uses to emit KindToolCallStart / KindToolCallEnd
// for dispatched-agent requests. It delegates to the shared
// publishTaskLog path so the wire layer never names internal/tasklog
// (pr_mcp_cli_shared_path).
func (c *Client) PublishTaskLog(ctx context.Context, taskID, kind string, payload any) {
	c.publishTaskLog(ctx, taskID, kind, payload)
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

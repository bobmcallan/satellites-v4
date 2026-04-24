package task

import (
	"context"

	"github.com/bobmcallan/satellites/internal/hubemit"
)

const (
	eventKindPrefix = "task."
	topicPrefix     = "ws:"
)

// emitStatus publishes a task.<status> event describing the post-mutation
// row. Advisory: a panic in a subscriber is recovered so the caller's
// write path is never aborted.
func emitStatus(ctx context.Context, p hubemit.Publisher, t Task) {
	if p == nil || t.WorkspaceID == "" || t.Status == "" {
		return
	}
	defer func() { _ = recover() }()
	payload := map[string]any{
		"workspace_id": t.WorkspaceID,
		"project_id":   t.ProjectID,
		"task_id":      t.ID,
		"origin":       t.Origin,
		"priority":     t.Priority,
	}
	if t.Outcome != "" {
		payload["outcome"] = t.Outcome
	}
	p.Publish(ctx, topicPrefix+t.WorkspaceID, eventKindPrefix+t.Status, t.WorkspaceID, payload)
}

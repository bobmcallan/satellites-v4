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
	// sty_a03449d1: story_id + kind + action let the portal route a
	// frame to the correct story panel + render a skeleton row when
	// the rejection-append loop spawns a fresh task without a page
	// reload. Empty fields are omitted so the payload stays tight.
	if t.StoryID != "" {
		payload["story_id"] = t.StoryID
	}
	if t.Kind != "" {
		payload["kind"] = t.Kind
	}
	if t.Action != "" {
		payload["action"] = t.Action
	}
	if t.Outcome != "" {
		payload["outcome"] = t.Outcome
	}
	p.Publish(ctx, topicPrefix+t.WorkspaceID, eventKindPrefix+t.Status, t.WorkspaceID, payload)
}

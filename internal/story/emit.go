package story

import (
	"context"

	"github.com/bobmcallan/satellites/internal/hubemit"
)

const (
	eventKindPrefix = "story."
	topicPrefix     = "ws:"
)

// emitStatus publishes a story.<status> event for the post-mutation row.
// Advisory: subscriber panics are recovered.
func emitStatus(ctx context.Context, p hubemit.Publisher, s Story) {
	if p == nil || s.WorkspaceID == "" || s.Status == "" {
		return
	}
	defer func() { _ = recover() }()
	payload := map[string]any{
		"workspace_id": s.WorkspaceID,
		"project_id":   s.ProjectID,
		"story_id":     s.ID,
		"title":        s.Title,
	}
	p.Publish(ctx, topicPrefix+s.WorkspaceID, eventKindPrefix+s.Status, s.WorkspaceID, payload)
}

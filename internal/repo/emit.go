package repo

import (
	"context"

	"github.com/bobmcallan/satellites/internal/hubemit"
)

const (
	reindexEventPrefix = "repo.reindex."
	reindexTopicPrefix = "ws:"
)

// emitReindex publishes a repo.reindex.<phase> event for the post-action
// row. Mirrors internal/story/emit.go: nil publisher / empty workspace
// silently no-op, subscriber panics are recovered.
func emitReindex(ctx context.Context, p hubemit.Publisher, phase string, r Repo, extra map[string]any) {
	if p == nil || r.WorkspaceID == "" || phase == "" {
		return
	}
	defer func() { _ = recover() }()
	payload := map[string]any{
		"workspace_id": r.WorkspaceID,
		"project_id":   r.ProjectID,
		"repo_id":      r.ID,
		"git_remote":   r.GitRemote,
		"phase":        phase,
	}
	for k, v := range extra {
		payload[k] = v
	}
	p.Publish(ctx, reindexTopicPrefix+r.WorkspaceID, reindexEventPrefix+phase, r.WorkspaceID, payload)
}

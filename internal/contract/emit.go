package contract

import (
	"context"

	"github.com/bobmcallan/satellites/internal/hubemit"
)

const (
	eventKindPrefix = "contract_instance."
	topicPrefix     = "ws:"
)

// emitStatus publishes a contract_instance.<status> event for the
// post-mutation row. Advisory: subscriber panics are recovered.
func emitStatus(ctx context.Context, p hubemit.Publisher, ci ContractInstance) {
	if p == nil || ci.WorkspaceID == "" || ci.Status == "" {
		return
	}
	defer func() { _ = recover() }()
	payload := map[string]any{
		"workspace_id":  ci.WorkspaceID,
		"story_id":      ci.StoryID,
		"ci_id":         ci.ID,
		"contract_name": ci.ContractName,
		"sequence":      ci.Sequence,
	}
	p.Publish(ctx, topicPrefix+ci.WorkspaceID, eventKindPrefix+ci.Status, ci.WorkspaceID, payload)
}

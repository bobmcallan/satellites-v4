package ledger

import (
	"context"

	"github.com/bobmcallan/satellites/internal/hubemit"
)

// EventKind values published to the websocket hub on ledger mutations.
// Slice 10.3 (story_7ed84379).
const (
	EventKindAppended     = "ledger.append"
	EventKindDereferenced = "ledger.dereference"
	topicPrefix           = "ws:"
)

// emitAppended publishes a ledger.append event for the supplied entry.
// The call is wrapped in a recover so a panicking subscriber cannot
// abort the caller's mutation — emits are advisory.
func emitAppended(ctx context.Context, p hubemit.Publisher, entry LedgerEntry) {
	if p == nil || entry.WorkspaceID == "" {
		return
	}
	defer func() { _ = recover() }()
	payload := map[string]any{
		"workspace_id": entry.WorkspaceID,
		"project_id":   entry.ProjectID,
		"ledger_id":    entry.ID,
		"type":         entry.Type,
		"tags":         entry.Tags,
	}
	if entry.StoryID != nil {
		payload["story_id"] = *entry.StoryID
	}
	if entry.ContractID != nil {
		payload["contract_id"] = *entry.ContractID
	}
	p.Publish(ctx, topicPrefix+entry.WorkspaceID, EventKindAppended, entry.WorkspaceID, payload)
}

// emitDereferenced publishes a ledger.dereference event for the target
// row id that has just been flipped to status=dereferenced.
func emitDereferenced(ctx context.Context, p hubemit.Publisher, workspaceID, ledgerID, reason string) {
	if p == nil || workspaceID == "" {
		return
	}
	defer func() { _ = recover() }()
	payload := map[string]any{
		"workspace_id": workspaceID,
		"ledger_id":    ledgerID,
		"reason":       reason,
	}
	p.Publish(ctx, topicPrefix+workspaceID, EventKindDereferenced, workspaceID, payload)
}

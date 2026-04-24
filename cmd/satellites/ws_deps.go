// Deps wiring for the websocket surface: session → user resolution for
// wshandler, and a ledger-backed mismatch audit for hub.AuthHub.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/hub"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// sessionResolverAdapter fans the wshandler.SessionResolver call out to
// the existing session + user stores.
type sessionResolverAdapter struct {
	sessions auth.SessionStore
	users    auth.UserStoreByID
}

// Resolve implements wshandler.SessionResolver.
func (a *sessionResolverAdapter) Resolve(_ context.Context, sessionID string) (auth.User, error) {
	sess, err := a.sessions.Get(sessionID)
	if err != nil {
		return auth.User{}, fmt.Errorf("session lookup: %w", err)
	}
	return a.users.GetByID(sess.UserID)
}

// ledgerMismatchAudit persists hub publish-time guard violations to the
// ledger so cross-workspace publish attempts are auditable. Writes are
// best-effort — a failed write logs but must not block the hub.
type ledgerMismatchAudit struct {
	ledger    ledger.Store
	projectID string
	logger    arbor.ILogger
}

// HubMismatch implements hub.MismatchAudit.
func (l *ledgerMismatchAudit) HubMismatch(ctx context.Context, event hub.Event, expectedWorkspaceID string) {
	if l.ledger == nil {
		return
	}
	payload := map[string]any{
		"event_id":       event.ID,
		"event_kind":     event.Kind,
		"event_topic":    event.Topic,
		"event_ws_id":    event.WorkspaceID,
		"expected_ws_id": expectedWorkspaceID,
	}
	structured, _ := json.Marshal(payload)
	entry := ledger.LedgerEntry{
		WorkspaceID: expectedWorkspaceID,
		ProjectID:   l.projectID,
		Type:        ledger.TypeEvidence,
		Tags:        []string{"kind:hub-mismatch", "source:authhub", "severity:warning"},
		Content: fmt.Sprintf("hub publish dropped — event.workspace_id=%q does not match topic suffix %q (kind=%q)",
			event.WorkspaceID, expectedWorkspaceID, event.Kind),
		Structured: structured,
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceSystem,
		Status:     ledger.StatusActive,
		CreatedBy:  "system:hub",
	}
	if _, err := l.ledger.Append(ctx, entry, time.Now().UTC()); err != nil {
		l.logger.Warn().Str("error", err.Error()).Msg("hub mismatch audit write failed")
	}
}

// Deps wiring for the websocket surface: session → user resolution for
// wshandler, a ledger-backed mismatch audit for hub.AuthHub, and the
// hubPublisher that stores call on every mutation (slice 10.3).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/hub"
	"github.com/bobmcallan/satellites/internal/hubemit"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// publisherAttacher is satisfied by every store that owns an emit hook
// (ledger, task, contract, story). cmd/satellites calls SetPublisher on
// each store at boot.
type publisherAttacher interface {
	SetPublisher(p hubemit.Publisher)
}

// attachPublisher installs p on store when store implements the setter.
// Unknown store types (plain test doubles, nil) are silently skipped.
func attachPublisher(store any, p hubemit.Publisher) {
	if store == nil {
		return
	}
	if a, ok := store.(publisherAttacher); ok {
		a.SetPublisher(p)
	}
}

// hubPublisher adapts *hub.AuthHub to the hubemit.Publisher contract the
// stores import. Running store-emitted events through AuthHub keeps the
// publish-time topic-suffix guard in the path for defence in depth.
type hubPublisher struct {
	authHub *hub.AuthHub
}

// Publish implements hubemit.Publisher.
func (h *hubPublisher) Publish(ctx context.Context, topic, kind, workspaceID string, data any) {
	if h == nil || h.authHub == nil {
		return
	}
	h.authHub.Publish(ctx, topic, hub.Event{
		Kind:        kind,
		WorkspaceID: workspaceID,
		Data:        data,
	})
}

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

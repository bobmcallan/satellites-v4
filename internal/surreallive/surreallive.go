// Package surreallive is the satellites server's typed subscriber over
// SurrealDB's LIVE query notification stream. Order:02 of
// epic:surreal-live-migration (sty_c7c3850f) — the seam every later
// phase consumes.
//
// The package wraps the surrealdb.go SDK's per-table Live + LiveNotifications
// pair behind a small DB interface so tests can stub the wire surface
// without standing up a real cluster. Subscribers open LIVE on a single
// table; workspace scoping is applied client-side as belt-and-braces
// per docs/surreal-live-feasibility.md (the privileged-credential
// connection model means DB-level RBAC does not auto-isolate workspaces;
// the WHERE clause is the security surface).
//
// Reconnect: each Subscribe call owns a dial loop. On stream close /
// underlying disconnect, the loop sleeps with exponential backoff
// bounded by [Config.MinBackoff, Config.MaxBackoff] and retries the
// LIVE registration. A nil ctx.Err() check at the top of every cycle
// makes graceful shutdown tight.
//
// Replay: this package does NOT cover the disconnect window — the SDK
// emits no replay, and the catch-up SELECT pattern from the design doc
// is deferred to a follow-up. Wake-event consumers (the agent's claim
// path) backstop on polling Claim; portal consumers refresh on tab
// focus. Order:03's parity verifier will surface any meaningful drift
// the missing replay introduces.
package surreallive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ternarybob/arbor"
)

// DefaultMinBackoff matches the WS subscriber's MinBackoff baseline.
const DefaultMinBackoff = 500 * time.Millisecond

// DefaultMaxBackoff matches the WS subscriber's MaxBackoff baseline.
const DefaultMaxBackoff = 30 * time.Second

// Config parameterises a Subscriber. Zero-valued durations select
// documented defaults.
type Config struct {
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// Action mirrors the SDK's notification action set; using a small
// internal alias keeps the public surface free of surrealdb-go types so
// downstream consumers don't import the SDK transitively.
type Action string

const (
	ActionCreate Action = "CREATE"
	ActionUpdate Action = "UPDATE"
	ActionDelete Action = "DELETE"
)

// Event is the unit a subscriber forwards to its onEvent callback.
// Kind is the table-prefixed action ("tasks.create", "tasks.update",
// etc.) so the wire format stays parallel with the existing
// hub.Event.Kind shape.
type Event struct {
	Kind        string
	Table       string
	Action      Action
	WorkspaceID string
	ProjectID   string
	Row         map[string]any
	OccurredAt  time.Time
}

// Notification is the wire-level shape the DB interface yields. Mirrors
// the SDK's connection.Notification but with map[string]any results so
// tests can construct fixtures without depending on surrealdb-go types.
type Notification struct {
	ID     string
	Action Action
	Result map[string]any
}

// DB is the minimal surface Subscriber depends on. Production wires the
// adapter at NewSubscriber; tests inject a stub.
type DB interface {
	// Live opens a LIVE query against table and returns the live query
	// id. diff=false yields full-row notifications; diff=true yields
	// JSON-patch ops. We use diff=false; ops support is a follow-up.
	Live(ctx context.Context, table string, diff bool) (string, error)
	// LiveNotifications returns the receive channel bound to liveID.
	// Closes when the underlying connection drops or
	// CloseLiveNotifications is called.
	LiveNotifications(liveID string) (<-chan Notification, error)
	// CloseLiveNotifications releases the channel for liveID. Safe to
	// call on an already-closed id.
	CloseLiveNotifications(liveID string) error
}

// Subscriber owns the dial loop for one or more LIVE registrations.
// Methods are concurrency-safe; multiple Subscribe calls run independent
// dial loops.
type Subscriber struct {
	db     DB
	cfg    Config
	logger arbor.ILogger

	mu      sync.Mutex
	closing bool
}

// New returns a Subscriber bound to db. logger may be nil. Zero-valued
// cfg fields fall to defaults.
func New(db DB, cfg Config, logger arbor.ILogger) *Subscriber {
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = cfg.MinBackoff
	}
	return &Subscriber{db: db, cfg: cfg, logger: logger}
}

// Subscribe opens a LIVE on table and forwards every notification whose
// row.workspace_id matches one of workspaceIDs to onEvent. An empty
// workspaceIDs slice forwards every notification (system-wide
// subscribers).
//
// The call blocks until ctx is cancelled. On clean ctx cancel the
// underlying live query is closed and the function returns ctx.Err().
// On non-graceful errors the dial loop reconnects with backoff; the
// function only returns when ctx is done.
//
// onEvent panics are recovered per-callback so a buggy consumer cannot
// abort the loop. Returns immediately if db is nil or table is empty.
func (s *Subscriber) Subscribe(ctx context.Context, table string, workspaceIDs []string, onEvent func(Event)) error {
	if s == nil {
		return errors.New("surreallive: nil subscriber")
	}
	if s.db == nil {
		return errors.New("surreallive: nil DB")
	}
	if table == "" {
		return errors.New("surreallive: empty table")
	}
	if onEvent == nil {
		return errors.New("surreallive: nil onEvent")
	}

	allowed := buildAllowedSet(workspaceIDs)
	backoff := s.cfg.MinBackoff
	for ctx.Err() == nil {
		drained, err := s.runOnce(ctx, table, allowed, onEvent)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if drained {
			// Clean drain — the channel closed without our cancellation.
			// Reset backoff so the next reconnect is fast.
			backoff = s.cfg.MinBackoff
		} else {
			if s.logger != nil {
				errStr := ""
				if err != nil {
					errStr = err.Error()
				}
				s.logger.Debug().
					Str("table", table).
					Str("error", errStr).
					Dur("backoff", backoff).
					Msg("surreallive: reconnect backoff")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > s.cfg.MaxBackoff {
			backoff = s.cfg.MaxBackoff
		}
	}
	return ctx.Err()
}

// runOnce opens one live registration, drains it until the channel
// closes or ctx cancels, and returns (drained, err). drained=true
// signals a clean server-side close (the channel returned via range
// loop with no error), drained=false signals a registration-level
// failure that warrants backoff.
func (s *Subscriber) runOnce(ctx context.Context, table string, allowed map[string]struct{}, onEvent func(Event)) (bool, error) {
	liveID, err := s.db.Live(ctx, table, false)
	if err != nil {
		return false, fmt.Errorf("Live(%s): %w", table, err)
	}
	ch, err := s.db.LiveNotifications(liveID)
	if err != nil {
		_ = s.db.CloseLiveNotifications(liveID)
		return false, fmt.Errorf("LiveNotifications(%s): %w", liveID, err)
	}
	defer func() { _ = s.db.CloseLiveNotifications(liveID) }()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case notif, ok := <-ch:
			if !ok {
				// Channel closed by the SDK or the server — clean drain.
				return true, nil
			}
			ev, fwdable := notificationToEvent(table, notif, allowed)
			if !fwdable {
				continue
			}
			s.dispatch(onEvent, ev)
		}
	}
}

// dispatch invokes onEvent under a recover guard so a single panicking
// consumer cannot abort the dial loop. Mirrors the pattern in
// internal/task/listener.go's fanoutListeners.
func (s *Subscriber) dispatch(onEvent func(Event), ev Event) {
	defer func() {
		if r := recover(); r != nil && s.logger != nil {
			s.logger.Warn().
				Str("table", ev.Table).
				Str("workspace_id", ev.WorkspaceID).
				Msgf("surreallive: onEvent panic recovered: %v", r)
		}
	}()
	onEvent(ev)
}

// notificationToEvent decodes a Notification into an Event suitable for
// downstream consumers. Returns fwdable=false when the notification
// targets a workspace the subscriber is not scoped to. The decode is
// best-effort: missing workspace_id / project_id leave those fields
// empty.
func notificationToEvent(table string, n Notification, allowed map[string]struct{}) (Event, bool) {
	row := n.Result
	if row == nil {
		row = map[string]any{}
	}
	wsID, _ := row["workspace_id"].(string)
	if len(allowed) > 0 {
		if _, ok := allowed[wsID]; !ok {
			return Event{}, false
		}
	}
	projID, _ := row["project_id"].(string)
	return Event{
		Kind:        table + "." + actionLowercase(n.Action),
		Table:       table,
		Action:      n.Action,
		WorkspaceID: wsID,
		ProjectID:   projID,
		Row:         row,
		OccurredAt:  time.Now().UTC(),
	}, true
}

// actionLowercase maps the SDK's uppercase action enum to the lowercase
// string the existing hub.Event.Kind convention uses.
func actionLowercase(a Action) string {
	switch a {
	case ActionCreate:
		return "create"
	case ActionUpdate:
		return "update"
	case ActionDelete:
		return "delete"
	}
	return string(a)
}

// buildAllowedSet returns a deduplicated set keyed on workspace id, or
// nil when ids is empty (signal: forward every notification).
func buildAllowedSet(ids []string) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		out[id] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealSessionStore is the SurrealDB-backed implementation of
// SessionStore. The cookie session id (UUID) is the row id; a single
// Sweep call removes expired rows on a cron tick. Created in
// story_0ab83f82 to replace MemorySessionStore on production deploys
// where Fly rolling restarts would otherwise drop in-flight sessions.
type SurrealSessionStore struct {
	db    *surrealdb.DB
	clock func() time.Time
}

// authSessionRow is the on-disk shape of an auth session.
// Field tags match the surrealdb.go json marshalling so SELECT * round-trips.
type authSessionRow struct {
	ID                string    `json:"id"`
	UserID            string    `json:"user_id"`
	ActiveWorkspaceID string    `json:"active_workspace_id"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// NewSurrealSessionStore wraps db with the auth_sessions table created on
// first call. Idempotent: re-running the DEFINE statements is a no-op.
func NewSurrealSessionStore(db *surrealdb.DB) *SurrealSessionStore {
	s := &SurrealSessionStore{db: db, clock: time.Now}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS auth_sessions SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS auth_sessions_expires_at ON TABLE auth_sessions FIELDS expires_at", nil)
	return s
}

const authSessionCols = "meta::id(id) AS id, user_id, active_workspace_id, expires_at"

// Create implements SessionStore.
func (s *SurrealSessionStore) Create(userID string, ttl time.Duration) (Session, error) {
	id := uuid.NewString()
	now := s.clock()
	row := authSessionRow{
		ID:        id,
		UserID:    userID,
		ExpiresAt: now.Add(ttl),
	}
	if err := s.write(context.Background(), row); err != nil {
		return Session{}, err
	}
	return Session{
		ID:        row.ID,
		UserID:    row.UserID,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// Get implements SessionStore. Lazily prunes rows past ExpiresAt to match
// the MemorySessionStore semantics.
func (s *SurrealSessionStore) Get(id string) (Session, error) {
	row, err := s.fetch(context.Background(), id)
	if err != nil {
		return Session{}, err
	}
	if s.clock().After(row.ExpiresAt) {
		_ = s.deleteRow(context.Background(), id)
		return Session{}, ErrSessionNotFound
	}
	return rowToSession(row), nil
}

// Delete implements SessionStore. Missing ids are silently ignored so
// logout stays idempotent (matching MemorySessionStore).
func (s *SurrealSessionStore) Delete(id string) error {
	return s.deleteRow(context.Background(), id)
}

// SetActiveWorkspace implements SessionStore.
func (s *SurrealSessionStore) SetActiveWorkspace(id, workspaceID string) error {
	row, err := s.fetch(context.Background(), id)
	if err != nil {
		return err
	}
	if s.clock().After(row.ExpiresAt) {
		_ = s.deleteRow(context.Background(), id)
		return ErrSessionNotFound
	}
	row.ActiveWorkspaceID = workspaceID
	return s.write(context.Background(), row)
}

// Sweep deletes every row whose expires_at is at or before `now`. Returns
// the number of rows removed. Wired to a cron task in main() so expired
// sessions don't accumulate between user logins.
func (s *SurrealSessionStore) Sweep(ctx context.Context, now time.Time) (int, error) {
	sql := "DELETE FROM auth_sessions WHERE expires_at <= $cutoff RETURN BEFORE"
	vars := map[string]any{"cutoff": now}
	results, err := surrealdb.Query[[]authSessionRow](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("auth: session sweep: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

func (s *SurrealSessionStore) fetch(ctx context.Context, id string) (authSessionRow, error) {
	sql := fmt.Sprintf("SELECT %s FROM auth_sessions WHERE id = $rid LIMIT 1", authSessionCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("auth_sessions", id)}
	results, err := surrealdb.Query[[]authSessionRow](ctx, s.db, sql, vars)
	if err != nil {
		return authSessionRow{}, fmt.Errorf("auth: session select: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return authSessionRow{}, ErrSessionNotFound
	}
	return (*results)[0].Result[0], nil
}

func (s *SurrealSessionStore) write(ctx context.Context, row authSessionRow) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("auth_sessions", row.ID),
		"doc": row,
	}
	if _, err := surrealdb.Query[[]authSessionRow](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("auth: session upsert: %w", err)
	}
	return nil
}

func (s *SurrealSessionStore) deleteRow(ctx context.Context, id string) error {
	sql := "DELETE $rid"
	vars := map[string]any{"rid": surrealmodels.NewRecordID("auth_sessions", id)}
	if _, err := surrealdb.Query[any](ctx, s.db, sql, vars); err != nil {
		// Surreal returns success on missing row deletes; only true errors
		// (connection drops, syntax) propagate.
		return fmt.Errorf("auth: session delete: %w", err)
	}
	return nil
}

func rowToSession(r authSessionRow) Session {
	return Session{
		ID:                r.ID,
		UserID:            r.UserID,
		ActiveWorkspaceID: r.ActiveWorkspaceID,
		ExpiresAt:         r.ExpiresAt,
	}
}

// Compile-time assertion that SurrealSessionStore implements SessionStore.
var _ SessionStore = (*SurrealSessionStore)(nil)

// ErrSessionStore is a sentinel kept for API symmetry with errors.Is
// callers; presently maps to ErrSessionNotFound.
var ErrSessionStore = errors.New("auth: session store error")

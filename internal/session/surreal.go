package session

import (
	"context"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is a SurrealDB-backed Store. The primary key is
// composite (user_id, session_id); the row id is derived by
// concatenation so the DEFINE TABLE upsert stays keyed.
type SurrealStore struct {
	db *surrealdb.DB
}

// NewSurrealStore wraps db as a Store.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS sessions SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS sessions_user_last_seen ON TABLE sessions FIELDS user_id, last_seen_at", nil)
	return s
}

const selectCols = "meta::id(id) AS id, user_id, session_id, source, registered_at, last_seen_at, orchestrator_grant_id"

func rowID(userID, sessionID string) string {
	// Record ids only tolerate a limited charset; join via "::" and let
	// Surreal accept it as a string id. Collisions impossible because
	// both fields are UUID-like.
	return userID + "::" + sessionID
}

// Register implements Store for SurrealStore.
func (s *SurrealStore) Register(ctx context.Context, userID, sessionID, source string, now time.Time) (Session, error) {
	existing, err := s.Get(ctx, userID, sessionID)
	if err == nil {
		existing.LastSeenAt = now
		if source != "" {
			existing.Source = source
		}
		if err := s.write(ctx, existing); err != nil {
			return Session{}, err
		}
		return existing, nil
	}
	sess := Session{
		UserID:     userID,
		SessionID:  sessionID,
		Source:     source,
		Registered: now,
		LastSeenAt: now,
	}
	if err := s.write(ctx, sess); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Get implements Store for SurrealStore.
func (s *SurrealStore) Get(ctx context.Context, userID, sessionID string) (Session, error) {
	sql := fmt.Sprintf("SELECT %s FROM sessions WHERE id = $rid LIMIT 1", selectCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("sessions", rowID(userID, sessionID))}
	results, err := surrealdb.Query[[]Session](ctx, s.db, sql, vars)
	if err != nil {
		return Session{}, fmt.Errorf("session: select: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Session{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// Touch implements Store for SurrealStore.
func (s *SurrealStore) Touch(ctx context.Context, userID, sessionID string, now time.Time) (Session, error) {
	sess, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return Session{}, err
	}
	sess.LastSeenAt = now
	if err := s.write(ctx, sess); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// SetOrchestratorGrant implements Store for SurrealStore.
func (s *SurrealStore) SetOrchestratorGrant(ctx context.Context, userID, sessionID, grantID string, now time.Time) (Session, error) {
	sess, err := s.Get(ctx, userID, sessionID)
	if err != nil {
		return Session{}, err
	}
	sess.OrchestratorGrantID = grantID
	sess.LastSeenAt = now
	if err := s.write(ctx, sess); err != nil {
		return Session{}, err
	}
	return sess, nil
}

func (s *SurrealStore) write(ctx context.Context, sess Session) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("sessions", rowID(sess.UserID, sess.SessionID)),
		"doc": sess,
	}
	if _, err := surrealdb.Query[[]Session](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("session: upsert: %w", err)
	}
	return nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

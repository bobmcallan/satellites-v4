package workspace

import (
	"context"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is a SurrealDB-backed Store. The caller must have already
// authenticated and selected ns/db on the supplied *surrealdb.DB (see
// internal/db.Connect).
type SurrealStore struct {
	db *surrealdb.DB
}

// NewSurrealStore wraps db as a Store. Defines the `workspaces` and
// `workspace_members` tables schemaless so first-time SELECTs don't error on
// a missing table, plus an index on workspace_members.user_id for the
// common ListByMember path.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	ctx := context.Background()
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS workspaces SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS workspace_members SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS workspace_members_user ON TABLE workspace_members COLUMNS user_id", nil)
	return s
}

// selectCols expands to a SELECT that preserves the string form of id.
const selectCols = "meta::id(id) AS id, name, owner_user_id, status, created_at, updated_at"

// memberSelectCols mirrors the workspace_members row shape.
const memberSelectCols = "workspace_id, user_id, role, added_at, added_by"

// memberKey builds the composite record id for a membership row so
// (workspace_id, user_id) stays uniquely addressable.
func memberKey(workspaceID, userID string) string {
	return fmt.Sprintf("%s::%s", workspaceID, userID)
}

// Create implements Store for SurrealStore. Writes the workspace row and the
// creator-as-admin membership row back-to-back.
func (s *SurrealStore) Create(ctx context.Context, ownerUserID, name string, now time.Time) (Workspace, error) {
	w := Workspace{
		ID:          NewID(),
		Name:        name,
		OwnerUserID: ownerUserID,
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.writeWorkspace(ctx, w); err != nil {
		return Workspace{}, err
	}
	member := Member{
		WorkspaceID: w.ID,
		UserID:      ownerUserID,
		Role:        RoleAdmin,
		AddedAt:     now,
		AddedBy:     ownerUserID,
	}
	if err := s.writeMember(ctx, member); err != nil {
		return Workspace{}, err
	}
	return w, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string) (Workspace, error) {
	sql := fmt.Sprintf("SELECT %s FROM workspaces WHERE id = $rid LIMIT 1", selectCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("workspaces", id)}
	results, err := surrealdb.Query[[]Workspace](ctx, s.db, sql, vars)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: select by id: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Workspace{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// ListByMember implements Store for SurrealStore. Newest-first.
//
// Runs two queries rather than a single nested subquery because Surreal's
// `SELECT VALUE workspace_id` returns plain strings while the outer table's
// `id` is a RecordID — the IN comparison silently misses across that type
// boundary. Fetching the id list first, then the workspace rows, side-steps
// the coercion problem and is cheap: each user's workspace count is bounded.
func (s *SurrealStore) ListByMember(ctx context.Context, userID string) ([]Workspace, error) {
	idSQL := "SELECT VALUE workspace_id FROM workspace_members WHERE user_id = $user"
	idResults, err := surrealdb.Query[[]string](ctx, s.db, idSQL, map[string]any{"user": userID})
	if err != nil {
		return nil, fmt.Errorf("workspace: list member ids: %w", err)
	}
	if idResults == nil || len(*idResults) == 0 {
		return []Workspace{}, nil
	}
	ids := (*idResults)[0].Result
	if len(ids) == 0 {
		return []Workspace{}, nil
	}
	rids := make([]surrealmodels.RecordID, 0, len(ids))
	for _, id := range ids {
		rids = append(rids, surrealmodels.NewRecordID("workspaces", id))
	}
	sql := fmt.Sprintf("SELECT %s FROM workspaces WHERE id IN $rids ORDER BY created_at DESC", selectCols)
	results, err := surrealdb.Query[[]Workspace](ctx, s.db, sql, map[string]any{"rids": rids})
	if err != nil {
		return nil, fmt.Errorf("workspace: list by member: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Workspace{}, nil
	}
	return (*results)[0].Result, nil
}

// IsMember implements Store for SurrealStore.
func (s *SurrealStore) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	sql := "SELECT workspace_id FROM workspace_members WHERE workspace_id = $ws AND user_id = $user LIMIT 1"
	vars := map[string]any{"ws": workspaceID, "user": userID}
	results, err := surrealdb.Query[[]map[string]any](ctx, s.db, sql, vars)
	if err != nil {
		return false, fmt.Errorf("workspace: is member: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return false, nil
	}
	return len((*results)[0].Result) > 0, nil
}

// GetRole implements Store for SurrealStore.
func (s *SurrealStore) GetRole(ctx context.Context, workspaceID, userID string) (string, error) {
	sql := fmt.Sprintf(
		"SELECT %s FROM workspace_members WHERE workspace_id = $ws AND user_id = $user LIMIT 1",
		memberSelectCols,
	)
	vars := map[string]any{"ws": workspaceID, "user": userID}
	results, err := surrealdb.Query[[]Member](ctx, s.db, sql, vars)
	if err != nil {
		return "", fmt.Errorf("workspace: get role: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return "", ErrNotFound
	}
	return (*results)[0].Result[0].Role, nil
}

func (s *SurrealStore) writeWorkspace(ctx context.Context, w Workspace) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("workspaces", w.ID),
		"doc": w,
	}
	if _, err := surrealdb.Query[[]Workspace](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("workspace: upsert: %w", err)
	}
	return nil
}

func (s *SurrealStore) writeMember(ctx context.Context, m Member) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("workspace_members", memberKey(m.WorkspaceID, m.UserID)),
		"doc": m,
	}
	if _, err := surrealdb.Query[[]Member](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("workspace: upsert member: %w", err)
	}
	return nil
}

// AddMember implements Store for SurrealStore.
func (s *SurrealStore) AddMember(ctx context.Context, workspaceID, userID, role, addedBy string, now time.Time) error {
	if !IsValidRole(role) {
		return ErrInvalidRole
	}
	if _, err := s.GetByID(ctx, workspaceID); err != nil {
		return err
	}
	member := Member{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Role:        role,
		AddedAt:     now,
		AddedBy:     addedBy,
	}
	return s.writeMember(ctx, member)
}

var _ Store = (*SurrealStore)(nil)

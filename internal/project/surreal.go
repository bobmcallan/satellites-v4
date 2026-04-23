package project

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

// NewSurrealStore wraps db as a Store. Defines the `projects` table
// schemaless so first-time SELECTs don't error on a missing table.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS projects SCHEMALESS", nil)
	return s
}

// Create implements Store for SurrealStore.
func (s *SurrealStore) Create(ctx context.Context, ownerUserID, workspaceID, name string, now time.Time) (Project, error) {
	p := Project{
		ID:          NewID(),
		WorkspaceID: workspaceID,
		Name:        name,
		OwnerUserID: ownerUserID,
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.write(ctx, p); err != nil {
		return Project{}, err
	}
	return p, nil
}

// selectCols expands to a SELECT that preserves the string form of id.
// SurrealDB otherwise returns id as a RecordID object, which JSON-unmarshals
// as empty into `ID string`. `meta::id(id) AS id` returns just the id portion
// (e.g. "proj_xxx") without the table prefix.
const selectCols = "meta::id(id) AS id, workspace_id, name, owner_user_id, status, created_at, updated_at"

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string) (Project, error) {
	sql := fmt.Sprintf("SELECT %s FROM projects WHERE id = $rid LIMIT 1", selectCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("projects", id)}
	results, err := surrealdb.Query[[]Project](ctx, s.db, sql, vars)
	if err != nil {
		return Project{}, fmt.Errorf("project: select by id: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Project{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// ListByOwner implements Store for SurrealStore. Newest-first.
func (s *SurrealStore) ListByOwner(ctx context.Context, ownerUserID string) ([]Project, error) {
	sql := fmt.Sprintf("SELECT %s FROM projects WHERE owner_user_id = $owner ORDER BY created_at DESC", selectCols)
	vars := map[string]any{"owner": ownerUserID}
	results, err := surrealdb.Query[[]Project](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("project: list by owner: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Project{}, nil
	}
	return (*results)[0].Result, nil
}

// UpdateName implements Store for SurrealStore.
func (s *SurrealStore) UpdateName(ctx context.Context, id, name string, now time.Time) (Project, error) {
	existing, err := s.GetByID(ctx, id)
	if err != nil {
		return Project{}, err
	}
	existing.Name = name
	existing.UpdatedAt = now
	if err := s.write(ctx, existing); err != nil {
		return Project{}, err
	}
	return existing, nil
}

func (s *SurrealStore) write(ctx context.Context, p Project) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("projects", p.ID),
		"doc": p,
	}
	if _, err := surrealdb.Query[[]Project](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("project: upsert: %w", err)
	}
	return nil
}

// SetWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) SetWorkspaceID(ctx context.Context, id, workspaceID string, now time.Time) (Project, error) {
	existing, err := s.GetByID(ctx, id)
	if err != nil {
		return Project{}, err
	}
	existing.WorkspaceID = workspaceID
	existing.UpdatedAt = now
	if err := s.write(ctx, existing); err != nil {
		return Project{}, err
	}
	return existing, nil
}

// ListMissingWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) ListMissingWorkspaceID(ctx context.Context) ([]Project, error) {
	sql := fmt.Sprintf("SELECT %s FROM projects WHERE workspace_id IS NONE OR workspace_id = '' ORDER BY created_at ASC", selectCols)
	results, err := surrealdb.Query[[]Project](ctx, s.db, sql, nil)
	if err != nil {
		return nil, fmt.Errorf("project: list missing workspace_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Project{}, nil
	}
	return (*results)[0].Result, nil
}

// Compile-time assertion that SurrealStore satisfies Store.
var _ Store = (*SurrealStore)(nil)

package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is a SurrealDB-backed Store. The caller must have already
// authenticated and selected ns/db on the supplied *surrealdb.DB.
type SurrealStore struct {
	db *surrealdb.DB
}

// NewSurrealStore wraps db as a Store. Defines the `ledger` table
// schemaless so first-time SELECTs don't error on a missing table.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS ledger SCHEMALESS", nil)
	return s
}

// selectCols preserves the string form of id (see internal/project/surreal.go).
const selectCols = "meta::id(id) AS id, workspace_id, project_id, type, content, actor, created_at"

// Append implements Store for SurrealStore.
func (s *SurrealStore) Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error) {
	entry.ID = NewID()
	entry.CreatedAt = now
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("ledger", entry.ID),
		"doc": entry,
	}
	if _, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars); err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: append: %w", err)
	}
	return entry, nil
}

// List implements Store for SurrealStore. Newest-first, limit clamped.
func (s *SurrealStore) List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error) {
	opts = opts.normalised()
	if memberships != nil && len(memberships) == 0 {
		return []LedgerEntry{}, nil
	}
	conds := []string{"project_id = $project"}
	vars := map[string]any{"project": projectID, "lim": opts.Limit}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	if opts.Type != "" {
		conds = append(conds, "type = $type")
		vars["type"] = opts.Type
	}
	where := conds[0]
	for i := 1; i < len(conds); i++ {
		where += " AND " + conds[i]
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at DESC LIMIT $lim", selectCols, where)
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []LedgerEntry{}, nil
	}
	return (*results)[0].Result, nil
}

// BackfillWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error) {
	sql := "UPDATE ledger SET workspace_id = $ws WHERE project_id = $project AND (workspace_id IS NONE OR workspace_id = '') RETURN AFTER"
	vars := map[string]any{"ws": workspaceID, "project": projectID}
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("ledger: backfill workspace_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// Compile-time assertion that SurrealStore satisfies Store.
var _ Store = (*SurrealStore)(nil)

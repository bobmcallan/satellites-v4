package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// schemaless so first-time SELECTs don't error on a missing table; also
// declares the §6 access indexes — idempotent under DEFINE INDEX IF NOT
// EXISTS.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	ctx := context.Background()
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS ledger SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS ledger_ws_story_created ON ledger FIELDS workspace_id, story_id, created_at", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS ledger_ws_contract ON ledger FIELDS workspace_id, contract_id", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS ledger_ws_tags ON ledger FIELDS workspace_id, tags", nil)
	return s
}

// selectCols preserves the string form of id (see internal/project/surreal.go).
const selectCols = "meta::id(id) AS id, workspace_id, project_id, story_id, contract_id, type, tags, content, structured, durability, expires_at, source_type, sensitive, status, created_at, created_by"

// Append implements Store for SurrealStore.
func (s *SurrealStore) Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error) {
	applyDefaults(&entry)
	if err := entry.Validate(); err != nil {
		return LedgerEntry{}, err
	}
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
// Default behaviour excludes dereferenced rows; callers opt in via
// ListOptions.Status='dereferenced' or ListOptions.IncludeDerefd=true.
func (s *SurrealStore) List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error) {
	opts = opts.normalised()
	if memberships != nil && len(memberships) == 0 {
		return []LedgerEntry{}, nil
	}
	conds, vars := s.buildListWhere(projectID, opts, memberships)
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at DESC LIMIT $lim", selectCols, strings.Join(conds, " AND "))
	vars["lim"] = opts.Limit
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []LedgerEntry{}, nil
	}
	return (*results)[0].Result, nil
}

// buildListWhere translates ListOptions into a SurrealDB WHERE clause +
// vars map. Workspace memberships and the dereferenced-default-exclude
// rules live here so List + Search share one source of truth.
func (s *SurrealStore) buildListWhere(projectID string, opts ListOptions, memberships []string) ([]string, map[string]any) {
	conds := []string{}
	vars := map[string]any{}
	if projectID != "" {
		conds = append(conds, "project_id = $project")
		vars["project"] = projectID
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	if opts.Type != "" {
		conds = append(conds, "type = $type")
		vars["type"] = opts.Type
	}
	if opts.StoryID != "" {
		conds = append(conds, "story_id = $story")
		vars["story"] = opts.StoryID
	}
	if opts.ContractID != "" {
		conds = append(conds, "contract_id = $contract")
		vars["contract"] = opts.ContractID
	}
	if len(opts.Tags) > 0 {
		conds = append(conds, "tags ANYINSIDE $tags")
		vars["tags"] = opts.Tags
	}
	if opts.Durability != "" {
		conds = append(conds, "durability = $durability")
		vars["durability"] = opts.Durability
	}
	if opts.SourceType != "" {
		conds = append(conds, "source_type = $source_type")
		vars["source_type"] = opts.SourceType
	}
	if opts.Sensitive != nil {
		conds = append(conds, "sensitive = $sensitive")
		vars["sensitive"] = *opts.Sensitive
	}
	if opts.Status != "" {
		conds = append(conds, "status = $status")
		vars["status"] = opts.Status
	} else if !opts.IncludeDerefd {
		conds = append(conds, "(status IS NONE OR status != 'dereferenced')")
	}
	return conds, vars
}

// Search implements Store for SurrealStore.
func (s *SurrealStore) Search(ctx context.Context, projectID string, opts SearchOptions, memberships []string) ([]LedgerEntry, error) {
	if memberships != nil && len(memberships) == 0 {
		return nil, nil
	}
	listOpts := opts.normalised()
	conds, vars := s.buildListWhere(projectID, listOpts, memberships)
	q := strings.ToLower(strings.TrimSpace(opts.Query))
	if q != "" {
		conds = append(conds, "string::lowercase(content) CONTAINS $q")
		vars["q"] = q
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 20
	}
	if topK > 100 {
		topK = 100
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at DESC LIMIT %d", selectCols, strings.Join(conds, " AND "), topK)
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: search: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	return (*results)[0].Result, nil
}

// Recall implements Store for SurrealStore. Returns the chain of rows
// tagged recall_root:<rootID> plus the root row, ordered by CreatedAt
// ASC.
func (s *SurrealStore) Recall(ctx context.Context, rootID string, memberships []string) ([]LedgerEntry, error) {
	if rootID == "" {
		return nil, errors.New("ledger: recall requires root id")
	}
	if memberships != nil && len(memberships) == 0 {
		return nil, nil
	}
	conds := []string{"(meta::id(id) = $root OR tags CONTAINS $tag)"}
	vars := map[string]any{"root": rootID, "tag": "recall_root:" + rootID}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at ASC", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: recall: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	return (*results)[0].Result, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string, memberships []string) (LedgerEntry, error) {
	if memberships != nil && len(memberships) == 0 {
		return LedgerEntry{}, ErrNotFound
	}
	conds := []string{"meta::id(id) = $id"}
	vars := map[string]any{"id": id}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s LIMIT 1", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: get: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return LedgerEntry{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// Dereference implements Store for SurrealStore.
func (s *SurrealStore) Dereference(ctx context.Context, id, reason, actor string, now time.Time, memberships []string) (LedgerEntry, error) {
	target, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return LedgerEntry{}, err
	}
	auditEntry := LedgerEntry{
		WorkspaceID: target.WorkspaceID,
		ProjectID:   target.ProjectID,
		StoryID:     target.StoryID,
		ContractID:  target.ContractID,
		Type:        TypeDecision,
		Tags:        []string{"kind:dereference", "target:" + id},
		Content:     reason,
		CreatedBy:   actor,
	}
	written, err := s.Append(ctx, auditEntry, now)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: write audit row: %w", err)
	}
	updateSQL := "UPDATE $rid SET status = 'dereferenced'"
	updateVars := map[string]any{"rid": surrealmodels.NewRecordID("ledger", id)}
	if _, err := surrealdb.Query[any](ctx, s.db, updateSQL, updateVars); err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: dereference target: %w", err)
	}
	return written, nil
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

// MigrateLegacyRows stamps the v4 enum + naming on rows that pre-date the
// schema reshape (story_368cd70f). Idempotent on every boot. Once every
// row has a non-empty `created_by`, the legacy `actor` field is dropped.
func (s *SurrealStore) MigrateLegacyRows(ctx context.Context, now time.Time) (int, error) {
	stamps := []struct {
		label string
		sql   string
	}{
		{"created_by=actor", "UPDATE ledger SET created_by = actor WHERE (created_by IS NONE OR created_by = '') AND actor IS NOT NONE RETURN AFTER"},
		{"durability=durable", "UPDATE ledger SET durability = 'durable' WHERE durability IS NONE OR durability = '' RETURN AFTER"},
		{"source_type=agent", "UPDATE ledger SET source_type = 'agent' WHERE source_type IS NONE OR source_type = '' RETURN AFTER"},
		{"status=active", "UPDATE ledger SET status = 'active' WHERE status IS NONE OR status = '' RETURN AFTER"},
	}
	stamped := 0
	for _, q := range stamps {
		results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, q.sql, nil)
		if err != nil {
			return stamped, fmt.Errorf("ledger: migrate %s: %w", q.label, err)
		}
		if results != nil && len(*results) > 0 {
			stamped += len((*results)[0].Result)
		}
	}
	type cnt struct {
		N int `json:"n"`
	}
	countSQL := "SELECT count() AS n FROM ledger WHERE actor IS NOT NONE AND actor != '' GROUP ALL"
	cres, err := surrealdb.Query[[]cnt](ctx, s.db, countSQL, nil)
	if err != nil {
		return stamped, nil
	}
	remaining := 0
	if cres != nil && len(*cres) > 0 && len((*cres)[0].Result) > 0 {
		remaining = (*cres)[0].Result[0].N
	}
	if remaining == 0 {
		_, _ = surrealdb.Query[any](ctx, s.db, "REMOVE FIELD actor ON ledger", nil)
	}
	return stamped, nil
}

// Compile-time assertion that SurrealStore satisfies Store.
var _ Store = (*SurrealStore)(nil)

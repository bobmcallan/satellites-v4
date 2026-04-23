package document

import (
	"context"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is a SurrealDB-backed Store. It assumes the caller has
// already authenticated + selected ns/db on the supplied *surrealdb.DB.
type SurrealStore struct {
	db *surrealdb.DB
}

// NewSurrealStore wraps db as a Store. Defines the `documents` table
// schemaless so first-time SELECTs don't error on a missing table — v3
// SurrealDB rejects SELECT from undefined tables.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS documents SCHEMALESS", nil)
	return s
}

// selectCols preserves the string form of id. SurrealDB otherwise returns
// id as a RecordID object which JSON-unmarshals as empty into `ID string`.
const selectCols = "meta::id(id) AS id, workspace_id, project_id, filename, type, body, body_hash, status, version, created_at, updated_at"

func (s *SurrealStore) Upsert(ctx context.Context, workspaceID, projectID, filename, docType string, body []byte, now time.Time) (UpsertResult, error) {
	hash := HashBody(body)
	existing, err := s.GetByFilename(ctx, projectID, filename, nil)
	if err == nil {
		if existing.BodyHash == hash {
			return UpsertResult{Document: existing}, nil
		}
		updated := existing
		updated.Body = string(body)
		updated.BodyHash = hash
		updated.Version = existing.Version + 1
		updated.UpdatedAt = now
		updated.Type = docType
		if updated.WorkspaceID == "" {
			updated.WorkspaceID = workspaceID
		}
		if err := s.write(ctx, updated); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Document: updated, Changed: true}, nil
	}
	doc := Document{
		ID:          NewID(),
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Filename:    filename,
		Type:        docType,
		Body:        string(body),
		BodyHash:    hash,
		Status:      "active",
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.write(ctx, doc); err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Document: doc, Changed: true, Created: true}, nil
}

func (s *SurrealStore) write(ctx context.Context, doc Document) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("documents", doc.ID),
		"doc": doc,
	}
	if _, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("document: upsert: %w", err)
	}
	return nil
}

func (s *SurrealStore) GetByFilename(ctx context.Context, projectID, filename string, memberships []string) (Document, error) {
	if memberships != nil && len(memberships) == 0 {
		return Document{}, ErrNotFound
	}
	conds := []string{"project_id = $project", "filename = $filename", "status = 'active'"}
	vars := map[string]any{"project": projectID, "filename": filename}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	where := conds[0]
	for i := 1; i < len(conds); i++ {
		where += " AND " + conds[i]
	}
	sql := fmt.Sprintf("SELECT %s FROM documents WHERE %s LIMIT 1", selectCols, where)
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return Document{}, fmt.Errorf("document: select by filename: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Document{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

func (s *SurrealStore) Count(ctx context.Context, projectID string, memberships []string) (int, error) {
	if memberships != nil && len(memberships) == 0 {
		return 0, nil
	}
	conds := []string{"project_id = $project", "status = 'active'"}
	vars := map[string]any{"project": projectID}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	where := conds[0]
	for i := 1; i < len(conds); i++ {
		where += " AND " + conds[i]
	}
	sql := fmt.Sprintf("SELECT count() AS n FROM documents WHERE %s GROUP ALL", where)
	type row struct {
		N int `json:"n"`
	}
	results, err := surrealdb.Query[[]row](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("document: count: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return 0, nil
	}
	return (*results)[0].Result[0].N, nil
}

// BackfillProjectID stamps rows that lack a project_id with defaultID. This
// is a one-pass idempotent migration for documents seeded before the
// project primitive existed. Second boot is a no-op because the WHERE
// clause filters out already-stamped rows.
func (s *SurrealStore) BackfillProjectID(ctx context.Context, defaultID string) (int, error) {
	sql := "UPDATE documents SET project_id = $project WHERE project_id IS NONE OR project_id = '' RETURN AFTER"
	vars := map[string]any{"project": defaultID}
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("document: backfill project_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// BackfillWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	sql := "UPDATE documents SET workspace_id = $ws, updated_at = $now WHERE project_id = $project AND (workspace_id IS NONE OR workspace_id = '') RETURN AFTER"
	vars := map[string]any{"ws": workspaceID, "project": projectID, "now": now}
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("document: backfill workspace_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

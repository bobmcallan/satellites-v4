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

type surrealQueryResult[T any] struct {
	Status string `json:"status"`
	Result T      `json:"result"`
	Time   string `json:"time"`
}

func (s *SurrealStore) Upsert(ctx context.Context, filename, docType string, body []byte, now time.Time) (UpsertResult, error) {
	hash := HashBody(body)
	existing, err := s.GetByFilename(ctx, filename)
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
		if err := s.write(ctx, updated); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Document: updated, Changed: true}, nil
	}
	doc := Document{
		ID:        NewID(),
		Filename:  filename,
		Type:      docType,
		Body:      string(body),
		BodyHash:  hash,
		Status:    "active",
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
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

func (s *SurrealStore) GetByFilename(ctx context.Context, filename string) (Document, error) {
	sql := "SELECT * FROM documents WHERE filename = $filename AND status = 'active' LIMIT 1"
	vars := map[string]any{"filename": filename}
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return Document{}, fmt.Errorf("document: select by filename: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Document{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

func (s *SurrealStore) Count(ctx context.Context) (int, error) {
	sql := "SELECT count() AS n FROM documents WHERE status = 'active' GROUP ALL"
	type row struct {
		N int `json:"n"`
	}
	results, err := surrealdb.Query[[]row](ctx, s.db, sql, nil)
	if err != nil {
		return 0, fmt.Errorf("document: count: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return 0, nil
	}
	return (*results)[0].Result[0].N, nil
}

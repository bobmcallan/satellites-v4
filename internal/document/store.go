package document

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a document lookup misses.
var ErrNotFound = errors.New("document: not found")

// UpsertResult is the outcome of Upsert. Changed==false means the body
// matched the existing hash, so version and body were left untouched.
type UpsertResult struct {
	Document Document
	Changed  bool
	Created  bool
}

// Store is the persistence surface for documents. SurrealStore is the
// production implementation; MemoryStore is the in-process test double.
//
// All read/write operations are scoped by projectID — a document belongs to
// exactly one project per principle pr_7ade92ae.
type Store interface {
	// Upsert inserts or updates a document keyed by (projectID, filename).
	// If the body hash matches the existing row, no write happens and
	// Changed=false. WorkspaceID is stamped on create and left alone on
	// version bumps (feature-order:2 migration path keeps back-compat).
	Upsert(ctx context.Context, workspaceID, projectID, filename, docType string, body []byte, now time.Time) (UpsertResult, error)

	// GetByFilename returns the active document with the given filename
	// inside projectID. memberships: nil = no scoping, empty = deny-all,
	// non-empty = workspace_id IN memberships (docs/architecture.md §8).
	GetByFilename(ctx context.Context, projectID, filename string, memberships []string) (Document, error)

	// Count returns the number of active documents in projectID. Boot
	// seeding uses this to skip work on a pre-populated project.
	// memberships semantics match GetByFilename.
	Count(ctx context.Context, projectID string, memberships []string) (int, error)

	// BackfillWorkspaceID stamps workspaceID on documents with the given
	// projectID whose workspace_id is empty. Feature-order:2 migration;
	// idempotent.
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Document // key = projectID|filename
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Document)}
}

func memKey(projectID, filename string) string {
	return projectID + "|" + filename
}

// Upsert implements Store for MemoryStore.
func (m *MemoryStore) Upsert(ctx context.Context, workspaceID, projectID, filename, docType string, body []byte, now time.Time) (UpsertResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hash := HashBody(body)
	k := memKey(projectID, filename)
	if existing, ok := m.rows[k]; ok {
		if existing.BodyHash == hash {
			return UpsertResult{Document: existing}, nil
		}
		existing.Body = string(body)
		existing.BodyHash = hash
		existing.Version++
		existing.UpdatedAt = now
		existing.Type = docType
		if existing.WorkspaceID == "" {
			existing.WorkspaceID = workspaceID
		}
		m.rows[k] = existing
		return UpsertResult{Document: existing, Changed: true}, nil
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
	m.rows[k] = doc
	return UpsertResult{Document: doc, Changed: true, Created: true}, nil
}

// GetByFilename implements Store for MemoryStore.
func (m *MemoryStore) GetByFilename(ctx context.Context, projectID, filename string, memberships []string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.rows[memKey(projectID, filename)]
	if !ok {
		return Document{}, ErrNotFound
	}
	if !inDocMemberships(doc.WorkspaceID, memberships) {
		return Document{}, ErrNotFound
	}
	return doc, nil
}

// Count implements Store for MemoryStore.
func (m *MemoryStore) Count(ctx context.Context, projectID string, memberships []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, d := range m.rows {
		if d.ProjectID != projectID {
			continue
		}
		if !inDocMemberships(d.WorkspaceID, memberships) {
			continue
		}
		n++
	}
	return n, nil
}

// inDocMemberships is the shared membership predicate for document rows.
// nil = no filter, empty = deny-all, non-empty = workspace_id IN memberships.
func inDocMemberships(wsID string, memberships []string) bool {
	if memberships == nil {
		return true
	}
	for _, m := range memberships {
		if m == wsID {
			return true
		}
	}
	return false
}

// NewID returns a fresh document id in the canonical `doc_<8hex>` form.
// Exported so the surreal store + memory store + tests mint ids identically.
func NewID() string {
	return fmt.Sprintf("doc_%s", uuid.NewString()[:8])
}

// BackfillWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, d := range m.rows {
		if d.ProjectID != projectID || d.WorkspaceID != "" {
			continue
		}
		d.WorkspaceID = workspaceID
		d.UpdatedAt = now
		m.rows[k] = d
		n++
	}
	return n, nil
}

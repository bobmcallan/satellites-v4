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
type Store interface {
	// Upsert inserts or updates a document keyed by filename. If the body
	// hash matches the existing row, no write happens and Changed=false.
	Upsert(ctx context.Context, filename, docType string, body []byte, now time.Time) (UpsertResult, error)

	// GetByFilename returns the active document with the given filename.
	GetByFilename(ctx context.Context, filename string) (Document, error)

	// Count returns the number of active documents. Boot seeding uses this
	// to skip work on a pre-populated DB.
	Count(ctx context.Context) (int, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Document // key = filename
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Document)}
}

// Upsert implements Store for MemoryStore.
func (m *MemoryStore) Upsert(ctx context.Context, filename, docType string, body []byte, now time.Time) (UpsertResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hash := HashBody(body)
	if existing, ok := m.rows[filename]; ok {
		if existing.BodyHash == hash {
			return UpsertResult{Document: existing}, nil
		}
		existing.Body = string(body)
		existing.BodyHash = hash
		existing.Version++
		existing.UpdatedAt = now
		existing.Type = docType
		m.rows[filename] = existing
		return UpsertResult{Document: existing, Changed: true}, nil
	}
	doc := Document{
		ID:        "doc_" + uuid.NewString()[:8],
		Filename:  filename,
		Type:      docType,
		Body:      string(body),
		BodyHash:  hash,
		Status:    "active",
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.rows[filename] = doc
	return UpsertResult{Document: doc, Changed: true, Created: true}, nil
}

// GetByFilename implements Store for MemoryStore.
func (m *MemoryStore) GetByFilename(ctx context.Context, filename string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.rows[filename]
	if !ok {
		return Document{}, ErrNotFound
	}
	return doc, nil
}

// Count implements Store for MemoryStore.
func (m *MemoryStore) Count(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows), nil
}

// NewID returns a fresh document id in the canonical `doc_<8hex>` form.
// Exported so the surreal store + memory store + tests mint ids identically.
func NewID() string {
	return fmt.Sprintf("doc_%s", uuid.NewString()[:8])
}

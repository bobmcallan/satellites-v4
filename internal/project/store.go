package project

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a project lookup misses.
var ErrNotFound = errors.New("project: not found")

// Store is the persistence surface for projects. SurrealStore is the
// production implementation; MemoryStore is the in-process test double.
//
// Mutation surface is intentionally narrow at v4 baseline: Create + UpdateName.
// Archive / delete / membership verbs arrive in follow-up stories.
type Store interface {
	// Create persists a new Project. The caller supplies ownerUserID + name;
	// the store mints the id, stamps CreatedAt/UpdatedAt, and sets Status
	// to StatusActive. Returns the resulting Project.
	Create(ctx context.Context, ownerUserID, name string, now time.Time) (Project, error)

	// GetByID returns the project with the given id, or ErrNotFound.
	GetByID(ctx context.Context, id string) (Project, error)

	// ListByOwner returns the owner's projects, newest-first by CreatedAt.
	ListByOwner(ctx context.Context, ownerUserID string) ([]Project, error)

	// UpdateName renames an existing project and bumps UpdatedAt. Returns the
	// updated Project. ErrNotFound on missing id.
	UpdateName(ctx context.Context, id, name string, now time.Time) (Project, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Project // key = id
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Project)}
}

// Create implements Store for MemoryStore.
func (m *MemoryStore) Create(ctx context.Context, ownerUserID, name string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := Project{
		ID:          NewID(),
		Name:        name,
		OwnerUserID: ownerUserID,
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	m.rows[p.ID] = p
	return p, nil
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

// ListByOwner implements Store for MemoryStore.
func (m *MemoryStore) ListByOwner(ctx context.Context, ownerUserID string) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Project, 0)
	for _, p := range m.rows {
		if p.OwnerUserID == ownerUserID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// UpdateName implements Store for MemoryStore.
func (m *MemoryStore) UpdateName(ctx context.Context, id, name string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	p.Name = name
	p.UpdatedAt = now
	m.rows[id] = p
	return p, nil
}

// Compile-time assertion that MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

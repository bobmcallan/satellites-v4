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
	// Create persists a new Project. The caller supplies ownerUserID +
	// workspaceID + name; the store mints the id, stamps CreatedAt/UpdatedAt,
	// and sets Status to StatusActive. An empty workspaceID is permitted at
	// write time so bootstrap + legacy paths can run; the boot-time backfill
	// stamps empty rows with the owner's default workspace.
	Create(ctx context.Context, ownerUserID, workspaceID, name string, now time.Time) (Project, error)

	// GetByID returns the project with the given id, or ErrNotFound. When
	// memberships is non-nil the row must carry a workspace_id that appears
	// in the slice; non-member rows return ErrNotFound (the same shape a
	// missing row would). nil memberships disable scoping (bootstrap and
	// backfill paths that must see every row).
	GetByID(ctx context.Context, id string, memberships []string) (Project, error)

	// ListByOwner returns the owner's projects, newest-first by CreatedAt.
	// memberships scoping matches GetByID semantics: nil = no scoping,
	// empty = deny-all, non-empty = workspace_id IN memberships.
	ListByOwner(ctx context.Context, ownerUserID string, memberships []string) ([]Project, error)

	// UpdateName renames an existing project and bumps UpdatedAt. Returns the
	// updated Project. ErrNotFound on missing id.
	UpdateName(ctx context.Context, id, name string, now time.Time) (Project, error)

	// SetWorkspaceID stamps workspaceID on an existing project. Used by the
	// boot-time backfill to migrate rows that pre-date workspace scoping.
	SetWorkspaceID(ctx context.Context, id, workspaceID string, now time.Time) (Project, error)

	// ListMissingWorkspaceID returns rows whose workspace_id is empty.
	// Backfill uses this to find work to do.
	ListMissingWorkspaceID(ctx context.Context) ([]Project, error)
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
func (m *MemoryStore) Create(ctx context.Context, ownerUserID, workspaceID, name string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := Project{
		ID:          NewID(),
		WorkspaceID: workspaceID,
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
func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	if !inMemberships(p.WorkspaceID, memberships) {
		return Project{}, ErrNotFound
	}
	return p, nil
}

// ListByOwner implements Store for MemoryStore.
func (m *MemoryStore) ListByOwner(ctx context.Context, ownerUserID string, memberships []string) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Project, 0)
	for _, p := range m.rows {
		if p.OwnerUserID != ownerUserID {
			continue
		}
		if !inMemberships(p.WorkspaceID, memberships) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// inMemberships is the shared membership-filter predicate. nil = no filter
// (seed/backfill paths); empty slice = deny-all; non-empty = row passes if
// its workspace_id is in the slice.
func inMemberships(wsID string, memberships []string) bool {
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

// SetWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) SetWorkspaceID(ctx context.Context, id, workspaceID string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	p.WorkspaceID = workspaceID
	p.UpdatedAt = now
	m.rows[id] = p
	return p, nil
}

// ListMissingWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) ListMissingWorkspaceID(ctx context.Context) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Project, 0)
	for _, p := range m.rows {
		if p.WorkspaceID == "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// Compile-time assertion that MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

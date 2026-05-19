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
// The project↔git-remote binding lives on the per-project repo row
// (internal/repo). Lookups by remote go through repo.Store.GetByRemote,
// not this interface. sty_14dfd05b dropped the legacy
// CreateWithRemote / GetByGitRemote / SetGitRemote surface.
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

	// SetMCPURL stamps an explicit mcp_url on an existing project,
	// overriding the derived form. Pass an empty string to clear the
	// override (the derived form takes over again).
	SetMCPURL(ctx context.Context, id, mcpURL string, now time.Time) (Project, error)

	// SetDescription stamps the free-form description on an existing
	// project. Pass an empty string to clear.
	SetDescription(ctx context.Context, id, description string, now time.Time) (Project, error)

	// SetStatus flips a project's status (active ↔ archived). Soft-delete
	// path; rows are never physically removed. Returns the updated Project.
	SetStatus(ctx context.Context, id, status string, now time.Time) (Project, error)

	// SetWorkspaceID stamps workspaceID on an existing project. Used by the
	// boot-time backfill to migrate rows that pre-date workspace scoping.
	SetWorkspaceID(ctx context.Context, id, workspaceID string, now time.Time) (Project, error)

	// ListMissingWorkspaceID returns rows whose workspace_id is empty.
	// Backfill uses this to find work to do.
	ListMissingWorkspaceID(ctx context.Context) ([]Project, error)

	// Delete hard-removes the project row. ErrNotFound when the id isn't
	// present. Sty_d357b28d — used by the project_delete hard-purge path;
	// the caller is responsible for cascading hard-deletes on dependent
	// rows (stories, tasks, ledger, api-keys, repo) BEFORE invoking this.
	Delete(ctx context.Context, id string) error

	// ListByWorkspaceID returns every project (active + archived,
	// regardless of owner) bound to the given workspace_id. Used by
	// `workspace_delete` to enumerate blocking projects before refusing
	// the destructive op. Sty_d357b28d.
	ListByWorkspaceID(ctx context.Context, workspaceID string) ([]Project, error)
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

// SetMCPURL implements Store for MemoryStore.
func (m *MemoryStore) SetMCPURL(ctx context.Context, id, mcpURL string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	p.MCPURL = mcpURL
	p.UpdatedAt = now
	m.rows[id] = p
	return p, nil
}

// SetDescription implements Store for MemoryStore.
func (m *MemoryStore) SetDescription(ctx context.Context, id, description string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	p.Description = description
	p.UpdatedAt = now
	m.rows[id] = p
	return p, nil
}

// SetStatus implements Store for MemoryStore.
func (m *MemoryStore) SetStatus(ctx context.Context, id, status string, now time.Time) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return Project{}, ErrNotFound
	}
	p.Status = status
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

// ListAll returns every row regardless of owner / workspace / status.
// Migration-only escape hatch — not on the Store interface. Callers use
// a type assertion to detect support.
func (m *MemoryStore) ListAll(ctx context.Context) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Project, 0, len(m.rows))
	for _, p := range m.rows {
		out = append(out, p)
	}
	return out, nil
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

// Delete implements Store for MemoryStore. Hard-removes the project row.
// Sty_d357b28d.
func (m *MemoryStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[id]; !ok {
		return ErrNotFound
	}
	delete(m.rows, id)
	return nil
}

// ListByWorkspaceID implements Store for MemoryStore. Sty_d357b28d.
func (m *MemoryStore) ListByWorkspaceID(ctx context.Context, workspaceID string) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Project, 0)
	for _, p := range m.rows {
		if p.WorkspaceID != workspaceID {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Compile-time assertion that MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

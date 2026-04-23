package workspace

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a workspace lookup misses.
var ErrNotFound = errors.New("workspace: not found")

// ErrInvalidRole is returned when a caller passes a role outside the
// admin/member/reviewer/viewer enum.
var ErrInvalidRole = errors.New("workspace: invalid role")

// Store is the persistence surface for workspaces and their members.
// SurrealStore is the production implementation; MemoryStore is the in-process
// test double.
//
// Membership mutation beyond the creator-as-admin pattern is intentionally
// out of scope at feature-order:1 — full member management verbs arrive in
// feature-order:4.
type Store interface {
	// Create persists a new Workspace and records its creator as admin. The
	// caller supplies ownerUserID + name; the store mints the id, stamps
	// CreatedAt/UpdatedAt, and sets Status to StatusActive.
	Create(ctx context.Context, ownerUserID, name string, now time.Time) (Workspace, error)

	// GetByID returns the workspace with the given id, or ErrNotFound.
	GetByID(ctx context.Context, id string) (Workspace, error)

	// ListByMember returns the workspaces the given user belongs to,
	// newest-first by CreatedAt.
	ListByMember(ctx context.Context, userID string) ([]Workspace, error)

	// IsMember reports whether userID is a member of workspaceID.
	IsMember(ctx context.Context, workspaceID, userID string) (bool, error)

	// GetRole returns the member's role on the workspace, or ErrNotFound
	// when there is no membership row.
	GetRole(ctx context.Context, workspaceID, userID string) (string, error)

	// AddMember inserts (or updates) a membership row. The full MCP verb
	// surface (add/list/update_role/remove with the admin-only guard + the
	// last-admin guard) lands in feature-order:4; this minimal store verb
	// is here now so the boot path can grant the "apikey" synthetic user
	// access to the system workspace without bespoke surgery.
	AddMember(ctx context.Context, workspaceID, userID, role, addedBy string, now time.Time) error
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu      sync.Mutex
	rows    map[string]Workspace         // key = workspace id
	members map[string]map[string]Member // key = workspace id -> user id
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		rows:    make(map[string]Workspace),
		members: make(map[string]map[string]Member),
	}
}

// Create implements Store for MemoryStore.
func (m *MemoryStore) Create(ctx context.Context, ownerUserID, name string, now time.Time) (Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := Workspace{
		ID:          NewID(),
		Name:        name,
		OwnerUserID: ownerUserID,
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	m.rows[w.ID] = w
	m.members[w.ID] = map[string]Member{
		ownerUserID: {
			WorkspaceID: w.ID,
			UserID:      ownerUserID,
			Role:        RoleAdmin,
			AddedAt:     now,
			AddedBy:     ownerUserID,
		},
	}
	return w, nil
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string) (Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.rows[id]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return w, nil
}

// ListByMember implements Store for MemoryStore.
func (m *MemoryStore) ListByMember(ctx context.Context, userID string) ([]Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Workspace, 0)
	for wsID, members := range m.members {
		if _, ok := members[userID]; !ok {
			continue
		}
		if w, ok := m.rows[wsID]; ok {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// IsMember implements Store for MemoryStore.
func (m *MemoryStore) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return false, nil
	}
	_, ok = members[userID]
	return ok, nil
}

// GetRole implements Store for MemoryStore.
func (m *MemoryStore) GetRole(ctx context.Context, workspaceID, userID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return "", ErrNotFound
	}
	member, ok := members[userID]
	if !ok {
		return "", ErrNotFound
	}
	return member.Role, nil
}

// AddMember implements Store for MemoryStore.
func (m *MemoryStore) AddMember(ctx context.Context, workspaceID, userID, role, addedBy string, now time.Time) error {
	if !IsValidRole(role) {
		return ErrInvalidRole
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[workspaceID]; !ok {
		return ErrNotFound
	}
	members, ok := m.members[workspaceID]
	if !ok {
		members = map[string]Member{}
		m.members[workspaceID] = members
	}
	members[userID] = Member{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Role:        role,
		AddedAt:     now,
		AddedBy:     addedBy,
	}
	return nil
}

var _ Store = (*MemoryStore)(nil)

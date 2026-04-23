package ledger

import (
	"context"
	"sort"
	"sync"
	"time"
)

// DefaultListLimit is applied when ListOptions.Limit <= 0.
const DefaultListLimit = 100

// MaxListLimit is the ceiling — higher values clamp to this.
const MaxListLimit = 500

// ListOptions filters a List call. Type="" means no type filter; Limit<=0
// uses DefaultListLimit; Limit>MaxListLimit clamps to MaxListLimit.
type ListOptions struct {
	Type  string
	Limit int
}

// normalised returns opts with Limit clamped into [1, MaxListLimit].
func (o ListOptions) normalised() ListOptions {
	if o.Limit <= 0 {
		o.Limit = DefaultListLimit
	}
	if o.Limit > MaxListLimit {
		o.Limit = MaxListLimit
	}
	return o
}

// Store is the persistence surface for the ledger. The interface is
// intentionally narrow — Append + List only, no mutation verbs. Append-only
// is enforced at the interface level so a reviewer can audit this file and
// confirm no mutation paths exist.
//
// BackfillWorkspaceID is the sole exception and is allowed for the
// feature-order:2 migration from pre-workspace rows; it only stamps
// workspace_id on rows where it was empty, never rewrites any other field.
type Store interface {
	// Append persists a new entry. The Store mints ID + CreatedAt; the
	// caller supplies WorkspaceID, ProjectID, Type, Content, Actor. Returns
	// the stored entry with the server-assigned fields filled in.
	Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error)

	// List returns entries for projectID, newest-first, subject to opts.
	// memberships: nil = no scoping, empty = deny-all, non-empty =
	// workspace_id IN memberships (docs/architecture.md §8).
	List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error)

	// BackfillWorkspaceID stamps workspaceID on ledger rows with the given
	// projectID whose workspace_id is empty. Feature-order:2 migration;
	// idempotent.
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows []LedgerEntry
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make([]LedgerEntry, 0)}
}

// Append implements Store for MemoryStore.
func (m *MemoryStore) Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.ID = NewID()
	entry.CreatedAt = now
	m.rows = append(m.rows, entry)
	return entry, nil
}

// List implements Store for MemoryStore.
func (m *MemoryStore) List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error) {
	opts = opts.normalised()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LedgerEntry, 0)
	for _, e := range m.rows {
		if e.ProjectID != projectID {
			continue
		}
		if !inLedgerMemberships(e.WorkspaceID, memberships) {
			continue
		}
		if opts.Type != "" && e.Type != opts.Type {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// inLedgerMemberships is the shared membership predicate for ledger rows.
// nil = no filter, empty = deny-all, non-empty = workspace_id IN memberships.
func inLedgerMemberships(wsID string, memberships []string) bool {
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

// BackfillWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i, e := range m.rows {
		if e.ProjectID != projectID || e.WorkspaceID != "" {
			continue
		}
		e.WorkspaceID = workspaceID
		m.rows[i] = e
		n++
	}
	return n, nil
}

// Compile-time assertion that MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

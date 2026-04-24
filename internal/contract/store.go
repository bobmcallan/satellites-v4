package contract

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/story"
)

// ErrNotFound is returned when a contract_instance lookup misses.
var ErrNotFound = errors.New("contract: not found")

// ErrDanglingContract is returned when a write references a ContractID
// that does not resolve to an active `document{type=contract}` visible
// in the caller's memberships or with scope=system.
var ErrDanglingContract = errors.New("contract: contract_id does not resolve to an active type=contract document")

// ErrMissingStory is returned when Create references a StoryID that does
// not resolve via the injected story.Store.
var ErrMissingStory = errors.New("contract: story_id does not resolve")

// Store is the persistence surface for contract_instances. There is no
// Delete verb — CIs persist for audit (pr_0c11b762).
//
// Workspace scoping is enforced via the memberships slice: nil = no
// filter, empty = deny-all, non-empty = workspace_id IN memberships
// (docs/architecture.md §8).
type Store interface {
	// Create writes a new CI. WorkspaceID + ProjectID cascade from the
	// parent story row — any caller-supplied values on ci are ignored.
	// ContractID is validated against the document store at write time.
	Create(ctx context.Context, ci ContractInstance, now time.Time) (ContractInstance, error)

	// GetByID returns the CI with the given id, or ErrNotFound.
	GetByID(ctx context.Context, id string, memberships []string) (ContractInstance, error)

	// List returns every CI on storyID ordered by sequence ASC. Empty
	// storyID is rejected.
	List(ctx context.Context, storyID string, memberships []string) ([]ContractInstance, error)

	// UpdateStatus transitions the CI's Status. ValidTransition is
	// enforced at the store layer — invalid moves return
	// ErrInvalidTransition. actor is persisted on UpdatedAt only; the
	// audit ledger row is the caller's responsibility (the workflow
	// contracts in 8.2-8.5 write those rows).
	UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time, memberships []string) (ContractInstance, error)

	// UpdateLedgerRefs stamps PlanLedgerID / CloseLedgerID. nil = leave
	// alone, non-nil = set (empty string clears). Used by the claim and
	// close verbs in 8.3 / 8.4.
	UpdateLedgerRefs(ctx context.Context, id string, plan, close *string, actor string, now time.Time, memberships []string) (ContractInstance, error)

	// Claim atomically transitions a CI from ready → claimed and binds
	// it to sessionID. Rejects with ErrInvalidTransition if CI.Status
	// is not ready; callers handle the same-session amend path
	// explicitly before calling Claim.
	Claim(ctx context.Context, id, sessionID string, now time.Time, memberships []string) (ContractInstance, error)

	// RebindSession updates ClaimedBySessionID on a CI that is already
	// claimed. Used by resume to rebind after a session restart.
	RebindSession(ctx context.Context, id, sessionID string, now time.Time, memberships []string) (ContractInstance, error)

	// ClearClaim flips a CI back to ready and clears
	// ClaimedBySessionID + ClaimedAt + PlanLedgerID. Used by the resume
	// downstream-rollback path when an earlier CI is re-opened.
	ClearClaim(ctx context.Context, id string, now time.Time, memberships []string) (ContractInstance, error)

	// BackfillWorkspaceID stamps workspace_id on rows matching projectID
	// whose workspace_id is empty. Idempotent boot-time migration.
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error)

	// SetClaimedViaGrant stamps the grant id on a CI that has already
	// been claimed. Called by the claim handler after it resolves the
	// caller's orchestrator grant (story_85675c33). Returns ErrNotFound
	// if the CI does not exist.
	SetClaimedViaGrant(ctx context.Context, id, grantID string, now time.Time, memberships []string) (ContractInstance, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
// It depends on a document.Store for FK resolution and a story.Store for
// parent-row lookup on Create.
type MemoryStore struct {
	mu      sync.Mutex
	rows    map[string]ContractInstance
	docs    document.Store
	stories story.Store
}

// NewMemoryStore returns an empty MemoryStore. docs and stories are
// required — Create needs both to resolve FKs and cascade
// workspace/project scope. Passing nil for either panics.
func NewMemoryStore(docs document.Store, stories story.Store) *MemoryStore {
	if docs == nil {
		panic("contract.MemoryStore requires a non-nil document.Store")
	}
	if stories == nil {
		panic("contract.MemoryStore requires a non-nil story.Store")
	}
	return &MemoryStore{
		rows:    make(map[string]ContractInstance),
		docs:    docs,
		stories: stories,
	}
}

// Create implements Store for MemoryStore.
func (m *MemoryStore) Create(ctx context.Context, ci ContractInstance, now time.Time) (ContractInstance, error) {
	if ci.StoryID == "" {
		return ContractInstance{}, fmt.Errorf("contract: story_id is required")
	}
	if ci.ContractID == "" {
		return ContractInstance{}, fmt.Errorf("contract: contract_id is required")
	}
	if ci.ContractName == "" {
		return ContractInstance{}, fmt.Errorf("contract: contract_name is required")
	}
	if ci.Status == "" {
		ci.Status = StatusReady
	}
	if !IsKnownStatus(ci.Status) {
		return ContractInstance{}, fmt.Errorf("contract: unknown status %q", ci.Status)
	}

	parent, err := m.stories.GetByID(ctx, ci.StoryID, nil)
	if err != nil {
		return ContractInstance{}, ErrMissingStory
	}
	ci.WorkspaceID = parent.WorkspaceID
	ci.ProjectID = parent.ProjectID

	if err := validateContractBinding(ctx, m.docs, ci.ContractID, ci.WorkspaceID); err != nil {
		return ContractInstance{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	ci.ID = NewID()
	ci.CreatedAt = now
	ci.UpdatedAt = now
	m.rows[ci.ID] = ci
	return ci, nil
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (ContractInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok {
		return ContractInstance{}, ErrNotFound
	}
	if !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	return ci, nil
}

// List implements Store for MemoryStore.
func (m *MemoryStore) List(ctx context.Context, storyID string, memberships []string) ([]ContractInstance, error) {
	if storyID == "" {
		return nil, fmt.Errorf("contract: story_id is required")
	}
	if memberships != nil && len(memberships) == 0 {
		return []ContractInstance{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ContractInstance, 0)
	for _, ci := range m.rows {
		if ci.StoryID != storyID {
			continue
		}
		if !inMemberships(ci.WorkspaceID, memberships) {
			continue
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

// UpdateStatus implements Store for MemoryStore.
func (m *MemoryStore) UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time, memberships []string) (ContractInstance, error) {
	if !IsKnownStatus(newStatus) {
		return ContractInstance{}, fmt.Errorf("contract: unknown status %q", newStatus)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok || !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	if !ValidTransition(ci.Status, newStatus) {
		return ContractInstance{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, ci.Status, newStatus)
	}
	ci.Status = newStatus
	ci.UpdatedAt = now
	m.rows[id] = ci
	return ci, nil
}

// Claim implements Store for MemoryStore.
func (m *MemoryStore) Claim(ctx context.Context, id, sessionID string, now time.Time, memberships []string) (ContractInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok || !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	if !ValidTransition(ci.Status, StatusClaimed) {
		return ContractInstance{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, ci.Status, StatusClaimed)
	}
	ci.Status = StatusClaimed
	ci.ClaimedBySessionID = sessionID
	ci.ClaimedAt = now
	ci.UpdatedAt = now
	m.rows[id] = ci
	return ci, nil
}

// RebindSession implements Store for MemoryStore.
func (m *MemoryStore) RebindSession(ctx context.Context, id, sessionID string, now time.Time, memberships []string) (ContractInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok || !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	ci.ClaimedBySessionID = sessionID
	ci.UpdatedAt = now
	m.rows[id] = ci
	return ci, nil
}

// ClearClaim implements Store for MemoryStore.
func (m *MemoryStore) ClearClaim(ctx context.Context, id string, now time.Time, memberships []string) (ContractInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok || !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	ci.Status = StatusReady
	ci.ClaimedBySessionID = ""
	ci.ClaimedAt = time.Time{}
	ci.PlanLedgerID = ""
	ci.CloseLedgerID = ""
	ci.UpdatedAt = now
	m.rows[id] = ci
	return ci, nil
}

// UpdateLedgerRefs implements Store for MemoryStore.
func (m *MemoryStore) UpdateLedgerRefs(ctx context.Context, id string, plan, closeRef *string, actor string, now time.Time, memberships []string) (ContractInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok || !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	if plan != nil {
		ci.PlanLedgerID = *plan
	}
	if closeRef != nil {
		ci.CloseLedgerID = *closeRef
	}
	ci.UpdatedAt = now
	m.rows[id] = ci
	return ci, nil
}

// SetClaimedViaGrant implements Store for MemoryStore.
func (m *MemoryStore) SetClaimedViaGrant(ctx context.Context, id, grantID string, now time.Time, memberships []string) (ContractInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ci, ok := m.rows[id]
	if !ok || !inMemberships(ci.WorkspaceID, memberships) {
		return ContractInstance{}, ErrNotFound
	}
	ci.ClaimedViaGrantID = grantID
	ci.UpdatedAt = now
	m.rows[id] = ci
	return ci, nil
}

// BackfillWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, ci := range m.rows {
		if ci.ProjectID != projectID || ci.WorkspaceID != "" {
			continue
		}
		ci.WorkspaceID = workspaceID
		ci.UpdatedAt = now
		m.rows[id] = ci
		n++
	}
	return n, nil
}

// validateContractBinding resolves contractID against docs. Target must
// be an active `type=contract` row either scope=system (globally
// readable) or in the caller's workspaceID.
func validateContractBinding(ctx context.Context, docs document.Store, contractID, workspaceID string) error {
	if contractID == "" {
		return ErrDanglingContract
	}
	// Lookup with nil memberships so scope=system rows resolve; the
	// per-workspace check is applied below.
	target, err := docs.GetByID(ctx, contractID, nil)
	if err != nil {
		return ErrDanglingContract
	}
	if target.Type != document.TypeContract || target.Status != document.StatusActive {
		return ErrDanglingContract
	}
	if target.Scope == document.ScopeSystem {
		return nil
	}
	if target.WorkspaceID != workspaceID {
		return ErrDanglingContract
	}
	return nil
}

// inMemberships is the shared workspace-scoping predicate.
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

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

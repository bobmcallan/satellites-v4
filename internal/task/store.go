package task

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/bobmcallan/satellites/internal/hubemit"
)

// ErrNotFound is returned when a task lookup misses.
var ErrNotFound = errors.New("task: not found")

// ErrInvalidTransition is returned when Close / Reclaim attempt an
// illegal state move per ValidTransition.
var ErrInvalidTransition = errors.New("task: invalid status transition")

// ErrNoTaskAvailable is returned by Claim when no enqueued task is
// visible to the caller's workspace memberships. Not an error in the
// strict sense — callers back off + retry.
var ErrNoTaskAvailable = errors.New("task: no task available")

// ListOptions bundles structured List filters. Workspace scoping is
// supplied via memberships on the call itself, not through this struct.
type ListOptions struct {
	Origin    string
	Status    string
	Priority  string
	ClaimedBy string
	Limit     int
}

// Store is the persistence surface for tasks.
//
// Workspace scoping is enforced via the memberships slice per
// docs/architecture.md §8: nil = no scoping, empty = deny-all,
// non-empty = workspace_id IN memberships. Never nil in production
// call paths except for internal maintenance (e.g. cron dispatcher
// using system identity).
type Store interface {
	// Enqueue writes a new task with Status=enqueued. Validates enum
	// fields + workspace_id. Returns the inserted row with ID minted.
	Enqueue(ctx context.Context, t Task, now time.Time) (Task, error)

	// GetByID returns the task with the given id, or ErrNotFound. Scoped
	// by memberships.
	GetByID(ctx context.Context, id string, memberships []string) (Task, error)

	// List returns tasks matching opts ordered by priority then
	// created_at ASC. Memberships-scoped.
	List(ctx context.Context, opts ListOptions, memberships []string) ([]Task, error)

	// Claim atomically picks the highest-priority oldest-queued task
	// from workspaceIDs and transitions it enqueued → claimed. Returns
	// ErrNoTaskAvailable when the queue is empty for those workspaces.
	// Exactly one caller wins under concurrency.
	Claim(ctx context.Context, workerID string, workspaceIDs []string, now time.Time) (Task, error)

	// Close transitions a task to Status=closed with the given outcome.
	// Rejects invalid transitions via ErrInvalidTransition.
	Close(ctx context.Context, id, outcome string, now time.Time, memberships []string) (Task, error)

	// Reclaim transitions a claimed task back to Status=enqueued
	// (typically after watchdog expiry). Outcome=timeout is the
	// convention but not enforced at this layer. Increments
	// Task.ReclaimCount so a subsequent stale task_close from the
	// original claimer can be detected and rejected.
	Reclaim(ctx context.Context, id, reason string, now time.Time, memberships []string) (Task, error)

	// ListExpiring returns tasks whose Status is claimed or in_flight
	// AND (now - ClaimedAt) exceeds `threshold * ExpectedDuration`. Used
	// by the dispatcher watchdog. When ExpectedDuration is zero, the
	// row is skipped (no expiry budget to compute against).
	// Story_b4513c8c.
	ListExpiring(ctx context.Context, now time.Time, multiplier float64, memberships []string) ([]Task, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu        sync.Mutex
	rows      map[string]Task
	publisher hubemit.Publisher
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Task)}
}

// SetPublisher installs the hub emit sink for subsequent mutations.
func (m *MemoryStore) SetPublisher(p hubemit.Publisher) { m.publisher = p }

// Enqueue implements Store for MemoryStore.
func (m *MemoryStore) Enqueue(ctx context.Context, t Task, now time.Time) (Task, error) {
	if t.Status == "" {
		t.Status = StatusEnqueued
	}
	if t.Priority == "" {
		t.Priority = PriorityMedium
	}
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	if t.Status != StatusEnqueued {
		return Task{}, fmt.Errorf("task: Enqueue requires status=enqueued, got %q", t.Status)
	}
	m.mu.Lock()
	if t.ID == "" {
		t.ID = NewID()
	}
	if _, exists := m.rows[t.ID]; exists {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("task: id %q already exists", t.ID)
	}
	t.CreatedAt = now
	m.rows[t.ID] = t
	pub := m.publisher
	m.mu.Unlock()
	emitStatus(ctx, pub, t)
	return t, nil
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.rows[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	if !workspaceVisible(t.WorkspaceID, memberships) {
		return Task{}, ErrNotFound
	}
	return t, nil
}

// List implements Store for MemoryStore.
func (m *MemoryStore) List(ctx context.Context, opts ListOptions, memberships []string) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.rows))
	for _, t := range m.rows {
		if !workspaceVisible(t.WorkspaceID, memberships) {
			continue
		}
		if opts.Origin != "" && t.Origin != opts.Origin {
			continue
		}
		if opts.Status != "" && t.Status != opts.Status {
			continue
		}
		if opts.Priority != "" && t.Priority != opts.Priority {
			continue
		}
		if opts.ClaimedBy != "" && t.ClaimedBy != opts.ClaimedBy {
			continue
		}
		out = append(out, t)
	}
	sortByPriorityThenCreated(out)
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Claim implements Store for MemoryStore. Mutex-guarded scan guarantees
// atomic pick: under N concurrent callers, exactly one transitions the
// head task to claimed; the others fall through to the next eligible
// task or ErrNoTaskAvailable.
func (m *MemoryStore) Claim(ctx context.Context, workerID string, workspaceIDs []string, now time.Time) (Task, error) {
	if workerID == "" {
		return Task{}, errors.New("task: worker_id required")
	}
	if len(workspaceIDs) == 0 {
		return Task{}, ErrNoTaskAvailable
	}
	allowed := make(map[string]struct{}, len(workspaceIDs))
	for _, w := range workspaceIDs {
		allowed[w] = struct{}{}
	}
	m.mu.Lock()
	candidates := make([]Task, 0)
	for _, t := range m.rows {
		if t.Status != StatusEnqueued {
			continue
		}
		if _, ok := allowed[t.WorkspaceID]; !ok {
			continue
		}
		candidates = append(candidates, t)
	}
	if len(candidates) == 0 {
		m.mu.Unlock()
		return Task{}, ErrNoTaskAvailable
	}
	sortByPriorityThenCreated(candidates)
	picked := candidates[0]
	picked.Status = StatusClaimed
	picked.ClaimedBy = workerID
	claimedAt := now
	picked.ClaimedAt = &claimedAt
	m.rows[picked.ID] = picked
	pub := m.publisher
	m.mu.Unlock()
	emitStatus(ctx, pub, picked)
	return picked, nil
}

// Close implements Store for MemoryStore.
func (m *MemoryStore) Close(ctx context.Context, id, outcome string, now time.Time, memberships []string) (Task, error) {
	if _, ok := validOutcomes[outcome]; !ok {
		return Task{}, fmt.Errorf("task: invalid outcome %q", outcome)
	}
	m.mu.Lock()
	t, ok := m.rows[id]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if !ValidTransition(t.Status, StatusClosed) {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusClosed)
	}
	t.Status = StatusClosed
	t.Outcome = outcome
	completed := now
	t.CompletedAt = &completed
	m.rows[id] = t
	pub := m.publisher
	m.mu.Unlock()
	emitStatus(ctx, pub, t)
	return t, nil
}

// Reclaim implements Store for MemoryStore.
func (m *MemoryStore) Reclaim(ctx context.Context, id, reason string, now time.Time, memberships []string) (Task, error) {
	m.mu.Lock()
	t, ok := m.rows[id]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if !ValidTransition(t.Status, StatusEnqueued) {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusEnqueued)
	}
	t.Status = StatusEnqueued
	t.ClaimedBy = ""
	t.ClaimedAt = nil
	t.ReclaimCount++
	m.rows[id] = t
	pub := m.publisher
	m.mu.Unlock()
	emitStatus(ctx, pub, t)
	return t, nil
}

// ListExpiring implements Store for MemoryStore.
func (m *MemoryStore) ListExpiring(ctx context.Context, now time.Time, multiplier float64, memberships []string) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0)
	for _, t := range m.rows {
		if t.Status != StatusClaimed && t.Status != StatusInFlight {
			continue
		}
		if !workspaceVisible(t.WorkspaceID, memberships) {
			continue
		}
		if t.ExpectedDuration <= 0 || t.ClaimedAt == nil {
			continue
		}
		budget := time.Duration(float64(t.ExpectedDuration) * multiplier)
		if now.Sub(*t.ClaimedAt) <= budget {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// sortByPriorityThenCreated orders tasks by priority rank (critical
// first) then created_at ASC so FIFO within bucket is preserved.
func sortByPriorityThenCreated(ts []Task) {
	sort.Slice(ts, func(i, j int) bool {
		ri, rj := PriorityRank(ts[i].Priority), PriorityRank(ts[j].Priority)
		if ri != rj {
			return ri < rj
		}
		return ts[i].CreatedAt.Before(ts[j].CreatedAt)
	})
}

// workspaceVisible returns true when workspaceID is in memberships (or
// memberships is nil = no scoping).
func workspaceVisible(workspaceID string, memberships []string) bool {
	if memberships == nil {
		return true
	}
	for _, m := range memberships {
		if m == workspaceID {
			return true
		}
	}
	return false
}

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

package task

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
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
//
// IncludeArchived defaults to false: archived rows fall out of the
// default query so the active queue stays uncluttered. Callers that
// need history (closed-pane "Load more", admin audit) opt in by
// setting it to true. sty_dc2998c5.
type ListOptions struct {
	Origin          string
	Status          string
	Priority        string
	ClaimedBy       string
	StoryID         string
	Kind            string
	IncludeArchived bool
	Limit           int
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

	// ClaimByID atomically transitions the task with the given id from
	// enqueued → claimed and stamps ClaimedBy/ClaimedAt. Returns
	// ErrNoTaskAvailable when the row is no longer enqueued (already
	// claimed by another worker, closed, etc.). Used by role-scoped
	// workers (e.g. the standalone reviewer service) that pick a
	// specific task via List + a role filter rather than the queue's
	// generic next-available semantic.
	// epic:v4-lifecycle-refactor sty_6077711d.
	ClaimByID(ctx context.Context, id, workerID string, now time.Time, memberships []string) (Task, error)

	// Close transitions a task to Status=closed with the given outcome.
	// Rejects invalid transitions via ErrInvalidTransition.
	Close(ctx context.Context, id, outcome string, now time.Time, memberships []string) (Task, error)

	// Reclaim transitions a claimed task back to Status=enqueued
	// (typically after watchdog expiry). Outcome=timeout is the
	// convention but not enforced at this layer. Increments
	// Task.ReclaimCount so a subsequent stale task_close from the
	// original claimer can be detected and rejected.
	Reclaim(ctx context.Context, id, reason string, now time.Time, memberships []string) (Task, error)

	// Archive flips a closed task to Status=archived. Idempotent:
	// already-archived rows return without mutation. Used by the
	// retention sweep (internal/task.Sweep). sty_dc2998c5.
	Archive(ctx context.Context, id string, now time.Time, memberships []string) (Task, error)

	// ListExpiring returns tasks whose Status is claimed or in_flight
	// AND (now - ClaimedAt) exceeds `threshold * ExpectedDuration`. Used
	// by the dispatcher watchdog. When ExpectedDuration is zero, the
	// row is skipped (no expiry budget to compute against).
	// Story_b4513c8c.
	ListExpiring(ctx context.Context, now time.Time, multiplier float64, memberships []string) ([]Task, error)

	// Save persists arbitrary mutations to a task row. Used by
	// migrations that need to rewrite fields outside the
	// Enqueue/Claim/Close lifecycle helpers (e.g. enqueued→published
	// status migration in sty_c1200f75). Validates enum fields and
	// transition legality via Task.Validate. Caller-supplied UpdatedAt
	// is implicit in `now`. Memberships-scoped.
	Save(ctx context.Context, t Task, now time.Time) error

	// Publish flips a task from StatusPlanned to StatusPublished —
	// the orchestrator's commit step in sty_c1200f75. Returns
	// ErrInvalidTransition if the task is not currently planned.
	// Memberships-scoped.
	Publish(ctx context.Context, id string, now time.Time, memberships []string) (Task, error)

	// SetPriorTaskID stamps PriorTaskID on the task with id=activeID.
	// Idempotent: returns the row unchanged if PriorTaskID is already
	// non-empty. Used exclusively by tools/migrate_prior_task_id to
	// backfill the linkage on chains that pre-date the auto-supersession
	// detection in client.TaskAdd (sty_9d046bc7). Not exposed via any
	// MCP/HTTP/CLI verb. Memberships-scoped.
	SetPriorTaskID(ctx context.Context, activeID, priorID string, now time.Time, memberships []string) (Task, error)

	// SetLinkage patches PriorTaskID and/or ParentTaskID on a
	// non-terminal task. Pointer args distinguish "leave alone" (nil)
	// from "patch to this value, including empty" (non-nil). Returns
	// ErrInvalidTransition when the row is closed or archived;
	// terminal rows are immutable. Idempotent when both pointers
	// already match the persisted values. Memberships-scoped.
	// sty_27516920.
	SetLinkage(ctx context.Context, id string, priorID, parentID *string, now time.Time, memberships []string) (Task, error)

	// DeleteByProjectID hard-removes every task whose ProjectID matches.
	// Returns the count removed. Memberships scoping is NOT applied —
	// the caller (project_delete hard-purge cascade) is responsible for
	// authorising the destructive op. Sty_d357b28d.
	DeleteByProjectID(ctx context.Context, projectID string) (int, error)

	// SetWorkspaceIDByProjectID stamps newWorkspaceID on every task whose
	// ProjectID matches. Returns the count touched. Used by the
	// project_move_workspace cascade. Sty_d357b28d.
	SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string, now time.Time) (int, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu        sync.Mutex
	rows      map[string]Task
	listeners []Listener
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Task)}
}

// AddListener registers l on the bus-subscriber slice (sty_c6d76a5b).
// Listeners fire on every status transition; consumers like
// internal/storystatus + internal/wshandler observe via this seam.
func (m *MemoryStore) AddListener(l Listener) {
	if l == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listeners = append(m.listeners, l)
}

// snapshotListeners returns a defensive copy of the listener slice
// for fan-out under-lock-free invocation.
func (m *MemoryStore) snapshotListeners() []Listener {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.listeners) == 0 {
		return nil
	}
	out := make([]Listener, len(m.listeners))
	copy(out, m.listeners)
	return out
}

// emit fans every status transition out to the registered listeners.
// Caller has already released the store lock so listener callbacks
// may re-enter the store safely. The pre-cutover hub publish path
// (sty_010a0543) is gone; surreallive picks up the row mutation
// directly from Surreal.
func (m *MemoryStore) emit(ctx context.Context, t Task) {
	fanoutListeners(ctx, m.snapshotListeners(), t)
}

// Enqueue implements Store for MemoryStore.
//
// Accepts t.Status ∈ {planned, published, enqueued (legacy default)}.
// Empty defaults to StatusEnqueued for back-compat; new callers should
// set t.Status=StatusPublished or use task_publish explicitly per
// sty_c1200f75. Set t.Status=StatusPlanned for the agent-local
// drafting state.
func (m *MemoryStore) Enqueue(ctx context.Context, t Task, now time.Time) (Task, error) {
	if t.Status == "" {
		t.Status = StatusEnqueued
	}
	if t.Priority == "" {
		t.Priority = PriorityMedium
	}
	if t.Iteration <= 0 {
		t.Iteration = 1
	}
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	switch t.Status {
	case StatusPlanned, StatusPublished, StatusEnqueued:
	default:
		return Task{}, fmt.Errorf("task: Enqueue accepts status ∈ {planned, published, enqueued}, got %q", t.Status)
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
	m.mu.Unlock()
	m.emit(ctx, t)
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
		// Default-filter archived rows unless the caller opts in or
		// explicitly asks for status=archived.
		if t.Status == StatusArchived && !opts.IncludeArchived && opts.Status != StatusArchived {
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
		if opts.StoryID != "" && t.StoryID != opts.StoryID {
			continue
		}
		if opts.Kind != "" && t.Kind != opts.Kind {
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
		if !isClaimable(t.Status) {
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
	m.mu.Unlock()
	m.emit(ctx, picked)
	return picked, nil
}

// ClaimByID implements Store for MemoryStore. Mutex-guarded so the
// concurrent-claim invariant matches the generic Claim path.
func (m *MemoryStore) ClaimByID(ctx context.Context, id, workerID string, now time.Time, memberships []string) (Task, error) {
	if workerID == "" {
		return Task{}, errors.New("task: worker_id required")
	}
	m.mu.Lock()
	t, ok := m.rows[id]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if !isClaimable(t.Status) {
		m.mu.Unlock()
		return Task{}, ErrNoTaskAvailable
	}
	t.Status = StatusClaimed
	t.ClaimedBy = workerID
	claimedAt := now
	t.ClaimedAt = &claimedAt
	m.rows[id] = t
	m.mu.Unlock()
	m.emit(ctx, t)
	return t, nil
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
	m.mu.Unlock()
	m.emit(ctx, t)
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
	m.mu.Unlock()
	m.emit(ctx, t)
	return t, nil
}

// Archive implements Store for MemoryStore.
func (m *MemoryStore) Archive(ctx context.Context, id string, now time.Time, memberships []string) (Task, error) {
	m.mu.Lock()
	t, ok := m.rows[id]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if t.Status == StatusArchived {
		m.mu.Unlock()
		return t, nil
	}
	if !ValidTransition(t.Status, StatusArchived) {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusArchived)
	}
	t.Status = StatusArchived
	m.rows[id] = t
	m.mu.Unlock()
	m.emit(ctx, t)
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

// Publish implements Store for MemoryStore.
func (m *MemoryStore) Publish(ctx context.Context, id string, now time.Time, memberships []string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.rows[id]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		return Task{}, ErrNotFound
	}
	if !ValidTransition(t.Status, StatusPublished) {
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusPublished)
	}
	t.Status = StatusPublished
	m.rows[id] = t
	go m.emit(context.Background(), t)
	return t, nil
}

// SetPriorTaskID implements Store for MemoryStore. Idempotent: when
// the active row already carries a non-empty PriorTaskID, returns the
// row unchanged.
func (m *MemoryStore) SetPriorTaskID(ctx context.Context, activeID, priorID string, now time.Time, memberships []string) (Task, error) {
	if activeID == "" {
		return Task{}, errors.New("task: active_id required")
	}
	if priorID == "" {
		return Task{}, errors.New("task: prior_id required")
	}
	m.mu.Lock()
	t, ok := m.rows[activeID]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if t.PriorTaskID != "" {
		m.mu.Unlock()
		return t, nil
	}
	t.PriorTaskID = priorID
	m.rows[activeID] = t
	m.mu.Unlock()
	return t, nil
}

// SetLinkage implements Store for MemoryStore. Rejects mutation on
// closed/archived rows with ErrInvalidTransition (terminal rows are
// immutable). Idempotent when both pointers match the persisted
// values. sty_27516920.
func (m *MemoryStore) SetLinkage(ctx context.Context, id string, priorID, parentID *string, now time.Time, memberships []string) (Task, error) {
	if id == "" {
		return Task{}, errors.New("task: id required")
	}
	if priorID == nil && parentID == nil {
		return Task{}, errors.New("task: SetLinkage requires at least one of prior_task_id or parent_task_id")
	}
	m.mu.Lock()
	t, ok := m.rows[id]
	if !ok || !workspaceVisible(t.WorkspaceID, memberships) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if t.Status == StatusClosed || t.Status == StatusArchived {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("%w: linkage patch rejected on terminal status %s", ErrInvalidTransition, t.Status)
	}
	changed := false
	if priorID != nil && t.PriorTaskID != *priorID {
		t.PriorTaskID = *priorID
		changed = true
	}
	if parentID != nil && t.ParentTaskID != *parentID {
		t.ParentTaskID = *parentID
		changed = true
	}
	if !changed {
		m.mu.Unlock()
		return t, nil
	}
	m.rows[id] = t
	m.mu.Unlock()
	return t, nil
}

// Save implements Store for MemoryStore.
func (m *MemoryStore) Save(ctx context.Context, t Task, now time.Time) error {
	if err := t.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[t.ID]; !ok {
		return ErrNotFound
	}
	m.rows[t.ID] = t
	return nil
}

// isClaimable reports whether a task at this status can transition to
// StatusClaimed via the Claim/ClaimByID path. published is the
// post-c1200f75 default; enqueued is the legacy pre-migration state.
func isClaimable(status string) bool {
	return status == StatusPublished || status == StatusEnqueued
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

// DeleteByProjectID implements Store for MemoryStore. Sty_d357b28d.
func (m *MemoryStore) DeleteByProjectID(ctx context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.rows {
		if t.ProjectID != projectID {
			continue
		}
		delete(m.rows, id)
		n++
	}
	return n, nil
}

// SetWorkspaceIDByProjectID implements Store for MemoryStore. Sty_d357b28d.
func (m *MemoryStore) SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.rows {
		if t.ProjectID != projectID {
			continue
		}
		if t.WorkspaceID == newWorkspaceID {
			continue
		}
		t.WorkspaceID = newWorkspaceID
		m.rows[id] = t
		n++
	}
	return n, nil
}

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

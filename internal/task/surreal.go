package task

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is a SurrealDB-backed Store. Atomic Claim uses a single
// UPDATE query with ORDER BY + LIMIT 1 so two workers cannot double-claim
// the same row.
type SurrealStore struct {
	db        *surrealdb.DB
	listenMu  sync.Mutex
	listeners []Listener
}

// AddListener registers l on the listener slice (sty_c6d76a5b).
// Consumers like internal/storystatus + internal/wshandler observe via
// this seam. Post-hub-cutover (sty_010a0543) this is the only fan-out
// path.
func (s *SurrealStore) AddListener(l Listener) {
	if l == nil {
		return
	}
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	s.listeners = append(s.listeners, l)
}

// snapshotListeners returns a defensive copy of the listener slice
// for fan-out under-lock-free invocation.
func (s *SurrealStore) snapshotListeners() []Listener {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	if len(s.listeners) == 0 {
		return nil
	}
	out := make([]Listener, len(s.listeners))
	copy(out, s.listeners)
	return out
}

// emit fans every status transition out to registered listeners.
func (s *SurrealStore) emit(ctx context.Context, t Task) {
	fanoutListeners(ctx, s.snapshotListeners(), t)
}

// NewSurrealStore wraps db as a Store and defines the `tasks` table +
// the three indexes the dispatcher + worker heartbeat queries rely on.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	ctx := context.Background()
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS tasks SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS tasks_dispatch ON TABLE tasks FIELDS workspace_id, status, priority, created_at", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS tasks_worker ON TABLE tasks FIELDS workspace_id, claimed_by", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS tasks_archival ON TABLE tasks FIELDS workspace_id, completed_at", nil)
	// sty_509a46fa: contract_instance_id and payload are dead post-
	// sty_c6d76a5b's "tasks are thin" mandate. UNSET clears any lingering
	// values on legacy rows; idempotent on already-cleared rows.
	_, _ = surrealdb.Query[any](ctx, db, "UPDATE tasks UNSET contract_instance_id", nil)
	_, _ = surrealdb.Query[any](ctx, db, "UPDATE tasks UNSET payload", nil)
	return s
}

const selectCols = "meta::id(id) AS id, workspace_id, project_id, story_id, kind, iteration, agent_id, prior_task_id, parent_task_id, action, description, origin, trigger, status, priority, claimed_by, claimed_at, completed_at, outcome, ledger_root_id, expected_duration, reclaim_count, created_at"

// Enqueue implements Store for SurrealStore.
//
// Accepts t.Status ∈ {planned, published, enqueued (legacy default)}.
// Empty defaults to StatusEnqueued for back-compat. sty_c1200f75.
func (s *SurrealStore) Enqueue(ctx context.Context, t Task, now time.Time) (Task, error) {
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
	if t.ID == "" {
		t.ID = NewID()
	}
	t.CreatedAt = now
	if err := s.write(ctx, t); err != nil {
		return Task{}, err
	}
	s.emit(ctx, t)
	return t, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string, memberships []string) (Task, error) {
	if memberships != nil && len(memberships) == 0 {
		return Task{}, ErrNotFound
	}
	conds := []string{"id = $rid"}
	vars := map[string]any{"rid": surrealmodels.NewRecordID("tasks", id)}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM tasks WHERE %s LIMIT 1", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]Task](ctx, s.db, sql, vars)
	if err != nil {
		return Task{}, fmt.Errorf("task: select: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Task{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// List implements Store for SurrealStore.
func (s *SurrealStore) List(ctx context.Context, opts ListOptions, memberships []string) ([]Task, error) {
	if memberships != nil && len(memberships) == 0 {
		return []Task{}, nil
	}
	conds := []string{}
	vars := map[string]any{}
	if opts.Origin != "" {
		conds = append(conds, "origin = $origin")
		vars["origin"] = opts.Origin
	}
	if opts.Status != "" {
		conds = append(conds, "status = $status")
		vars["status"] = opts.Status
	} else if !opts.IncludeArchived {
		conds = append(conds, "status != $archived")
		vars["archived"] = StatusArchived
	}
	if opts.Priority != "" {
		conds = append(conds, "priority = $priority")
		vars["priority"] = opts.Priority
	}
	if opts.ClaimedBy != "" {
		conds = append(conds, "claimed_by = $claimed_by")
		vars["claimed_by"] = opts.ClaimedBy
	}
	if opts.StoryID != "" {
		conds = append(conds, "story_id = $story_id")
		vars["story_id"] = opts.StoryID
	}
	if opts.Kind != "" {
		conds = append(conds, "kind = $kind")
		vars["kind"] = opts.Kind
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	limit := ""
	if opts.Limit > 0 {
		limit = fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	sql := fmt.Sprintf("SELECT %s FROM tasks %s ORDER BY created_at ASC%s", selectCols, where, limit)
	results, err := surrealdb.Query[[]Task](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("task: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Task{}, nil
	}
	return (*results)[0].Result, nil
}

// Claim implements Store for SurrealStore with an atomic UPDATE ...
// RETURN AFTER. The query selects the highest-priority oldest-queued
// task visible to workspaceIDs, transitions it to claimed, and returns
// the mutated row. Under concurrency SurrealDB serialises the UPDATE so
// exactly one caller wins per candidate row.
func (s *SurrealStore) Claim(ctx context.Context, workerID string, workspaceIDs []string, now time.Time) (Task, error) {
	if workerID == "" {
		return Task{}, fmt.Errorf("task: worker_id required")
	}
	if len(workspaceIDs) == 0 {
		return Task{}, ErrNoTaskAvailable
	}
	// SurrealDB's UPDATE ... WHERE ... does not support ORDER BY +
	// LIMIT on the WHERE clause directly; we pick via a SELECT then
	// UPDATE with the resolved id. Ordering is created_at ASC (FIFO)
	// only; SurrealDB's ORDER BY does not accept the priority enum
	// idiom natively, so priority-aware dispatch is a follow-up
	// optimisation (feature-order:9.3 dispatcher will layer that in
	// via a priority_rank column or bucketed queries). MemoryStore
	// already enforces priority order for unit-test parity.
	// SurrealDB v3's parser needs the ORDER BY field to appear in the
	// SELECT list — including created_at here keeps it happy.
	selectSQL := "SELECT meta::id(id) AS id, workspace_id, created_at FROM tasks WHERE status IN $statuses AND workspace_id IN $memberships ORDER BY created_at LIMIT 1"
	type head struct {
		ID          string    `json:"id"`
		WorkspaceID string    `json:"workspace_id"`
		CreatedAt   time.Time `json:"created_at"`
	}
	selectRes, err := surrealdb.Query[[]head](ctx, s.db, selectSQL, map[string]any{
		"statuses":    []string{StatusPublished, StatusEnqueued},
		"memberships": workspaceIDs,
	})
	if err != nil {
		return Task{}, fmt.Errorf("task: claim select: %w", err)
	}
	if selectRes == nil || len(*selectRes) == 0 || len((*selectRes)[0].Result) == 0 {
		return Task{}, ErrNoTaskAvailable
	}
	id := (*selectRes)[0].Result[0].ID

	// Conditional UPDATE: only transitions when the row is still
	// enqueued. Concurrent callers racing on the same id get an empty
	// Result from the losing UPDATE; they retry the SELECT.
	updateSQL := fmt.Sprintf(
		"UPDATE $rid SET status = $new, claimed_by = $by, claimed_at = $at WHERE status IN $oldset RETURN %s",
		selectCols,
	)
	updateRes, err := surrealdb.Query[[]Task](ctx, s.db, updateSQL, map[string]any{
		"rid":    surrealmodels.NewRecordID("tasks", id),
		"new":    StatusClaimed,
		"oldset": []string{StatusPublished, StatusEnqueued},
		"by":     workerID,
		"at":     now,
	})
	if err != nil {
		return Task{}, fmt.Errorf("task: claim update: %w", err)
	}
	if updateRes == nil || len(*updateRes) == 0 || len((*updateRes)[0].Result) == 0 {
		// Lost the race; caller retries.
		return Task{}, ErrNoTaskAvailable
	}
	claimed := (*updateRes)[0].Result[0]
	s.emit(ctx, claimed)
	return claimed, nil
}

// ClaimByID implements Store for SurrealStore. Mirrors Claim's
// conditional UPDATE: the query only transitions when the row is
// still enqueued, so two callers racing on the same id end with one
// winner and one ErrNoTaskAvailable.
func (s *SurrealStore) ClaimByID(ctx context.Context, id, workerID string, now time.Time, memberships []string) (Task, error) {
	if workerID == "" {
		return Task{}, fmt.Errorf("task: worker_id required")
	}
	if memberships != nil && len(memberships) == 0 {
		return Task{}, ErrNotFound
	}
	conds := []string{"status IN $oldset"}
	vars := map[string]any{
		"rid":    surrealmodels.NewRecordID("tasks", id),
		"new":    StatusClaimed,
		"oldset": []string{StatusPublished, StatusEnqueued},
		"by":     workerID,
		"at":     now,
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	updateSQL := fmt.Sprintf(
		"UPDATE $rid SET status = $new, claimed_by = $by, claimed_at = $at WHERE %s RETURN %s",
		strings.Join(conds, " AND "), selectCols,
	)
	updateRes, err := surrealdb.Query[[]Task](ctx, s.db, updateSQL, vars)
	if err != nil {
		return Task{}, fmt.Errorf("task: claim_by_id: %w", err)
	}
	if updateRes == nil || len(*updateRes) == 0 || len((*updateRes)[0].Result) == 0 {
		return Task{}, ErrNoTaskAvailable
	}
	claimed := (*updateRes)[0].Result[0]
	s.emit(ctx, claimed)
	return claimed, nil
}

// Close implements Store for SurrealStore.
func (s *SurrealStore) Close(ctx context.Context, id, outcome string, now time.Time, memberships []string) (Task, error) {
	if _, ok := validOutcomes[outcome]; !ok {
		return Task{}, fmt.Errorf("task: invalid outcome %q", outcome)
	}
	t, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return Task{}, err
	}
	if !ValidTransition(t.Status, StatusClosed) {
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusClosed)
	}
	t.Status = StatusClosed
	t.Outcome = outcome
	completed := now
	t.CompletedAt = &completed
	if err := s.write(ctx, t); err != nil {
		return Task{}, err
	}
	s.emit(ctx, t)
	return t, nil
}

// Reclaim implements Store for SurrealStore.
func (s *SurrealStore) Reclaim(ctx context.Context, id, reason string, now time.Time, memberships []string) (Task, error) {
	t, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return Task{}, err
	}
	if !ValidTransition(t.Status, StatusEnqueued) {
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusEnqueued)
	}
	t.Status = StatusEnqueued
	t.ClaimedBy = ""
	t.ClaimedAt = nil
	t.ReclaimCount++
	if err := s.write(ctx, t); err != nil {
		return Task{}, err
	}
	s.emit(ctx, t)
	return t, nil
}

// Archive implements Store for SurrealStore.
func (s *SurrealStore) Archive(ctx context.Context, id string, now time.Time, memberships []string) (Task, error) {
	t, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return Task{}, err
	}
	if t.Status == StatusArchived {
		return t, nil
	}
	if !ValidTransition(t.Status, StatusArchived) {
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusArchived)
	}
	t.Status = StatusArchived
	if err := s.write(ctx, t); err != nil {
		return Task{}, err
	}
	s.emit(ctx, t)
	return t, nil
}

// ListExpiring implements Store for SurrealStore.
func (s *SurrealStore) ListExpiring(ctx context.Context, now time.Time, multiplier float64, memberships []string) ([]Task, error) {
	// Fetch all claimed/in_flight tasks in the caller's workspaces and
	// filter in-process. Simpler than encoding the expected_duration *
	// multiplier comparison in SurrealDB SQL; the in-flight set is
	// bounded by worker concurrency per workspace, so the linear scan
	// cost is acceptable.
	if memberships != nil && len(memberships) == 0 {
		return []Task{}, nil
	}
	conds := []string{"status IN [$claimed, $in_flight]"}
	vars := map[string]any{"claimed": StatusClaimed, "in_flight": StatusInFlight}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM tasks WHERE %s", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]Task](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("task: list expiring: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Task{}, nil
	}
	out := make([]Task, 0)
	for _, t := range (*results)[0].Result {
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

// Publish implements Store for SurrealStore.
func (s *SurrealStore) Publish(ctx context.Context, id string, now time.Time, memberships []string) (Task, error) {
	t, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return Task{}, err
	}
	if !ValidTransition(t.Status, StatusPublished) {
		return Task{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.Status, StatusPublished)
	}
	t.Status = StatusPublished
	if err := s.write(ctx, t); err != nil {
		return Task{}, err
	}
	s.emit(ctx, t)
	return t, nil
}

// SetPriorTaskID implements Store for SurrealStore. Idempotent: when
// the active row already carries a non-empty PriorTaskID, returns the
// row unchanged. Backfill primitive for tools/migrate_prior_task_id
// (sty_9d046bc7); not exposed via any MCP/HTTP/CLI verb.
func (s *SurrealStore) SetPriorTaskID(ctx context.Context, activeID, priorID string, now time.Time, memberships []string) (Task, error) {
	t, err := s.GetByID(ctx, activeID, memberships)
	if err != nil {
		return Task{}, err
	}
	if priorID == "" {
		return Task{}, fmt.Errorf("task: prior_id required")
	}
	if t.PriorTaskID != "" {
		return t, nil
	}
	t.PriorTaskID = priorID
	if werr := s.write(ctx, t); werr != nil {
		return Task{}, werr
	}
	return t, nil
}

// SetLinkage implements Store for SurrealStore. Rejects mutation on
// closed/archived rows with ErrInvalidTransition. Idempotent when
// both pointers match the persisted values. sty_27516920.
func (s *SurrealStore) SetLinkage(ctx context.Context, id string, priorID, parentID *string, now time.Time, memberships []string) (Task, error) {
	if id == "" {
		return Task{}, fmt.Errorf("task: id required")
	}
	if priorID == nil && parentID == nil {
		return Task{}, fmt.Errorf("task: SetLinkage requires at least one of prior_task_id or parent_task_id")
	}
	t, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return Task{}, err
	}
	if t.Status == StatusClosed || t.Status == StatusArchived {
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
		return t, nil
	}
	if werr := s.write(ctx, t); werr != nil {
		return Task{}, werr
	}
	return t, nil
}

// Save implements Store for SurrealStore — generic upsert used by
// migrations that mutate fields outside the lifecycle helpers.
func (s *SurrealStore) Save(ctx context.Context, t Task, now time.Time) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return s.write(ctx, t)
}

func (s *SurrealStore) write(ctx context.Context, t Task) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("tasks", t.ID),
		"doc": t,
	}
	if _, err := surrealdb.Query[[]Task](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("task: upsert: %w", err)
	}
	return nil
}

// DeleteByProjectID implements Store for SurrealStore. Sty_d357b28d.
func (s *SurrealStore) DeleteByProjectID(ctx context.Context, projectID string) (int, error) {
	n, err := countTable(ctx, s.db, "tasks", "project_id = $project", map[string]any{"project": projectID})
	if err != nil {
		return 0, fmt.Errorf("task: count by project: %w", err)
	}
	if _, err := surrealdb.Query[any](ctx, s.db, "DELETE tasks WHERE project_id = $project", map[string]any{"project": projectID}); err != nil {
		return 0, fmt.Errorf("task: delete by project: %w", err)
	}
	return n, nil
}

// SetWorkspaceIDByProjectID implements Store for SurrealStore. Sty_d357b28d.
func (s *SurrealStore) SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string, now time.Time) (int, error) {
	sql := "UPDATE tasks SET workspace_id = $ws WHERE project_id = $project AND workspace_id != $ws RETURN AFTER"
	vars := map[string]any{"ws": newWorkspaceID, "project": projectID}
	results, err := surrealdb.Query[[]Task](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("task: set workspace_id by project: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// countTable runs SELECT count() FROM <table> WHERE <where>. Used by the
// hard-delete cascade paths so the verb can surface a row count without
// asking the caller to List + len first. Sty_d357b28d.
func countTable(ctx context.Context, db *surrealdb.DB, table, where string, vars map[string]any) (int, error) {
	sql := fmt.Sprintf("SELECT count() FROM %s WHERE %s GROUP ALL", table, where)
	results, err := surrealdb.Query[[]map[string]any](ctx, db, sql, vars)
	if err != nil {
		return 0, err
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return 0, nil
	}
	v, ok := (*results)[0].Result[0]["count"]
	if !ok {
		return 0, nil
	}
	switch c := v.(type) {
	case int:
		return c, nil
	case int64:
		return int(c), nil
	case float64:
		return int(c), nil
	case uint64:
		return int(c), nil
	}
	return 0, nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

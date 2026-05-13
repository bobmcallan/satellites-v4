package tasklog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is the SurrealDB-backed task_log store. Table +
// replay-index defined at boot. Append-only — the production path
// issues no UPDATE / DELETE; only the boot DEFINE statements touch
// table metadata.
type SurrealStore struct {
	db *surrealdb.DB

	// pub/sub fan-out runs in-memory because satellites-server is
	// single-instance today; multi-instance fan-out is out of scope
	// (see gap-log §5). Mirrors MemoryStore.subs.
	mu     sync.Mutex
	subs   map[string][]*subFn
	wsByID map[string]string
}

const selectCols = "meta::id(id) AS id, task_id, workspace_id, project_id, seq, ts, kind, payload, created_at"

// NewSurrealStore wraps db as a Store and defines the `task_logs` table
// + replay/workspace indexes the producer + SSE handler depend on.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{
		db:     db,
		subs:   make(map[string][]*subFn),
		wsByID: make(map[string]string),
	}
	ctx := context.Background()
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS task_logs SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS task_logs_replay ON TABLE task_logs FIELDS task_id, seq", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS task_logs_workspace ON TABLE task_logs FIELDS workspace_id, task_id", nil)
	return s
}

// Append implements Store for SurrealStore.
func (s *SurrealStore) Append(ctx context.Context, e Entry, now time.Time) (Entry, error) {
	if err := e.Validate(); err != nil {
		return Entry{}, err
	}
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.TS.IsZero() {
		e.TS = now
	}
	e.CreatedAt = now

	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("task_logs", e.ID),
		"doc": e,
	}
	if _, err := surrealdb.Query[[]Entry](ctx, s.db, sql, vars); err != nil {
		return Entry{}, fmt.Errorf("task_log: upsert: %w", err)
	}

	s.mu.Lock()
	s.wsByID[e.TaskID] = e.WorkspaceID
	live := append([]*subFn(nil), s.subs[e.TaskID]...)
	s.mu.Unlock()
	for _, sub := range live {
		if sub == nil || sub.cancelled {
			continue
		}
		select {
		case sub.ch <- e:
		default:
		}
	}
	return e, nil
}

// List implements Store for SurrealStore.
func (s *SurrealStore) List(ctx context.Context, opts ListOptions) ([]Entry, error) {
	if opts.TaskID == "" {
		return nil, fmt.Errorf("task_log: task_id required")
	}
	if opts.Memberships != nil && len(opts.Memberships) == 0 {
		return []Entry{}, nil
	}
	conds := []string{"task_id = $task_id", "seq >= $from_seq"}
	vars := map[string]any{
		"task_id":  opts.TaskID,
		"from_seq": opts.FromSeq,
	}
	if opts.Memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = opts.Memberships
	}
	where := "WHERE "
	for i, c := range conds {
		if i > 0 {
			where += " AND "
		}
		where += c
	}
	limit := ""
	if opts.Limit > 0 {
		limit = fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	sql := fmt.Sprintf("SELECT %s FROM task_logs %s ORDER BY seq ASC%s", selectCols, where, limit)
	results, err := surrealdb.Query[[]Entry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("task_log: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Entry{}, nil
	}
	return (*results)[0].Result, nil
}

// Subscribe implements Store for SurrealStore. Same in-memory fan-out
// model as MemoryStore — single-instance assumption documented in
// gap-log §5.
func (s *SurrealStore) Subscribe(ctx context.Context, taskID string, memberships []string) (<-chan Entry, func(), error) {
	if taskID == "" {
		return nil, nil, fmt.Errorf("task_log: task_id required")
	}
	// Membership check: if we've seen any row for this task, resolve
	// its workspace_id from cache; else fall through to a List() probe.
	s.mu.Lock()
	ws, cached := s.wsByID[taskID]
	s.mu.Unlock()
	if !cached {
		rows, err := s.List(ctx, ListOptions{TaskID: taskID, Limit: 1, Memberships: nil})
		if err == nil && len(rows) > 0 {
			ws = rows[0].WorkspaceID
			s.mu.Lock()
			s.wsByID[taskID] = ws
			s.mu.Unlock()
		}
	}
	if memberships != nil && !workspaceVisible(ws, memberships) {
		return nil, nil, ErrNotFound
	}
	sub := &subFn{ch: make(chan Entry, 256)}
	s.mu.Lock()
	s.subs[taskID] = append(s.subs[taskID], sub)
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		sub.cancelled = true
		slots := s.subs[taskID]
		kept := slots[:0]
		for _, x := range slots {
			if !x.cancelled {
				kept = append(kept, x)
			}
		}
		s.subs[taskID] = kept
		s.mu.Unlock()
	}
	return sub.ch, cancel, nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

package story

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// SurrealStore is a SurrealDB-backed Store. UpdateStatus reads → validates
// → writes the new status → appends the ledger row → compensates the
// status write on ledger failure. This is not a true distributed
// transaction but satisfies the v4-baseline invariant: "failed ledger
// emission must not leave the status advanced" (pr_20440c77).
type SurrealStore struct {
	db     *surrealdb.DB
	ledger ledger.Store
}

// NewSurrealStore wraps db as a Store. Defines the `stories` table
// schemaless and panics if led is nil.
func NewSurrealStore(db *surrealdb.DB, led ledger.Store) *SurrealStore {
	if led == nil {
		panic("story.SurrealStore requires a non-nil ledger.Store")
	}
	s := &SurrealStore{db: db, ledger: led}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS stories SCHEMALESS", nil)
	return s
}

// selectCols preserves the string id (see project/surreal.go note).
const selectCols = "meta::id(id) AS id, project_id, title, description, acceptance_criteria, status, priority, category, tags, created_by, created_at, updated_at"

func (s *SurrealStore) Create(ctx context.Context, st Story, now time.Time) (Story, error) {
	if st.Status == "" {
		st.Status = StatusBacklog
	}
	if !IsKnownStatus(st.Status) {
		return Story{}, fmt.Errorf("story: unknown status %q", st.Status)
	}
	st.ID = NewID()
	st.CreatedAt = now
	st.UpdatedAt = now
	if st.Tags == nil {
		st.Tags = []string{}
	}
	if err := s.write(ctx, st); err != nil {
		return Story{}, err
	}
	return st, nil
}

func (s *SurrealStore) GetByID(ctx context.Context, id string) (Story, error) {
	sql := fmt.Sprintf("SELECT %s FROM stories WHERE id = $rid LIMIT 1", selectCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("stories", id)}
	results, err := surrealdb.Query[[]Story](ctx, s.db, sql, vars)
	if err != nil {
		return Story{}, fmt.Errorf("story: get: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Story{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

func (s *SurrealStore) List(ctx context.Context, projectID string, opts ListOptions) ([]Story, error) {
	opts = opts.normalised()
	conds := []string{"project_id = $project"}
	vars := map[string]any{"project": projectID, "lim": opts.Limit}
	if opts.Status != "" {
		conds = append(conds, "status = $status")
		vars["status"] = opts.Status
	}
	if opts.Priority != "" {
		conds = append(conds, "priority = $priority")
		vars["priority"] = opts.Priority
	}
	if opts.Tag != "" {
		conds = append(conds, "$tag IN tags")
		vars["tag"] = opts.Tag
	}
	where := ""
	for i, c := range conds {
		if i == 0 {
			where = "WHERE " + c
		} else {
			where += " AND " + c
		}
	}
	sql := fmt.Sprintf("SELECT %s FROM stories %s ORDER BY created_at DESC LIMIT $lim", selectCols, where)
	results, err := surrealdb.Query[[]Story](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("story: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Story{}, nil
	}
	return (*results)[0].Result, nil
}

func (s *SurrealStore) UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time) (Story, error) {
	current, err := s.GetByID(ctx, id)
	if err != nil {
		return Story{}, err
	}
	if !ValidTransition(current.Status, newStatus) {
		return Story{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, newStatus)
	}
	prior := current.Status
	current.Status = newStatus
	current.UpdatedAt = now
	if err := s.write(ctx, current); err != nil {
		return Story{}, err
	}
	payload := transitionPayload{
		StoryID: id,
		From:    prior,
		To:      newStatus,
		Actor:   actor,
	}
	content, _ := json.Marshal(payload)
	if _, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		ProjectID: current.ProjectID,
		Type:      LedgerEntryType,
		Content:   string(content),
		Actor:     actor,
	}, now); err != nil {
		// Compensating write — revert the status. Any error on the revert
		// is wrapped alongside the original failure so the caller sees
		// both. See pr_20440c77.
		current.Status = prior
		current.UpdatedAt = now
		if writeErr := s.write(ctx, current); writeErr != nil {
			return Story{}, fmt.Errorf("story: ledger emission failed (%v) AND revert failed (%w)", err, writeErr)
		}
		return Story{}, fmt.Errorf("story: ledger emission failed (status reverted): %w", err)
	}
	return current, nil
}

func (s *SurrealStore) write(ctx context.Context, st Story) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("stories", st.ID),
		"doc": st,
	}
	if _, err := surrealdb.Query[[]Story](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("story: upsert: %w", err)
	}
	return nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

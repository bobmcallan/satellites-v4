package contract

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/hubemit"
	"github.com/bobmcallan/satellites/internal/story"
)

// SurrealStore is a SurrealDB-backed Store. docs resolves the
// ContractID FK; stories cascades workspace + project on Create.
type SurrealStore struct {
	db        *surrealdb.DB
	docs      document.Store
	stories   story.Store
	publisher hubemit.Publisher
}

// SetPublisher installs the hub emit sink for subsequent mutations.
func (s *SurrealStore) SetPublisher(p hubemit.Publisher) { s.publisher = p }

// NewSurrealStore wraps db as a Store. Defines the
// `contract_instances` table schemaless and the two indexes required
// for story-ordered reads and reviewer rollups. Panics if docs or
// stories is nil — Create cannot proceed without FK resolution or
// parent cascade.
func NewSurrealStore(db *surrealdb.DB, docs document.Store, stories story.Store) *SurrealStore {
	if docs == nil {
		panic("contract.SurrealStore requires a non-nil document.Store")
	}
	if stories == nil {
		panic("contract.SurrealStore requires a non-nil story.Store")
	}
	s := &SurrealStore{db: db, docs: docs, stories: stories}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS contract_instances SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS contract_instances_story_seq ON TABLE contract_instances FIELDS workspace_id, story_id, sequence", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS contract_instances_reviewer ON TABLE contract_instances FIELDS workspace_id, contract_id", nil)
	return s
}

// selectCols preserves the string form of id (see document/surreal.go).
const selectCols = "meta::id(id) AS id, workspace_id, project_id, story_id, contract_id, contract_name, phase, sequence, status, claimed_via_grant_id, claimed_at, plan_ledger_id, close_ledger_id, required_for_close, created_at, updated_at"

// Create implements Store for SurrealStore.
func (s *SurrealStore) Create(ctx context.Context, ci ContractInstance, now time.Time) (ContractInstance, error) {
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
	parent, err := s.stories.GetByID(ctx, ci.StoryID, nil)
	if err != nil {
		return ContractInstance{}, ErrMissingStory
	}
	ci.WorkspaceID = parent.WorkspaceID
	ci.ProjectID = parent.ProjectID
	if err := validateContractBinding(ctx, s.docs, ci.ContractID, ci.WorkspaceID); err != nil {
		return ContractInstance{}, err
	}
	ci.ID = NewID()
	ci.CreatedAt = now
	ci.UpdatedAt = now
	if err := s.write(ctx, ci); err != nil {
		return ContractInstance{}, err
	}
	return ci, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string, memberships []string) (ContractInstance, error) {
	if memberships != nil && len(memberships) == 0 {
		return ContractInstance{}, ErrNotFound
	}
	conds := []string{"id = $rid"}
	vars := map[string]any{"rid": surrealmodels.NewRecordID("contract_instances", id)}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM contract_instances WHERE %s LIMIT 1", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]ContractInstance](ctx, s.db, sql, vars)
	if err != nil {
		return ContractInstance{}, fmt.Errorf("contract: select by id: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return ContractInstance{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// List implements Store for SurrealStore.
func (s *SurrealStore) List(ctx context.Context, storyID string, memberships []string) ([]ContractInstance, error) {
	if storyID == "" {
		return nil, fmt.Errorf("contract: story_id is required")
	}
	if memberships != nil && len(memberships) == 0 {
		return []ContractInstance{}, nil
	}
	conds := []string{"story_id = $story"}
	vars := map[string]any{"story": storyID}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM contract_instances WHERE %s ORDER BY sequence ASC", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]ContractInstance](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("contract: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []ContractInstance{}, nil
	}
	return (*results)[0].Result, nil
}

// UpdateStatus implements Store for SurrealStore.
func (s *SurrealStore) UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time, memberships []string) (ContractInstance, error) {
	if !IsKnownStatus(newStatus) {
		return ContractInstance{}, fmt.Errorf("contract: unknown status %q", newStatus)
	}
	ci, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return ContractInstance{}, err
	}
	if !ValidTransition(ci.Status, newStatus) {
		return ContractInstance{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, ci.Status, newStatus)
	}
	ci.Status = newStatus
	ci.UpdatedAt = now
	if err := s.write(ctx, ci); err != nil {
		return ContractInstance{}, err
	}
	emitStatus(ctx, s.publisher, ci)
	return ci, nil
}

// Claim implements Store for SurrealStore.
func (s *SurrealStore) Claim(ctx context.Context, id, grantID string, now time.Time, memberships []string) (ContractInstance, error) {
	ci, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return ContractInstance{}, err
	}
	if !ValidTransition(ci.Status, StatusClaimed) {
		return ContractInstance{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, ci.Status, StatusClaimed)
	}
	ci.Status = StatusClaimed
	ci.ClaimedViaGrantID = grantID
	ci.ClaimedAt = now
	ci.UpdatedAt = now
	if err := s.write(ctx, ci); err != nil {
		return ContractInstance{}, err
	}
	emitStatus(ctx, s.publisher, ci)
	return ci, nil
}

// RebindGrant implements Store for SurrealStore.
func (s *SurrealStore) RebindGrant(ctx context.Context, id, grantID string, now time.Time, memberships []string) (ContractInstance, error) {
	ci, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return ContractInstance{}, err
	}
	ci.ClaimedViaGrantID = grantID
	ci.UpdatedAt = now
	if err := s.write(ctx, ci); err != nil {
		return ContractInstance{}, err
	}
	return ci, nil
}

// ClearClaim implements Store for SurrealStore.
func (s *SurrealStore) ClearClaim(ctx context.Context, id string, now time.Time, memberships []string) (ContractInstance, error) {
	ci, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return ContractInstance{}, err
	}
	ci.Status = StatusReady
	ci.ClaimedViaGrantID = ""
	ci.ClaimedAt = time.Time{}
	ci.PlanLedgerID = ""
	ci.CloseLedgerID = ""
	ci.UpdatedAt = now
	if err := s.write(ctx, ci); err != nil {
		return ContractInstance{}, err
	}
	emitStatus(ctx, s.publisher, ci)
	return ci, nil
}

// DropLegacySessionColumn UNSETs the `claimed_by_session_id` column from
// every contract_instances row. Idempotent — SurrealDB's UNSET is a
// no-op on rows that already lack the column. Called by the boot-time
// migration in cmd/satellites/main.go to complete the transition away
// from the session-id-bound claim binding (story_4608a82c).
func (s *SurrealStore) DropLegacySessionColumn(ctx context.Context) error {
	sql := "UPDATE contract_instances UNSET claimed_by_session_id RETURN NONE"
	if _, err := surrealdb.Query[any](ctx, s.db, sql, nil); err != nil {
		return fmt.Errorf("contract: drop legacy session column: %w", err)
	}
	return nil
}

// BackfillClaimedViaGrant stamps claimed_via_grant_id on rows whose
// legacy claimed_by_session_id matches a key in sessionToGrant. Only
// rows with empty claimed_via_grant_id are touched — never overwrite an
// already-stamped grant binding. Returns (stamped, missed) where missed
// counts rows with legacy session ids that do not resolve to any grant
// in the lookup map. Idempotent: a second call after DropLegacySessionColumn
// finds zero legacy rows. Called by cmd/satellites/main.go
// (story_4608a82c).
func (s *SurrealStore) BackfillClaimedViaGrant(ctx context.Context, sessionToGrant map[string]string, now time.Time) (stamped int, missed int, err error) {
	sql := "SELECT meta::id(id) AS id, claimed_by_session_id, claimed_via_grant_id FROM contract_instances WHERE claimed_by_session_id != NONE AND claimed_by_session_id != ''"
	type row struct {
		ID                 string `json:"id"`
		ClaimedBySessionID string `json:"claimed_by_session_id"`
		ClaimedViaGrantID  string `json:"claimed_via_grant_id"`
	}
	results, qerr := surrealdb.Query[[]row](ctx, s.db, sql, nil)
	if qerr != nil {
		return 0, 0, fmt.Errorf("contract: backfill scan: %w", qerr)
	}
	if results == nil || len(*results) == 0 {
		return 0, 0, nil
	}
	for _, r := range (*results)[0].Result {
		if r.ClaimedViaGrantID != "" {
			continue
		}
		grantID, ok := sessionToGrant[r.ClaimedBySessionID]
		if !ok || grantID == "" {
			missed++
			continue
		}
		upd := "UPDATE $rid SET claimed_via_grant_id = $grant, updated_at = $now RETURN NONE"
		vars := map[string]any{
			"rid":   surrealmodels.NewRecordID("contract_instances", r.ID),
			"grant": grantID,
			"now":   now,
		}
		if _, uerr := surrealdb.Query[any](ctx, s.db, upd, vars); uerr != nil {
			return stamped, missed, fmt.Errorf("contract: stamp grant on %s: %w", r.ID, uerr)
		}
		stamped++
	}
	return stamped, missed, nil
}

// UpdateLedgerRefs implements Store for SurrealStore.
func (s *SurrealStore) UpdateLedgerRefs(ctx context.Context, id string, plan, closeRef *string, actor string, now time.Time, memberships []string) (ContractInstance, error) {
	ci, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return ContractInstance{}, err
	}
	if plan != nil {
		ci.PlanLedgerID = *plan
	}
	if closeRef != nil {
		ci.CloseLedgerID = *closeRef
	}
	ci.UpdatedAt = now
	if err := s.write(ctx, ci); err != nil {
		return ContractInstance{}, err
	}
	return ci, nil
}

// BackfillWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	sql := "UPDATE contract_instances SET workspace_id = $ws, updated_at = $now WHERE project_id = $project AND (workspace_id IS NONE OR workspace_id = '') RETURN AFTER"
	vars := map[string]any{"ws": workspaceID, "project": projectID, "now": now}
	results, err := surrealdb.Query[[]ContractInstance](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("contract: backfill workspace_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

func (s *SurrealStore) write(ctx context.Context, ci ContractInstance) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("contract_instances", ci.ID),
		"doc": ci,
	}
	if _, err := surrealdb.Query[[]ContractInstance](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("contract: upsert: %w", err)
	}
	return nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

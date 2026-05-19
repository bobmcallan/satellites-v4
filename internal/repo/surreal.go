package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealStore is a SurrealDB-backed Store. The DDL bootstraps two
// indexes: one for the workspace+project+status filter that drives
// list/dispatch reads, and one for the (workspace, git_remote)
// uniqueness probe that the MCP add verb consults before Create.
type SurrealStore struct {
	db *surrealdb.DB
}

// NewSurrealStore wraps db as a Store. Defines the `repos` table
// schemaless and installs the two indexes named in the AC. Index
// uniqueness is enforced at the application layer (Create rejects
// duplicates) — DDL UNIQUE is intentionally absent to match the
// project-wide convention seen in story / contract / document.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS repos SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS repos_workspace_project_status ON TABLE repos FIELDS workspace_id, project_id, status", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS repos_workspace_remote ON TABLE repos FIELDS workspace_id, git_remote", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS commits SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS commits_repo_committed_at ON TABLE commits FIELDS repo_id, committed_at", nil)
	return s
}

// selectCols preserves the string id (see story / project surreal
// stores for the same idiom).
const selectCols = "meta::id(id) AS id, workspace_id, project_id, git_remote, default_branch, head_sha, last_indexed_at, index_version, symbol_count, file_count, status, webhook_secret, created_at, updated_at"

// Create implements Store for SurrealStore. Performs the
// one-per-project pre-check via a count query so the rejection path
// matches the in-memory store byte-for-byte from the caller's view.
func (s *SurrealStore) Create(ctx context.Context, r Repo, now time.Time) (Repo, error) {
	if r.ProjectID == "" {
		return Repo{}, fmt.Errorf("repo: project_id is required")
	}
	if r.GitRemote == "" {
		return Repo{}, fmt.Errorf("repo: git_remote is required")
	}
	if r.Status == "" {
		r.Status = StatusActive
	}
	if err := validateStatus(r.Status); err != nil {
		return Repo{}, err
	}
	existing, err := s.findByProject(ctx, r.ProjectID)
	if err != nil {
		return Repo{}, err
	}
	if existing != nil {
		return Repo{}, fmt.Errorf("%w: project=%s existing=%s", ErrAlreadyExists, r.ProjectID, existing.ID)
	}
	r.ID = NewID()
	r.CreatedAt = now
	r.UpdatedAt = now
	if err := s.write(ctx, r); err != nil {
		return Repo{}, err
	}
	return r, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string, memberships []string) (Repo, error) {
	if memberships != nil && len(memberships) == 0 {
		return Repo{}, ErrNotFound
	}
	where := "id = $rid"
	vars := map[string]any{"rid": surrealmodels.NewRecordID("repos", id)}
	if memberships != nil {
		where += " AND workspace_id IN $memberships"
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM repos WHERE %s LIMIT 1", selectCols, where)
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return Repo{}, fmt.Errorf("repo: get: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Repo{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// List implements Store for SurrealStore.
func (s *SurrealStore) List(ctx context.Context, projectID string, memberships []string) ([]Repo, error) {
	if memberships != nil && len(memberships) == 0 {
		return []Repo{}, nil
	}
	where := "project_id = $project"
	vars := map[string]any{"project": projectID}
	if memberships != nil {
		where += " AND workspace_id IN $memberships"
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM repos WHERE %s ORDER BY created_at ASC", selectCols, where)
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("repo: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Repo{}, nil
	}
	return (*results)[0].Result, nil
}

// GetByRemote implements Store for SurrealStore.
func (s *SurrealStore) GetByRemote(ctx context.Context, workspaceID, gitRemote string) (Repo, error) {
	sql := fmt.Sprintf("SELECT %s FROM repos WHERE workspace_id = $ws AND git_remote = $remote LIMIT 1", selectCols)
	vars := map[string]any{"ws": workspaceID, "remote": gitRemote}
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return Repo{}, fmt.Errorf("repo: get by remote: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Repo{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// UpdateIndexState implements Store for SurrealStore. Reads the row
// (memberships=nil — internal write path), applies the field updates +
// index_version bump, and writes the new state.
func (s *SurrealStore) UpdateIndexState(ctx context.Context, id, headSHA string, lastIndexedAt time.Time, symbolCount, fileCount int) (Repo, error) {
	current, err := s.GetByID(ctx, id, nil)
	if err != nil {
		return Repo{}, err
	}
	current.HeadSHA = headSHA
	current.LastIndexedAt = lastIndexedAt
	current.SymbolCount = symbolCount
	current.FileCount = fileCount
	current.IndexVersion = current.IndexVersion + 1
	current.UpdatedAt = lastIndexedAt
	if err := s.write(ctx, current); err != nil {
		return Repo{}, err
	}
	return current, nil
}

// Archive implements Store for SurrealStore. Idempotent re-archive is
// a no-op return of the current row.
func (s *SurrealStore) Archive(ctx context.Context, id string) (Repo, error) {
	current, err := s.GetByID(ctx, id, nil)
	if err != nil {
		return Repo{}, err
	}
	if current.Status == StatusArchived {
		return current, nil
	}
	current.Status = StatusArchived
	current.UpdatedAt = time.Now().UTC()
	if err := s.write(ctx, current); err != nil {
		return Repo{}, err
	}
	return current, nil
}

// findByProject probes the (workspace, project) slice to enforce
// one-repo-per-project on Create. Returns nil + nil when no row
// matches.
func (s *SurrealStore) findByProject(ctx context.Context, projectID string) (*Repo, error) {
	sql := fmt.Sprintf("SELECT %s FROM repos WHERE project_id = $project LIMIT 1", selectCols)
	vars := map[string]any{"project": projectID}
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("repo: find by project: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return nil, nil
	}
	r := (*results)[0].Result[0]
	return &r, nil
}

func (s *SurrealStore) write(ctx context.Context, r Repo) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("repos", r.ID),
		"doc": r,
	}
	if _, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("repo: upsert: %w", err)
	}
	return nil
}

// ListActive implements Store for SurrealStore.
func (s *SurrealStore) ListActive(ctx context.Context) ([]Repo, error) {
	sql := fmt.Sprintf("SELECT %s FROM repos WHERE status = $status ORDER BY created_at ASC", selectCols)
	vars := map[string]any{"status": StatusActive}
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("repo: list active: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Repo{}, nil
	}
	return (*results)[0].Result, nil
}

// LookupByRemote implements Store for SurrealStore.
func (s *SurrealStore) LookupByRemote(ctx context.Context, gitRemote string) ([]Repo, error) {
	sql := fmt.Sprintf("SELECT %s FROM repos WHERE git_remote = $remote", selectCols)
	vars := map[string]any{"remote": gitRemote}
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("repo: lookup by remote: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Repo{}, nil
	}
	return (*results)[0].Result, nil
}

// commitSelectCols pins the column projection for the commits table.
const commitSelectCols = "repo_id, sha, subject, author, url, committed_at, parent_sha, story_ids"

// commitRowID returns the canonical record id for the commits table.
// Idempotent on retry: re-writing the same (repoID, sha) replaces in
// place rather than duplicating.
func commitRowID(repoID, sha string) string {
	return repoID + "_" + sha
}

// UpsertCommit implements Store for SurrealStore.
func (s *SurrealStore) UpsertCommit(ctx context.Context, c Commit) (Commit, error) {
	if c.RepoID == "" || c.SHA == "" {
		return Commit{}, fmt.Errorf("repo: commit requires repo_id and sha")
	}
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("commits", commitRowID(c.RepoID, c.SHA)),
		"doc": c,
	}
	if _, err := surrealdb.Query[[]Commit](ctx, s.db, sql, vars); err != nil {
		return Commit{}, fmt.Errorf("repo: upsert commit: %w", err)
	}
	return c, nil
}

// GetCommit implements Store for SurrealStore.
func (s *SurrealStore) GetCommit(ctx context.Context, repoID, sha string, memberships []string) (Commit, error) {
	if _, err := s.GetByID(ctx, repoID, memberships); err != nil {
		return Commit{}, err
	}
	sql := fmt.Sprintf("SELECT %s FROM commits WHERE id = $rid LIMIT 1", commitSelectCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("commits", commitRowID(repoID, sha))}
	results, err := surrealdb.Query[[]Commit](ctx, s.db, sql, vars)
	if err != nil {
		return Commit{}, fmt.Errorf("repo: get commit: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Commit{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// ListCommits implements Store for SurrealStore.
func (s *SurrealStore) ListCommits(ctx context.Context, repoID, branch string, limit int, memberships []string) ([]Commit, error) {
	if _, err := s.GetByID(ctx, repoID, memberships); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultCommitListLimit
	}
	sql := fmt.Sprintf("SELECT %s FROM commits WHERE repo_id = $repo ORDER BY committed_at DESC LIMIT $limit", commitSelectCols)
	vars := map[string]any{"repo": repoID, "limit": limit}
	results, err := surrealdb.Query[[]Commit](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("repo: list commits: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []Commit{}, nil
	}
	return (*results)[0].Result, nil
}

// Diff implements Store for SurrealStore. Walks the persisted parent
// chain via repeat GetCommit calls.
func (s *SurrealStore) Diff(ctx context.Context, repoID, fromRef, toRef string, memberships []string) (Diff, error) {
	if _, err := s.GetByID(ctx, repoID, memberships); err != nil {
		return Diff{}, err
	}
	out := Diff{
		RepoID:           repoID,
		FromRef:          fromRef,
		ToRef:            toRef,
		Commits:          []Commit{},
		Unified:          "",
		SymbolChanges:    []SymbolChange{},
		DiffSource:       DiffSourceUnavailable,
		DiffSourceReason: "satellites does not clone repos and webhook payloads do not carry file diffs; follow-up story will integrate the GitHub Compare API",
	}
	cur := toRef
	for steps := 0; cur != "" && cur != fromRef && steps < 1000; steps++ {
		c, err := s.GetCommit(ctx, repoID, cur, memberships)
		if err != nil {
			break
		}
		out.Commits = append(out.Commits, c)
		cur = c.ParentSHA
	}
	return out, nil
}

// DeleteByProjectID implements Store for SurrealStore. Sty_d357b28d.
func (s *SurrealStore) DeleteByProjectID(ctx context.Context, projectID string) (int, error) {
	type idRow struct {
		ID string `json:"id"`
	}
	idResults, err := surrealdb.Query[[]idRow](ctx, s.db, "SELECT meta::id(id) AS id FROM repos WHERE project_id = $project", map[string]any{"project": projectID})
	if err != nil {
		return 0, fmt.Errorf("repo: list ids by project: %w", err)
	}
	ids := []string{}
	if idResults != nil && len(*idResults) > 0 {
		for _, r := range (*idResults)[0].Result {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	for _, id := range ids {
		if _, err := surrealdb.Query[any](ctx, s.db, "DELETE commits WHERE repo_id = $repo", map[string]any{"repo": id}); err != nil {
			return 0, fmt.Errorf("repo: delete commits: %w", err)
		}
	}
	if _, err := surrealdb.Query[any](ctx, s.db, "DELETE repos WHERE project_id = $project", map[string]any{"project": projectID}); err != nil {
		return 0, fmt.Errorf("repo: delete by project: %w", err)
	}
	return len(ids), nil
}

// SetWorkspaceIDByProjectID implements Store for SurrealStore. Sty_d357b28d.
func (s *SurrealStore) SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string, now time.Time) (int, error) {
	sql := "UPDATE repos SET workspace_id = $ws, updated_at = $now WHERE project_id = $project AND workspace_id != $ws RETURN AFTER"
	vars := map[string]any{"ws": newWorkspaceID, "project": projectID, "now": now}
	results, err := surrealdb.Query[[]Repo](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("repo: set workspace_id by project: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

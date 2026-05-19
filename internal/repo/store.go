package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a repo lookup misses or the caller's
// memberships exclude the row's workspace.
var ErrNotFound = errors.New("repo: not found")

// ErrAlreadyExists is returned by Create when a repo already exists on
// the project. The "one repo per project" invariant ignores status —
// archiving is not a free slot for a fresh remote per architecture §7
// ("each project has exactly zero or one repo record").
var ErrAlreadyExists = errors.New("repo: already exists for project")

// ErrInvalidStatus is returned when a write supplies a status outside
// the documented enum. Keeping the validator at the store layer means
// neither MCP handlers nor task workers can poke a row into an unknown
// state.
var ErrInvalidStatus = errors.New("repo: invalid status")

// Store is the persistence surface for repo rows. The verb set is the
// minimum needed by slice 12.1; richer query verbs (search, file, …)
// proxy to jcodemunch from the MCP layer in slice 12.2 and never go
// through this interface.
//
// memberships follows the project-wide convention: nil = no scoping
// (boot/backfill paths), empty = deny-all, non-empty = workspace_id IN
// memberships.
type Store interface {
	Create(ctx context.Context, r Repo, now time.Time) (Repo, error)
	GetByID(ctx context.Context, id string, memberships []string) (Repo, error)
	List(ctx context.Context, projectID string, memberships []string) ([]Repo, error)
	GetByRemote(ctx context.Context, workspaceID, gitRemote string) (Repo, error)
	UpdateIndexState(ctx context.Context, id, headSHA string, lastIndexedAt time.Time, symbolCount, fileCount int) (Repo, error)
	Archive(ctx context.Context, id string) (Repo, error)
	// ListActive returns every active repo across all workspaces. Used
	// by the stale-check cron worker (system identity, no membership
	// scope). Story_21d22880.
	ListActive(ctx context.Context) ([]Repo, error)
	// LookupByRemote returns every repo (across all workspaces) whose
	// git_remote matches. Used by the push-webhook receiver to find the
	// tracked repo before signature verification. Story_21d22880.
	LookupByRemote(ctx context.Context, gitRemote string) ([]Repo, error)

	// UpsertCommit writes a per-commit row keyed by (RepoID, SHA).
	// Idempotent on retry: re-writing the same (RepoID, SHA) replaces
	// the existing row in place. Used by the webhook commit-receiver.
	// Story_c2a2f073.
	UpsertCommit(ctx context.Context, c Commit) (Commit, error)

	// GetCommit returns the persisted commit row for (repoID, sha) or
	// ErrNotFound. Used by the Diff parent-walk and the MCP layer.
	GetCommit(ctx context.Context, repoID, sha string, memberships []string) (Commit, error)

	// ListCommits returns commits for (repoID) ordered DESC by
	// CommittedAt. Branch is currently advisory — the persisted commits
	// table does not carry a branch column, so non-empty branch is a
	// no-op filter. Limit caps the result; <=0 uses 50.
	ListCommits(ctx context.Context, repoID, branch string, limit int, memberships []string) ([]Commit, error)

	// Diff walks the persisted parent-chain from toRef back to fromRef
	// (or until the chain ends) and returns the Diff shape. Unified +
	// SymbolChanges remain empty in v1 — see Diff.DiffSource.
	Diff(ctx context.Context, repoID, fromRef, toRef string, memberships []string) (Diff, error)

	// DeleteByProjectID hard-removes every repo row whose ProjectID
	// matches plus the commit rows attached to them. Returns the count
	// of repo rows removed. Sty_d357b28d.
	DeleteByProjectID(ctx context.Context, projectID string) (int, error)

	// SetWorkspaceIDByProjectID stamps newWorkspaceID on every repo row
	// whose ProjectID matches. Returns the count touched. Sty_d357b28d.
	SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string, now time.Time) (int, error)
}

// validateStatus rejects writes that supply a status outside the
// documented enum. Empty defaults to active in Create per the §7
// "Status: enum(active, archived)" definition.
func validateStatus(s string) error {
	if !IsKnownStatus(s) {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, s)
	}
	return nil
}

// inMemberships is the shared workspace-scope predicate.
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

// MemoryStore is a concurrency-safe in-process Store used by unit tests
// and the local-iteration substrate per pr_local_iteration. The
// one-per-project invariant and status-enum validator live in this
// shared type so the surreal impl can defer to the same checks.
type MemoryStore struct {
	mu      sync.Mutex
	rows    map[string]Repo
	commits map[string]Commit // key = repoID + "|" + sha
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Repo), commits: make(map[string]Commit)}
}

// commitKey is the in-memory composite-key for the commits map.
func commitKey(repoID, sha string) string {
	return repoID + "|" + sha
}

// Create implements Store for MemoryStore.
func (m *MemoryStore) Create(ctx context.Context, r Repo, now time.Time) (Repo, error) {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.rows {
		if existing.ProjectID == r.ProjectID {
			return Repo{}, fmt.Errorf("%w: project=%s existing=%s", ErrAlreadyExists, r.ProjectID, existing.ID)
		}
	}
	r.ID = NewID()
	r.CreatedAt = now
	r.UpdatedAt = now
	m.rows[r.ID] = r
	return r, nil
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return Repo{}, ErrNotFound
	}
	if !inMemberships(r.WorkspaceID, memberships) {
		return Repo{}, ErrNotFound
	}
	return r, nil
}

// List implements Store for MemoryStore. Rows are ordered by CreatedAt
// ascending so callers see the project's first-registered remote first.
func (m *MemoryStore) List(ctx context.Context, projectID string, memberships []string) ([]Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Repo, 0)
	for _, r := range m.rows {
		if r.ProjectID != projectID {
			continue
		}
		if !inMemberships(r.WorkspaceID, memberships) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// GetByRemote implements Store for MemoryStore. The lookup is
// workspace-scoped; the same git_remote may appear in two workspaces
// without colliding (tenancy isolation per pr_0779e5af).
func (m *MemoryStore) GetByRemote(ctx context.Context, workspaceID, gitRemote string) (Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.WorkspaceID == workspaceID && r.GitRemote == gitRemote {
			return r, nil
		}
	}
	return Repo{}, ErrNotFound
}

// UpdateIndexState implements Store for MemoryStore. Bumps
// IndexVersion by one so subscribers can detect a fresh index without
// holding a snapshot per docs/architecture.md §7 ("index_version").
func (m *MemoryStore) UpdateIndexState(ctx context.Context, id, headSHA string, lastIndexedAt time.Time, symbolCount, fileCount int) (Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return Repo{}, ErrNotFound
	}
	r.HeadSHA = headSHA
	r.LastIndexedAt = lastIndexedAt
	r.SymbolCount = symbolCount
	r.FileCount = fileCount
	r.IndexVersion = r.IndexVersion + 1
	r.UpdatedAt = lastIndexedAt
	m.rows[id] = r
	return r, nil
}

// Archive implements Store for MemoryStore. Idempotent: archiving an
// already-archived row is a no-op return of the current state, not an
// error.
func (m *MemoryStore) Archive(ctx context.Context, id string) (Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return Repo{}, ErrNotFound
	}
	if r.Status == StatusArchived {
		return r, nil
	}
	r.Status = StatusArchived
	r.UpdatedAt = time.Now().UTC()
	m.rows[id] = r
	return r, nil
}

// ListActive implements Store for MemoryStore.
func (m *MemoryStore) ListActive(ctx context.Context) ([]Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Repo, 0)
	for _, r := range m.rows {
		if r.Status == StatusActive {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// LookupByRemote implements Store for MemoryStore.
func (m *MemoryStore) LookupByRemote(ctx context.Context, gitRemote string) ([]Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Repo, 0)
	for _, r := range m.rows {
		if r.GitRemote == gitRemote {
			out = append(out, r)
		}
	}
	return out, nil
}

// UpsertCommit implements Store for MemoryStore.
func (m *MemoryStore) UpsertCommit(ctx context.Context, c Commit) (Commit, error) {
	if c.RepoID == "" || c.SHA == "" {
		return Commit{}, fmt.Errorf("repo: commit requires repo_id and sha")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commits[commitKey(c.RepoID, c.SHA)] = c
	return c, nil
}

// GetCommit implements Store for MemoryStore.
func (m *MemoryStore) GetCommit(ctx context.Context, repoID, sha string, memberships []string) (Commit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[repoID]
	if !ok || !inMemberships(r.WorkspaceID, memberships) {
		return Commit{}, ErrNotFound
	}
	c, ok := m.commits[commitKey(repoID, sha)]
	if !ok {
		return Commit{}, ErrNotFound
	}
	return c, nil
}

const defaultCommitListLimit = 50

// ListCommits implements Store for MemoryStore. Branch is currently a
// no-op filter — see Store.ListCommits docs.
func (m *MemoryStore) ListCommits(ctx context.Context, repoID, branch string, limit int, memberships []string) ([]Commit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[repoID]
	if !ok || !inMemberships(r.WorkspaceID, memberships) {
		return nil, ErrNotFound
	}
	out := make([]Commit, 0)
	for _, c := range m.commits {
		if c.RepoID != repoID {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CommittedAt.After(out[j].CommittedAt) })
	if limit <= 0 {
		limit = defaultCommitListLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Diff implements Store for MemoryStore. Walks the parent chain from
// toRef back to fromRef using persisted ParentSHA. Unified +
// SymbolChanges remain empty in v1; DiffSource carries the constraint
// marker.
func (m *MemoryStore) Diff(ctx context.Context, repoID, fromRef, toRef string, memberships []string) (Diff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[repoID]
	if !ok || !inMemberships(r.WorkspaceID, memberships) {
		return Diff{}, ErrNotFound
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
		c, ok := m.commits[commitKey(repoID, cur)]
		if !ok {
			break
		}
		out.Commits = append(out.Commits, c)
		cur = c.ParentSHA
	}
	return out, nil
}

// DeleteByProjectID implements Store for MemoryStore. Removes every repo
// row whose ProjectID matches plus all commit rows for those repos.
// Sty_d357b28d.
func (m *MemoryStore) DeleteByProjectID(ctx context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	repoIDs := make(map[string]struct{})
	for id, r := range m.rows {
		if r.ProjectID != projectID {
			continue
		}
		repoIDs[id] = struct{}{}
		delete(m.rows, id)
	}
	for key := range m.commits {
		repoID := key
		if i := strings.IndexByte(key, '|'); i >= 0 {
			repoID = key[:i]
		}
		if _, ok := repoIDs[repoID]; ok {
			delete(m.commits, key)
		}
	}
	return len(repoIDs), nil
}

// SetWorkspaceIDByProjectID implements Store for MemoryStore. Sty_d357b28d.
func (m *MemoryStore) SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if r.ProjectID != projectID {
			continue
		}
		if r.WorkspaceID == newWorkspaceID {
			continue
		}
		r.WorkspaceID = newWorkspaceID
		r.UpdatedAt = now
		m.rows[id] = r
		n++
	}
	return n, nil
}

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

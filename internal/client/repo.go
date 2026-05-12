package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
)

// ErrRepoStoreNotConfigured is returned when a repo verb is invoked
// against a Client whose Deps.Repos is nil. The wire layer maps this
// to the per-verb "<verb> unavailable" envelopes the MCP handler
// previously produced.
var ErrRepoStoreNotConfigured = errors.New("repo store not configured")

// ErrGitRemoteRequired is returned by RepoAdd when the input supplies
// no git_remote. Mirrors the RequireString("git_remote") envelope.
var ErrGitRemoteRequired = errors.New("git_remote required")

// ErrGitRemoteInvalid is returned by RepoAdd when the supplied remote
// fails project.CanonicaliseGitRemote. Wire envelope: "git_remote_invalid".
var ErrGitRemoteInvalid = errors.New("git_remote_invalid")

// ErrRepoIDRequired is returned by the read + proxy verbs when the
// input supplies no repo_id. Mirrors the RequireString("repo_id")
// envelope.
var ErrRepoIDRequired = errors.New("repo_id required")

// ErrRepoQueryRequired is returned by RepoSearch / RepoSearchText
// when the input supplies no query string.
var ErrRepoQueryRequired = errors.New("query required")

// ErrRepoSymbolIDRequired is returned by RepoGetSymbolSource when the
// input supplies no symbol_id.
var ErrRepoSymbolIDRequired = errors.New("symbol_id required")

// ErrRepoPathRequired is returned by RepoGetFile / RepoGetOutline
// when the input supplies no path.
var ErrRepoPathRequired = errors.New("path required")

// RepoAddInput captures the repo_add request shape. RawRemote is the
// caller-supplied git remote (any of ssh, https, .git suffix); the
// typed method canonicalises before persisting. RequestedProjectID is
// the inbound `project_id` argument; ScopedProjectID is the URL-scoped
// hint the wire layer enforces. Memberships is pre-resolved.
type RepoAddInput struct {
	RawRemote          string
	DefaultBranch      string
	RequestedProjectID string
	ScopedProjectID    string
	Memberships        []string
	Now                time.Time
}

// RepoAddOutput mirrors the JSON shape the handler previously
// emitted: repo_id + deduplicated flag + canonicalised git_remote +
// default_branch. The wire layer marshals this verbatim.
type RepoAddOutput struct {
	RepoID        string `json:"repo_id"`
	Deduplicated  bool   `json:"deduplicated"`
	GitRemote     string `json:"git_remote"`
	DefaultBranch string `json:"default_branch"`
}

// RepoAdd canonicalises the inbound remote, dedups against
// (workspace, git_remote), creates the row when fresh, and writes a
// kind:repo-added audit row. Mirrors mcpserver.Server.handleRepoAdd
// minus the wire-format wrap.
func (c *Client) RepoAdd(ctx context.Context, caller Caller, in RepoAddInput) (RepoAddOutput, error) {
	if c.deps.Repos == nil {
		return RepoAddOutput{}, ErrRepoStoreNotConfigured
	}
	if in.RawRemote == "" {
		return RepoAddOutput{}, ErrGitRemoteRequired
	}
	gitRemote, err := project.CanonicaliseGitRemote(in.RawRemote)
	if err != nil || gitRemote == "" {
		return RepoAddOutput{}, ErrGitRemoteInvalid
	}
	defaultBranch := in.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	projectID, err := c.ResolveProjectID(ctx, in.RequestedProjectID, in.ScopedProjectID, caller, in.Memberships)
	if err != nil {
		return RepoAddOutput{}, err
	}
	wsID := c.ResolveProjectWorkspaceID(ctx, projectID)
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if existing, gerr := c.deps.Repos.GetByRemote(ctx, wsID, gitRemote); gerr == nil {
		return RepoAddOutput{
			RepoID:        existing.ID,
			Deduplicated:  true,
			GitRemote:     existing.GitRemote,
			DefaultBranch: existing.DefaultBranch,
		}, nil
	} else if !errors.Is(gerr, repo.ErrNotFound) {
		return RepoAddOutput{}, fmt.Errorf("repo_add: dedup probe: %w", gerr)
	}
	created, err := c.deps.Repos.Create(ctx, repo.Repo{
		WorkspaceID:   wsID,
		ProjectID:     projectID,
		GitRemote:     gitRemote,
		DefaultBranch: defaultBranch,
		Status:        repo.StatusActive,
	}, now)
	if err != nil {
		return RepoAddOutput{}, fmt.Errorf("repo_add: %w", err)
	}
	c.appendRepoAuditRow(ctx, created, "kind:repo-added", caller.UserID, map[string]any{
		"git_remote":     created.GitRemote,
		"default_branch": created.DefaultBranch,
	}, now)
	return RepoAddOutput{
		RepoID:        created.ID,
		Deduplicated:  false,
		GitRemote:     created.GitRemote,
		DefaultBranch: created.DefaultBranch,
	}, nil
}

// RepoGetInput captures the repo_get request shape. Memberships is
// pre-resolved by the wire layer for workspace scoping.
type RepoGetInput struct {
	RepoID      string
	Memberships []string
}

// RepoGet returns a workspace-scoped repo row by id. Mirrors
// mcpserver.Server.handleRepoGet.
func (c *Client) RepoGet(ctx context.Context, caller Caller, in RepoGetInput) (repo.Repo, error) {
	if c.deps.Repos == nil {
		return repo.Repo{}, ErrRepoStoreNotConfigured
	}
	if in.RepoID == "" {
		return repo.Repo{}, ErrRepoIDRequired
	}
	return c.deps.Repos.GetByID(ctx, in.RepoID, in.Memberships)
}

// RepoListInput captures the repo_list request shape. StatusFilter is
// "active" / "archived" / "all"; empty defaults to "active" to mirror
// the pre-extraction handler.
type RepoListInput struct {
	RequestedProjectID string
	ScopedProjectID    string
	StatusFilter       string
	Memberships        []string
}

// RepoListOutput mirrors the JSON shape the handler previously
// emitted: project_id, status filter, repos slice.
type RepoListOutput struct {
	ProjectID string      `json:"project_id"`
	Status    string      `json:"status"`
	Repos     []repo.Repo `json:"repos"`
}

// RepoList lists repos for the caller's project, optionally filtered
// by status. Mirrors mcpserver.Server.handleRepoList.
func (c *Client) RepoList(ctx context.Context, caller Caller, in RepoListInput) (RepoListOutput, error) {
	if c.deps.Repos == nil {
		return RepoListOutput{}, ErrRepoStoreNotConfigured
	}
	projectID, err := c.ResolveProjectID(ctx, in.RequestedProjectID, in.ScopedProjectID, caller, in.Memberships)
	if err != nil {
		return RepoListOutput{}, err
	}
	statusFilter := in.StatusFilter
	if statusFilter == "" {
		statusFilter = repo.StatusActive
	}
	rows, err := c.deps.Repos.List(ctx, projectID, in.Memberships)
	if err != nil {
		return RepoListOutput{}, err
	}
	if statusFilter != "all" {
		filtered := make([]repo.Repo, 0, len(rows))
		for _, r := range rows {
			if r.Status == statusFilter {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	return RepoListOutput{
		ProjectID: projectID,
		Status:    statusFilter,
		Repos:     rows,
	}, nil
}

// RepoProxyInput captures the shared request shape for the five
// indexer-proxy verbs: repo_search / repo_search_text /
// repo_get_symbol_source / repo_get_file / repo_get_outline. Only the
// fields the underlying verb consumes are read; unused fields are
// zero-valued.
type RepoProxyInput struct {
	RepoID      string
	Query       string
	Kind        string
	Language    string
	FilePattern string
	SymbolID    string
	Path        string
	Memberships []string
	Now         time.Time
}

// RepoSearch proxies repo_search to the indexer.
// Returns the raw JSON payload the indexer produced; the wire layer
// wraps it via mcpgo.NewToolResultText.
func (c *Client) RepoSearch(ctx context.Context, caller Caller, in RepoProxyInput) (json.RawMessage, error) {
	r, err := c.resolveRepoForProxy(ctx, in.RepoID, in.Memberships)
	if err != nil {
		return nil, err
	}
	if in.Query == "" {
		return nil, ErrRepoQueryRequired
	}
	now := nowOrTime(in.Now)
	c.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action":   "search",
		"query":    in.Query,
		"kind":     in.Kind,
		"language": in.Language,
	}, now, "action:search")
	return c.deps.Indexer.SearchSymbols(ctx, r.GitRemote, in.Query, in.Kind, in.Language)
}

// RepoSearchText proxies repo_search_text to the indexer.
func (c *Client) RepoSearchText(ctx context.Context, caller Caller, in RepoProxyInput) (json.RawMessage, error) {
	r, err := c.resolveRepoForProxy(ctx, in.RepoID, in.Memberships)
	if err != nil {
		return nil, err
	}
	if in.Query == "" {
		return nil, ErrRepoQueryRequired
	}
	now := nowOrTime(in.Now)
	c.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action":       "search_text",
		"query":        in.Query,
		"file_pattern": in.FilePattern,
	}, now, "action:search_text")
	return c.deps.Indexer.SearchText(ctx, r.GitRemote, in.Query, in.FilePattern)
}

// RepoGetSymbolSource proxies repo_get_symbol_source to the indexer.
func (c *Client) RepoGetSymbolSource(ctx context.Context, caller Caller, in RepoProxyInput) (json.RawMessage, error) {
	r, err := c.resolveRepoForProxy(ctx, in.RepoID, in.Memberships)
	if err != nil {
		return nil, err
	}
	if in.SymbolID == "" {
		return nil, ErrRepoSymbolIDRequired
	}
	now := nowOrTime(in.Now)
	c.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action":    "get_symbol_source",
		"symbol_id": in.SymbolID,
	}, now, "action:get_symbol_source")
	return c.deps.Indexer.GetSymbolSource(ctx, r.GitRemote, in.SymbolID)
}

// RepoGetFile proxies repo_get_file to the indexer.
func (c *Client) RepoGetFile(ctx context.Context, caller Caller, in RepoProxyInput) (json.RawMessage, error) {
	r, err := c.resolveRepoForProxy(ctx, in.RepoID, in.Memberships)
	if err != nil {
		return nil, err
	}
	if in.Path == "" {
		return nil, ErrRepoPathRequired
	}
	now := nowOrTime(in.Now)
	c.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action": "get_file",
		"path":   in.Path,
	}, now, "action:get_file")
	return c.deps.Indexer.GetFileContent(ctx, r.GitRemote, in.Path)
}

// RepoGetOutline proxies repo_get_outline to the indexer.
func (c *Client) RepoGetOutline(ctx context.Context, caller Caller, in RepoProxyInput) (json.RawMessage, error) {
	r, err := c.resolveRepoForProxy(ctx, in.RepoID, in.Memberships)
	if err != nil {
		return nil, err
	}
	if in.Path == "" {
		return nil, ErrRepoPathRequired
	}
	now := nowOrTime(in.Now)
	c.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action": "get_outline",
		"path":   in.Path,
	}, now, "action:get_outline")
	return c.deps.Indexer.GetFileOutline(ctx, r.GitRemote, in.Path)
}

// resolveRepoForProxy is the shared boilerplate for the five proxy
// verbs: validate inputs, scope by memberships, return the row.
// Migrated from mcpserver.Server.resolveRepoForProxy.
func (c *Client) resolveRepoForProxy(ctx context.Context, repoID string, memberships []string) (repo.Repo, error) {
	if c.deps.Repos == nil {
		return repo.Repo{}, ErrRepoStoreNotConfigured
	}
	if repoID == "" {
		return repo.Repo{}, ErrRepoIDRequired
	}
	return c.deps.Repos.GetByID(ctx, repoID, memberships)
}

// appendRepoAuditRow writes a repo audit ledger row. Extra tags are
// appended after the canonical {kind, repo_id} pair so callers can
// add action sub-tags without recomputing the base set. Migrated
// from mcpserver.Server.appendRepoAuditRow.
func (c *Client) appendRepoAuditRow(ctx context.Context, r repo.Repo, kind, actor string, payload map[string]any, now time.Time, extraTags ...string) {
	if c.deps.Ledger == nil {
		return
	}
	tags := make([]string, 0, 2+len(extraTags))
	tags = append(tags, kind, "repo_id:"+r.ID)
	tags = append(tags, extraTags...)
	body, _ := json.Marshal(payload)
	_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		Type:        ledger.TypeDecision,
		Tags:        tags,
		Content:     fmt.Sprintf("%s repo=%s", kind, r.ID),
		Structured:  body,
		CreatedBy:   actor,
	}, now)
}

// nowOrTime returns the supplied time when non-zero, else time.Now().UTC().
// The proxy verbs share this default so test fixtures can pin the
// timestamp on the input struct.
func nowOrTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

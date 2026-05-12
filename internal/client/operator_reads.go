// operator_reads.go — typed read-method extensions for the operator-tier
// verbs landed in sty_ef248ab2 (cli-primary order:04-followup).
//
// Each method delegates to the underlying store(s) the corresponding MCP
// handler used. Membership + project resolution flow through the same
// helpers the order:04 anchor introduced (ResolveCallerMemberships,
// ResolveProjectID, ResolveProjectWorkspaceID).
//
// The MCP layer continues to call its own handlers in mcpserver/* — the
// /api/v1 layer calls these typed methods. Parity with the MCP shapes
// is asserted by tests/api/api_integration_test.go.

package client

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// ----- story -----

// StoryListInput captures the filter set for story_list.
type StoryListInput struct {
	ProjectID   string
	Status      string
	Priority    string
	Tag         string
	Limit       int
	Memberships []string
}

// StoryList returns stories scoped to a project, filtered by the supplied
// status / priority / tag / limit.
func (c *Client) StoryList(ctx context.Context, caller Caller, in StoryListInput) ([]story.Story, error) {
	if c.deps.Stories == nil {
		return nil, errors.New("story store not configured")
	}
	if in.ProjectID == "" {
		return nil, errors.New("project_id required")
	}
	opts := story.ListOptions{
		Status:   in.Status,
		Priority: in.Priority,
		Tag:      in.Tag,
		Limit:    in.Limit,
	}
	return c.deps.Stories.List(ctx, in.ProjectID, opts, in.Memberships)
}

// StoryTemplateGetInput names a story-category template lookup.
type StoryTemplateGetInput struct {
	Category string
}

// StoryTemplateGet returns the parsed Template for the given category.
// Returns an error when no template is registered.
func (c *Client) StoryTemplateGet(ctx context.Context, caller Caller, in StoryTemplateGetInput) (story.Template, error) {
	if in.Category == "" {
		return story.Template{}, errors.New("category required")
	}
	t, ok := c.loadStoryTemplate(ctx, in.Category)
	if !ok {
		return story.Template{}, fmt.Errorf("no story template registered for category %q", in.Category)
	}
	return t, nil
}

// StoryTemplateList returns every system-scope story_template document
// parsed into Template form. Malformed templates are skipped.
func (c *Client) StoryTemplateList(ctx context.Context, caller Caller) ([]story.Template, error) {
	if c.deps.Documents == nil {
		return []story.Template{}, nil
	}
	docs, err := c.deps.Documents.List(ctx, document.ListOptions{
		Type:  document.TypeStoryTemplate,
		Scope: document.ScopeSystem,
		Limit: 100,
	}, nil)
	if err != nil {
		return nil, err
	}
	out := make([]story.Template, 0, len(docs))
	for _, d := range docs {
		t, err := story.LoadTemplate(d)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// ----- task -----

// TaskListInput captures the filter set for task_list.
type TaskListInput struct {
	StoryID         string
	Status          string
	Kind            string
	Origin          string
	Priority        string
	ClaimedBy       string
	Limit           int
	IncludeArchived bool
	Memberships     []string
}

// TaskList returns tasks matching the supplied filters. Workspace-scoped
// via memberships.
func (c *Client) TaskList(ctx context.Context, caller Caller, in TaskListInput) ([]task.Task, error) {
	if c.deps.Tasks == nil {
		return nil, errors.New("task store not configured")
	}
	opts := task.ListOptions{
		StoryID:         in.StoryID,
		Status:          in.Status,
		Kind:            in.Kind,
		Origin:          in.Origin,
		Priority:        in.Priority,
		ClaimedBy:       in.ClaimedBy,
		Limit:           in.Limit,
		IncludeArchived: in.IncludeArchived,
	}
	return c.deps.Tasks.List(ctx, opts, in.Memberships)
}

// ----- project -----

// ProjectGetInput identifies a project to retrieve. Memberships scope
// the lookup.
type ProjectGetInput struct {
	ID          string
	Memberships []string
}

// ProjectGetOutput pairs the project row with the orientation bundle
// (intent + principles).
type ProjectGetOutput struct {
	Project    project.Project  `json:"project"`
	IntentBody string           `json:"intent_body,omitempty"`
	Principles []PrincipleEntry `json:"principles,omitempty"`
}

// ProjectGet returns the project + orientation bundle. Returns
// "project not found" for stories outside the caller's memberships.
func (c *Client) ProjectGet(ctx context.Context, caller Caller, in ProjectGetInput) (ProjectGetOutput, error) {
	if c.deps.Projects == nil {
		return ProjectGetOutput{}, errors.New("project store not configured")
	}
	if in.ID == "" {
		return ProjectGetOutput{}, errors.New("id required")
	}
	p, err := c.deps.Projects.GetByID(ctx, in.ID, in.Memberships)
	if err != nil || p.OwnerUserID != caller.UserID {
		return ProjectGetOutput{}, errors.New("project not found")
	}
	bundle := c.BuildOrientation(ctx, p)
	return ProjectGetOutput{
		Project:    p,
		IntentBody: bundle.IntentBody,
		Principles: bundle.Principles,
	}, nil
}

// ProjectList returns the caller's projects.
func (c *Client) ProjectList(ctx context.Context, caller Caller, memberships []string) ([]project.Project, error) {
	if c.deps.Projects == nil {
		return nil, errors.New("project store not configured")
	}
	if caller.UserID == "" {
		return nil, errors.New("no caller identity")
	}
	return c.deps.Projects.ListByOwner(ctx, caller.UserID, memberships)
}

// ----- workspace -----

// WorkspaceGetInput identifies a workspace lookup.
type WorkspaceGetInput struct {
	ID string
}

// WorkspaceGet returns a workspace row when the caller is a member.
func (c *Client) WorkspaceGet(ctx context.Context, caller Caller, in WorkspaceGetInput) (workspace.Workspace, error) {
	if c.deps.Workspaces == nil {
		return workspace.Workspace{}, errors.New("workspace store not configured")
	}
	if in.ID == "" {
		return workspace.Workspace{}, errors.New("id required")
	}
	if caller.UserID == "" {
		return workspace.Workspace{}, errors.New("no caller identity")
	}
	is, err := c.deps.Workspaces.IsMember(ctx, in.ID, caller.UserID)
	if err != nil || !is {
		return workspace.Workspace{}, errors.New("workspace not found")
	}
	w, err := c.deps.Workspaces.GetByID(ctx, in.ID)
	if err != nil {
		return workspace.Workspace{}, errors.New("workspace not found")
	}
	return w, nil
}

// WorkspaceList returns the workspaces the caller is a member of.
func (c *Client) WorkspaceList(ctx context.Context, caller Caller) ([]workspace.Workspace, error) {
	if c.deps.Workspaces == nil {
		return nil, errors.New("workspace store not configured")
	}
	if caller.UserID == "" {
		return nil, errors.New("no caller identity")
	}
	return c.deps.Workspaces.ListByMember(ctx, caller.UserID)
}

// WorkspaceMemberListInput identifies a workspace member-list lookup.
type WorkspaceMemberListInput struct {
	WorkspaceID string
}

// WorkspaceMemberList returns the members of a workspace when the caller
// is a member.
func (c *Client) WorkspaceMemberList(ctx context.Context, caller Caller, in WorkspaceMemberListInput) ([]workspace.Member, error) {
	if c.deps.Workspaces == nil {
		return nil, errors.New("workspace store not configured")
	}
	if in.WorkspaceID == "" {
		return nil, errors.New("workspace_id required")
	}
	if caller.UserID == "" {
		return nil, errors.New("no caller identity")
	}
	is, err := c.deps.Workspaces.IsMember(ctx, in.WorkspaceID, caller.UserID)
	if err != nil || !is {
		return nil, errors.New("workspace not found")
	}
	return c.deps.Workspaces.ListMembers(ctx, in.WorkspaceID)
}

// ----- changelog -----

// ChangelogGetInput names a changelog row lookup.
type ChangelogGetInput struct {
	ID          string
	Memberships []string
}

// ChangelogGet returns a changelog row when the caller has access to its
// project.
func (c *Client) ChangelogGet(ctx context.Context, caller Caller, in ChangelogGetInput) (changelog.Changelog, error) {
	if c.deps.Changelog == nil {
		return changelog.Changelog{}, errors.New("changelog store not configured")
	}
	if in.ID == "" {
		return changelog.Changelog{}, errors.New("id required")
	}
	row, err := c.deps.Changelog.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return changelog.Changelog{}, errors.New("changelog not found")
	}
	if _, err := c.ResolveProjectID(ctx, row.ProjectID, "", caller, in.Memberships); err != nil {
		return changelog.Changelog{}, errors.New("changelog not found")
	}
	return row, nil
}

// ChangelogListInput captures the filter set for changelog_list.
type ChangelogListInput struct {
	ProjectID   string
	Service     string
	Limit       int
	Memberships []string
}

// ChangelogList returns changelog rows newest-first.
func (c *Client) ChangelogList(ctx context.Context, caller Caller, in ChangelogListInput) ([]changelog.Changelog, error) {
	if c.deps.Changelog == nil {
		return nil, errors.New("changelog store not configured")
	}
	projectID, err := c.ResolveProjectID(ctx, in.ProjectID, "", caller, in.Memberships)
	if err != nil {
		return nil, err
	}
	return c.deps.Changelog.List(ctx, changelog.ListOptions{
		ProjectID: projectID,
		Service:   in.Service,
		Limit:     in.Limit,
	}, in.Memberships)
}

// ----- document family (search) -----

// DocumentSearchInput captures the document_search filter set. Type can
// be pinned by callers (the agent_search / contract_search / etc. wrappers
// at the wire layer set it).
type DocumentSearchInput struct {
	Type             string
	Query            string
	Scope            string
	ProjectID        string
	ContractBinding  string
	Tags             []string
	TopK             int
	Memberships      []string
	ResolveProjectID bool
}

// DocumentSearch is the typed surface for document_search + the type-
// pinned wrappers (agent_search, contract_search, principle_search,
// document_search). Reuses document.Store.Search with the supplied opts.
func (c *Client) DocumentSearch(ctx context.Context, caller Caller, in DocumentSearchInput) ([]document.Document, error) {
	if c.deps.Documents == nil {
		return nil, errors.New("document store not configured")
	}
	projectID := in.ProjectID
	if in.ResolveProjectID && projectID != "" {
		resolved, err := c.ResolveProjectID(ctx, projectID, "", caller, in.Memberships)
		if err == nil {
			projectID = resolved
		}
	}
	opts := document.SearchOptions{
		ListOptions: document.ListOptions{
			Type:            in.Type,
			Scope:           in.Scope,
			ProjectID:       projectID,
			ContractBinding: in.ContractBinding,
			Tags:            in.Tags,
		},
		Query: in.Query,
		TopK:  in.TopK,
	}
	return c.deps.Documents.Search(ctx, opts, in.Memberships)
}

// ----- principle get (typed wrapper of DocumentGet) -----
// Lives in operator_reads.go because principle_get is a thin wrapper of
// document_get with type pinned to "principle".

// PrincipleGet returns the principle document with the supplied id or
// (name, project_id) tuple.
func (c *Client) PrincipleGet(ctx context.Context, caller Caller, id, name, projectID string, memberships []string) (document.Document, error) {
	if c.deps.Documents == nil {
		return document.Document{}, errors.New("document store not configured")
	}
	resolvedProj, _ := c.ResolveProjectID(ctx, projectID, "", caller, memberships)
	wsID := c.ResolveProjectWorkspaceID(ctx, resolvedProj)
	return c.DocumentGet(ctx, caller, DocumentGetInput{
		ID:                id,
		Name:              name,
		Type:              document.TypePrinciple,
		WorkspaceID:       wsID,
		ResolvedProjectID: resolvedProj,
		Memberships:       memberships,
	})
}

// ----- KV -----

// KVScopeArgs is the resolved per-scope identifier set the projection
// helpers need. WorkspaceID / ProjectID / UserID match ledger.KVProjectionOptions.
type KVScopeArgs struct {
	Scope       ledger.KVScope
	WorkspaceID string
	ProjectID   string
	UserID      string
}

// ResolveKVScopeArgs validates the scope + the per-scope identifier
// requirements and resolves the project id through the caller's
// memberships. Mirrors mcpserver.Server.resolveKVScopeArgs so /api/v1
// and /mcp emit identical errors.
func (c *Client) ResolveKVScopeArgs(ctx context.Context, scope string, workspaceID, projectID, userID string, caller Caller, memberships []string) (KVScopeArgs, []string, error) {
	out := KVScopeArgs{}
	kvScope, ok := validKVScope(scope)
	if !ok {
		return out, nil, fmt.Errorf("invalid scope %q (want system|workspace|project|user)", scope)
	}
	out.Scope = kvScope
	mems := memberships
	switch kvScope {
	case ledger.KVScopeSystem:
		mems = append([]string{""}, mems...)
	case ledger.KVScopeWorkspace:
		if workspaceID == "" {
			return out, nil, errors.New("workspace_id is required for scope=workspace")
		}
		out.WorkspaceID = workspaceID
	case ledger.KVScopeProject:
		if projectID == "" {
			return out, nil, errors.New("project_id is required for scope=project")
		}
		resolved, err := c.ResolveProjectID(ctx, projectID, "", caller, mems)
		if err != nil {
			return out, nil, err
		}
		out.ProjectID = resolved
		out.WorkspaceID = c.ResolveProjectWorkspaceID(ctx, resolved)
	case ledger.KVScopeUser:
		if workspaceID == "" {
			return out, nil, errors.New("workspace_id is required for scope=user")
		}
		eff := userID
		if eff == "" {
			eff = caller.UserID
		}
		if eff == "" {
			return out, nil, errors.New("user_id is required for scope=user")
		}
		out.WorkspaceID = workspaceID
		out.UserID = eff
	}
	return out, mems, nil
}

// validKVScope is the package-private form of mcpserver.validKVScope.
func validKVScope(s string) (ledger.KVScope, bool) {
	switch ledger.KVScope(s) {
	case ledger.KVScopeSystem, ledger.KVScopeWorkspace, ledger.KVScopeProject, ledger.KVScopeUser:
		return ledger.KVScope(s), true
	default:
		return "", false
	}
}

// KVRow is the wire shape kv_get / kv_list / kv_get_resolved emit.
type KVRow struct {
	Scope         ledger.KVScope `json:"scope"`
	Key           string         `json:"key"`
	Value         string         `json:"value"`
	UpdatedAt     string         `json:"updated_at"`
	UpdatedBy     string         `json:"updated_by"`
	EntryID       string         `json:"entry_id"`
	ResolvedScope ledger.KVScope `json:"resolved_scope,omitempty"`
}

// KVGetInput names the (scope, key) tuple to read.
type KVGetInput struct {
	Scope       string
	Key         string
	WorkspaceID string
	ProjectID   string
	UserID      string
	Memberships []string
}

// KVGet returns the latest non-tombstone row for (scope, ids, key).
// Returns (KVRow{}, false, nil) on not-found.
func (c *Client) KVGet(ctx context.Context, caller Caller, in KVGetInput) (KVRow, bool, error) {
	if c.deps.Ledger == nil {
		return KVRow{}, false, errors.New("ledger store not configured")
	}
	if in.Key == "" {
		return KVRow{}, false, errors.New("key required")
	}
	args, mems, err := c.ResolveKVScopeArgs(ctx, in.Scope, in.WorkspaceID, in.ProjectID, in.UserID, caller, in.Memberships)
	if err != nil {
		return KVRow{}, false, err
	}
	opts := ledger.KVProjectionOptions{
		Scope:       args.Scope,
		WorkspaceID: args.WorkspaceID,
		ProjectID:   args.ProjectID,
		UserID:      args.UserID,
	}
	rows, err := ledger.KVProjectionScoped(ctx, c.deps.Ledger, opts, mems)
	if err != nil {
		return KVRow{}, false, err
	}
	row, present := rows[in.Key]
	if !present {
		return KVRow{}, false, nil
	}
	return KVRow{
		Scope:     row.Scope,
		Key:       row.Key,
		Value:     row.Value,
		UpdatedAt: row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedBy: row.UpdatedBy,
		EntryID:   row.EntryID,
	}, true, nil
}

// KVListInput captures the (scope, ids) tuple for kv_list.
type KVListInput struct {
	Scope       string
	WorkspaceID string
	ProjectID   string
	UserID      string
	Memberships []string
}

// KVList returns the full projection for the (scope, ids) tuple,
// sorted by key.
func (c *Client) KVList(ctx context.Context, caller Caller, in KVListInput) (ledger.KVScope, []KVRow, error) {
	if c.deps.Ledger == nil {
		return "", nil, errors.New("ledger store not configured")
	}
	args, mems, err := c.ResolveKVScopeArgs(ctx, in.Scope, in.WorkspaceID, in.ProjectID, in.UserID, caller, in.Memberships)
	if err != nil {
		return "", nil, err
	}
	opts := ledger.KVProjectionOptions{
		Scope:       args.Scope,
		WorkspaceID: args.WorkspaceID,
		ProjectID:   args.ProjectID,
		UserID:      args.UserID,
	}
	rows, err := ledger.KVProjectionScoped(ctx, c.deps.Ledger, opts, mems)
	if err != nil {
		return "", nil, err
	}
	out := make([]KVRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, KVRow{
			Scope:     row.Scope,
			Key:       row.Key,
			Value:     row.Value,
			UpdatedAt: row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
			UpdatedBy: row.UpdatedBy,
			EntryID:   row.EntryID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return args.Scope, out, nil
}

// KVGetResolvedInput captures the resolver inputs for kv_get_resolved.
type KVGetResolvedInput struct {
	Key         string
	WorkspaceID string
	ProjectID   string
	UserID      string
	Memberships []string
}

// KVGetResolved walks system → user → project → workspace returning the
// first matching value. Missing identifiers skip their tier. Returns
// (KVRow{}, false, nil) on not-found.
func (c *Client) KVGetResolved(ctx context.Context, caller Caller, in KVGetResolvedInput) (KVRow, bool, error) {
	if c.deps.Ledger == nil {
		return KVRow{}, false, errors.New("ledger store not configured")
	}
	if in.Key == "" {
		return KVRow{}, false, errors.New("key required")
	}
	memberships := append([]string{""}, in.Memberships...)
	effUser := in.UserID
	if effUser == "" {
		effUser = caller.UserID
	}
	opts := ledger.KVResolveOptions{
		WorkspaceID: in.WorkspaceID,
		ProjectID:   in.ProjectID,
		UserID:      effUser,
	}
	if opts.ProjectID != "" && opts.WorkspaceID == "" {
		opts.WorkspaceID = c.ResolveProjectWorkspaceID(ctx, opts.ProjectID)
	}
	row, found, err := ledger.KVResolveScoped(ctx, c.deps.Ledger, in.Key, opts, memberships)
	if err != nil {
		return KVRow{}, false, err
	}
	if !found {
		return KVRow{}, false, nil
	}
	return KVRow{
		Scope:         row.Scope,
		Key:           row.Key,
		Value:         row.Value,
		UpdatedAt:     row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedBy:     row.UpdatedBy,
		EntryID:       row.EntryID,
		ResolvedScope: row.Scope,
	}, true, nil
}

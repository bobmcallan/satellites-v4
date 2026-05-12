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
	"time"

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
// WorkspaceGet, WorkspaceList, and WorkspaceMemberList moved to
// internal/client/workspace.go alongside the workspace mutators
// (slice 8 of sty_f3f7bf9b).

// ----- changelog -----
// ChangelogGet, ChangelogList, and the new ChangelogAdd /
// ChangelogUpdate / ChangelogDelete mutators moved to
// internal/client/changelog.go (slice 9 of sty_f3f7bf9b).

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

// ----- KV writes (sty_4db0e025 slice A4) -----

// KVCheckWriteAuth enforces the per-scope role gate for kv_set and
// kv_delete (story_eb17cb16). Reads remain unrestricted within
// workspace boundaries.
//
//   - system: caller.GlobalAdmin == true. The seed loader writes via
//     the internal Append path, not MCP; this gate covers MCP/HTTP callers.
//   - workspace: caller is RoleAdmin of args.WorkspaceID.
//   - project: caller is project.OwnerUserID OR workspace admin of
//     project.WorkspaceID.
//   - user: caller.UserID == args.UserID. v1 default is self-only;
//     cross-user writes are not permitted (cross-tier admin override
//     deferred to a future story).
//
// Returns nil on permit and a structured "forbidden: scope=X requires
// role=Y" error on reject. Errors are user-safe. Mirrors the previous
// mcpserver.Server.kvCheckWriteAuth and httpserver-side helper so the
// MCP and HTTP transports share one auth surface.
func (c *Client) KVCheckWriteAuth(ctx context.Context, args KVScopeArgs, caller Caller) error {
	switch args.Scope {
	case ledger.KVScopeSystem:
		if !caller.GlobalAdmin {
			return errors.New("forbidden: scope=system requires role=global_admin")
		}
		return nil
	case ledger.KVScopeWorkspace:
		if caller.GlobalAdmin {
			return nil
		}
		if c.deps.Workspaces == nil {
			return errors.New("forbidden: workspace store unavailable")
		}
		role, err := c.deps.Workspaces.GetRole(ctx, args.WorkspaceID, caller.UserID)
		if err != nil || role != workspace.RoleAdmin {
			return errors.New("forbidden: scope=workspace requires role=workspace_admin")
		}
		return nil
	case ledger.KVScopeProject:
		if caller.GlobalAdmin {
			return nil
		}
		if c.deps.Projects == nil {
			return errors.New("forbidden: project store unavailable")
		}
		p, err := c.deps.Projects.GetByID(ctx, args.ProjectID, nil)
		if err == nil && p.OwnerUserID == caller.UserID {
			return nil
		}
		// Workspace admin of the project's workspace also passes.
		if c.deps.Workspaces != nil && args.WorkspaceID != "" {
			role, rerr := c.deps.Workspaces.GetRole(ctx, args.WorkspaceID, caller.UserID)
			if rerr == nil && role == workspace.RoleAdmin {
				return nil
			}
		}
		return errors.New("forbidden: scope=project requires role=project_owner_or_workspace_admin")
	case ledger.KVScopeUser:
		// v1 default: only self may write to their own user-scope KV.
		// Cross-user writes (workspace/global admins overriding) deferred.
		if caller.UserID == "" || caller.UserID != args.UserID {
			return errors.New("forbidden: scope=user is self-only (v1)")
		}
		return nil
	default:
		return fmt.Errorf("forbidden: unknown scope %q", args.Scope)
	}
}

// kvWriteEntry constructs the LedgerEntry for a KV write or delete.
// Tags include `scope:<scope>` plus `key:<name>`; user-scope rows add
// `user:<id>`; deletes add `kind:tombstone`. Package-private — the
// wire layer reaches the entry via KVSet / KVDelete only.
func kvWriteEntry(args KVScopeArgs, key, value, actor string, tombstone bool) ledger.LedgerEntry {
	tags := []string{
		"scope:" + string(args.Scope),
		"key:" + key,
	}
	if args.Scope == ledger.KVScopeUser && args.UserID != "" {
		tags = append(tags, "user:"+args.UserID)
	}
	if tombstone {
		tags = append(tags, ledger.KVTombstoneTag)
	}
	entry := ledger.LedgerEntry{
		WorkspaceID: args.WorkspaceID,
		Type:        ledger.TypeKV,
		Tags:        tags,
		Content:     value,
		CreatedBy:   actor,
	}
	if args.Scope == ledger.KVScopeProject {
		entry.ProjectID = args.ProjectID
	}
	return entry
}

// KVSetInput names the (scope, key, value) tuple to write.
type KVSetInput struct {
	Scope       string
	Key         string
	Value       string
	WorkspaceID string
	ProjectID   string
	UserID      string
	Memberships []string
	// Now overrides the append timestamp; zero falls back to
	// time.Now().UTC(). Tests pin a deterministic clock through this.
	Now time.Time
}

// KVSetOutput names the persisted row's id alongside the resolved
// scope echoed back to the caller.
type KVSetOutput struct {
	Scope   ledger.KVScope
	Key     string
	Value   string
	EntryID string
}

// KVSet appends a new KV row. Resolves the scope-specific identifier
// set, enforces the per-scope role gate via KVCheckWriteAuth, and
// writes the row through the ledger store. Mirrors the previous
// mcpserver.handleKVSet body so MCP + HTTP share one path.
func (c *Client) KVSet(ctx context.Context, caller Caller, in KVSetInput) (KVSetOutput, error) {
	if c.deps.Ledger == nil {
		return KVSetOutput{}, errors.New("ledger store not configured")
	}
	if in.Key == "" {
		return KVSetOutput{}, errors.New("key required")
	}
	args, _, err := c.ResolveKVScopeArgs(ctx, in.Scope, in.WorkspaceID, in.ProjectID, in.UserID, caller, in.Memberships)
	if err != nil {
		return KVSetOutput{}, err
	}
	if err := c.KVCheckWriteAuth(ctx, args, caller); err != nil {
		return KVSetOutput{}, err
	}
	entry := kvWriteEntry(args, in.Key, in.Value, caller.UserID, false)
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	row, err := c.deps.Ledger.Append(ctx, entry, now)
	if err != nil {
		return KVSetOutput{}, err
	}
	return KVSetOutput{
		Scope:   args.Scope,
		Key:     in.Key,
		Value:   in.Value,
		EntryID: row.ID,
	}, nil
}

// KVDeleteInput names the (scope, key) tuple to tombstone.
type KVDeleteInput struct {
	Scope       string
	Key         string
	WorkspaceID string
	ProjectID   string
	UserID      string
	Memberships []string
	Now         time.Time
}

// KVDeleteOutput names the tombstone row's id alongside the resolved
// scope echoed back to the caller.
type KVDeleteOutput struct {
	Scope   ledger.KVScope
	Key     string
	EntryID string
}

// KVDelete appends a tombstone row. The append-only ledger has no
// Delete primitive; the projection helpers suppress keys whose latest
// row carries the tombstone tag. Mirrors the previous
// mcpserver.handleKVDelete body.
func (c *Client) KVDelete(ctx context.Context, caller Caller, in KVDeleteInput) (KVDeleteOutput, error) {
	if c.deps.Ledger == nil {
		return KVDeleteOutput{}, errors.New("ledger store not configured")
	}
	if in.Key == "" {
		return KVDeleteOutput{}, errors.New("key required")
	}
	args, _, err := c.ResolveKVScopeArgs(ctx, in.Scope, in.WorkspaceID, in.ProjectID, in.UserID, caller, in.Memberships)
	if err != nil {
		return KVDeleteOutput{}, err
	}
	if err := c.KVCheckWriteAuth(ctx, args, caller); err != nil {
		return KVDeleteOutput{}, err
	}
	entry := kvWriteEntry(args, in.Key, "", caller.UserID, true)
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	row, err := c.deps.Ledger.Append(ctx, entry, now)
	if err != nil {
		return KVDeleteOutput{}, err
	}
	return KVDeleteOutput{
		Scope:   args.Scope,
		Key:     in.Key,
		EntryID: row.ID,
	}, nil
}

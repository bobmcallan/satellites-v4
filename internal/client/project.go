package client

import (
	"context"
	"errors"
	"time"

	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
)

// ErrRepoURLRequired is the typed-error sentinel for an empty / blank
// repo_url passed to ProjectSet. The wire-layer caller maps this to
// the documented "repo_url_required" error envelope.
var ErrRepoURLRequired = errors.New("repo_url_required")

// ErrRepoURLInvalid signals project.CanonicaliseGitRemote rejected the
// supplied URL. Wire-layer envelope: "repo_url_invalid".
var ErrRepoURLInvalid = errors.New("repo_url_invalid")

// ErrProjectStoreNotConfigured is returned when the typed project
// surface is called against a Client whose Deps.Projects is nil. The
// wire-layer caller maps this to a per-verb "unavailable" envelope.
var ErrProjectStoreNotConfigured = errors.New("project store not configured")

// ErrProjectNameRequired is returned by ProjectAdd when the input
// supplies no name. Wire-layer envelope mirrors the historical
// project_add error message produced by the HTTP handler.
var ErrProjectNameRequired = errors.New("name required")

// ErrProjectIDRequired is returned by ProjectUpdate / ProjectDelete
// when the input supplies no id. Mirrors the historical "id required"
// surfacing from the MCP RequireString path.
var ErrProjectIDRequired = errors.New("id required")

// ErrProjectNotFound is returned when GetByID fails or the row's
// owner_user_id does not match the calling user. Mirrors the
// "project not found" envelope the MCP handler produced.
var ErrProjectNotFound = errors.New("project not found")

// ErrNoCallerIdentity is returned when a typed project mutator runs
// without a resolved caller user id. Mirrors the historical
// "no caller identity" envelope the project_add HTTP handler raised.
var ErrNoCallerIdentity = errors.New("no caller identity")

// ProjectSetInput names the bind: repo_url is required; the workspace
// is resolved by the wire layer from the caller's session context.
// SessionID is optional — when non-empty + the session store is
// wired, ProjectSet auto-registers the session row and stamps its
// active project pointer.
type ProjectSetInput struct {
	RepoURL     string
	WorkspaceID string
	SessionID   string
	Now         time.Time
}

// ProjectSetOutput captures the resolved bundle. ResolvedProject is
// the zero value when Status="no_project_for_remote"; the canonical
// repo URL is always returned so the wire layer can echo it.
type ProjectSetOutput struct {
	Status           string            `json:"status"`
	ResolvedProject  project.Project   `json:"project,omitempty"`
	RepoURLCanonical string            `json:"repo_url_canonical"`
	Orientation      OrientationFields `json:"orientation,omitempty"`
}

const (
	// ProjectSetStatusResolved signals the canonical repo URL bound to
	// an existing project in the caller's workspace; orientation +
	// resolved project are populated.
	ProjectSetStatusResolved = "resolved"
	// ProjectSetStatusNoProject signals no project tracks the canonical
	// remote in the caller's workspace; the wire layer returns a
	// minimal envelope without orientation.
	ProjectSetStatusNoProject = "no_project_for_remote"
)

// ProjectSet binds the caller's session to the project that owns the
// canonicalised git remote in the caller's workspace, then returns the
// orientation bundle. Idempotent: a fresh bind on an already-bound
// session refreshes LastSeenAt + ActiveProjectID. Never creates a
// project — projects are minted by project_add, not by this bind.
//
// The function takes a worker-callback into the repos store (via
// c.deps.Repos.GetByRemote) to honour the workspace-scoped (workspace,
// canonical) index. Project resolution then loads the project row by
// id without re-applying memberships (the repo lookup already gated
// workspace membership).
func (c *Client) ProjectSet(ctx context.Context, caller Caller, in ProjectSetInput) (ProjectSetOutput, error) {
	canonical, err := canoniseGitRemote(in.RepoURL)
	if err != nil {
		return ProjectSetOutput{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := ProjectSetOutput{RepoURLCanonical: canonical}
	p, ok := c.resolveProjectByRemote(ctx, in.WorkspaceID, canonical)
	if !ok {
		out.Status = ProjectSetStatusNoProject
		return out, nil
	}
	out.Status = ProjectSetStatusResolved
	out.ResolvedProject = p
	if in.SessionID != "" && c.deps.Sessions != nil {
		_, _ = c.deps.Sessions.Register(ctx, caller.UserID, in.SessionID, session.SourceSessionStart, now)
		_, _ = c.deps.Sessions.SetActiveProject(ctx, caller.UserID, in.SessionID, p.ID, now)
	}
	out.Orientation = c.BuildOrientation(ctx, p)
	return out, nil
}

// canoniseGitRemote wraps project.CanonicaliseGitRemote with the
// typed ErrRepoURL* sentinels so the wire layer can switch on errors.Is
// without inspecting message text.
func canoniseGitRemote(raw string) (string, error) {
	if raw == "" {
		return "", ErrRepoURLRequired
	}
	canonical, err := project.CanonicaliseGitRemote(raw)
	if err != nil {
		return "", ErrRepoURLInvalid
	}
	if canonical == "" {
		return "", ErrRepoURLRequired
	}
	return canonical, nil
}

// resolveProjectByRemote walks repos.GetByRemote(workspace, canonical)
// → repo.ProjectID → projects.GetByID. Returns (project, true) when a
// repo row in the workspace tracks the canonical remote and the
// owning project is still readable (not archived, not cross-workspace).
// Mirrors mcpserver.Server.resolveProjectByRemote.
func (c *Client) resolveProjectByRemote(ctx context.Context, workspaceID, canonical string) (project.Project, bool) {
	if c.deps.Repos == nil || c.deps.Projects == nil || canonical == "" {
		return project.Project{}, false
	}
	r, err := c.deps.Repos.GetByRemote(ctx, workspaceID, canonical)
	if err != nil {
		return project.Project{}, false
	}
	p, err := c.deps.Projects.GetByID(ctx, r.ProjectID, nil)
	if err != nil {
		return project.Project{}, false
	}
	if p.WorkspaceID != workspaceID {
		return project.Project{}, false
	}
	return p, true
}

// ProjectAddInput captures the project_add request shape. The
// wire-layer caller pre-resolves the workspace id from the caller's
// session context and threads it in as WorkspaceID; Now overrides the
// timestamp for deterministic tests (zero falls back to time.Now().UTC()).
type ProjectAddInput struct {
	Name        string
	WorkspaceID string
	Now         time.Time
}

// ProjectAdd mints a new project row owned by caller.UserID in the
// supplied workspace. Returns the freshly-persisted project.Project;
// wire-layer assembly of the projectView (mcp_url / mcp_config) stays
// in the adapter because it depends on the request's base-URL stash —
// a transport-only concern.
func (c *Client) ProjectAdd(ctx context.Context, caller Caller, in ProjectAddInput) (project.Project, error) {
	if c.deps.Projects == nil {
		return project.Project{}, ErrProjectStoreNotConfigured
	}
	if caller.UserID == "" {
		return project.Project{}, ErrNoCallerIdentity
	}
	if in.Name == "" {
		return project.Project{}, ErrProjectNameRequired
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Projects.Create(ctx, caller.UserID, in.WorkspaceID, in.Name, now)
}

// ProjectUpdateInput captures the project_update partial-mutation
// shape. Name is applied only when non-empty and different from the
// existing row. MCPURL uses pointer semantics — nil means "not
// supplied"; a non-nil empty string clears the override. Memberships
// is pre-resolved by the wire layer for workspace scoping on
// GetByID.
type ProjectUpdateInput struct {
	ID          string
	Name        string
	MCPURL      *string
	Memberships []string
	Now         time.Time
}

// ProjectUpdate applies a partial mutation to a project owned by
// caller.UserID. Returns the post-update project.Project; ownership
// is enforced post-fetch so the typed surface mirrors the MCP
// handler's "project not found" envelope on cross-tenant access.
func (c *Client) ProjectUpdate(ctx context.Context, caller Caller, in ProjectUpdateInput) (project.Project, error) {
	if c.deps.Projects == nil {
		return project.Project{}, ErrProjectStoreNotConfigured
	}
	if in.ID == "" {
		return project.Project{}, ErrProjectIDRequired
	}
	existing, err := c.deps.Projects.GetByID(ctx, in.ID, in.Memberships)
	if err != nil || existing.OwnerUserID != caller.UserID {
		return project.Project{}, ErrProjectNotFound
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	updated := existing
	if in.Name != "" && in.Name != existing.Name {
		next, rerr := c.deps.Projects.UpdateName(ctx, in.ID, in.Name, now)
		if rerr != nil {
			return project.Project{}, rerr
		}
		updated = next
	}
	if in.MCPURL != nil && *in.MCPURL != updated.MCPURL {
		next, merr := c.deps.Projects.SetMCPURL(ctx, in.ID, *in.MCPURL, now)
		if merr != nil {
			return project.Project{}, merr
		}
		updated = next
	}
	return updated, nil
}

// ProjectDeleteInput captures the project_delete request shape.
// Memberships is pre-resolved by the wire layer. Soft-delete only —
// the substrate flips status to archived rather than removing rows.
type ProjectDeleteInput struct {
	ID          string
	Memberships []string
	Now         time.Time
}

// ProjectDelete soft-deletes a project owned by caller.UserID by
// flipping its status to archived. Returns the post-archive
// project.Project so the wire layer can echo the canonical row.
func (c *Client) ProjectDelete(ctx context.Context, caller Caller, in ProjectDeleteInput) (project.Project, error) {
	if c.deps.Projects == nil {
		return project.Project{}, ErrProjectStoreNotConfigured
	}
	if in.ID == "" {
		return project.Project{}, ErrProjectIDRequired
	}
	existing, err := c.deps.Projects.GetByID(ctx, in.ID, in.Memberships)
	if err != nil || existing.OwnerUserID != caller.UserID {
		return project.Project{}, ErrProjectNotFound
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Projects.SetStatus(ctx, in.ID, project.StatusArchived, now)
}

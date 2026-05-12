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
// project — projects are minted by project_create, not by this bind.
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

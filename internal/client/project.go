package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
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

// ErrProjectStatusInvalid is returned by ProjectUpdate when the
// caller supplies a status outside {active, archived}.
var ErrProjectStatusInvalid = errors.New("project status invalid")

// ProjectHasOpenWorkError is the typed concrete error returned by
// ProjectDelete when any story on the project still carries open
// (planned / published) tasks. Wire-shape mirrors story's
// StoryHasOpenTasksError: callers extract OpenTaskIDs + StoryIDs via
// errors.As to render the 422 / `project_has_open_work` envelope.
type ProjectHasOpenWorkError struct {
	ProjectID   string
	StoryIDs    []string
	OpenTaskIDs []string
}

// Error formats the envelope for logs / error messages.
func (e *ProjectHasOpenWorkError) Error() string {
	return fmt.Sprintf("project_has_open_work: %s has %d story(s) with %d open task(s)", e.ProjectID, len(e.StoryIDs), len(e.OpenTaskIDs))
}

// ErrProjectHasOpenWork is the sentinel matched by errors.Is when the
// open-work pre-flight gate fires. Wraps ProjectHasOpenWorkError per
// the established story.ErrStoryHasOpenTasks pattern.
var ErrProjectHasOpenWork = errors.New("project_has_open_work")

// Unwrap supports errors.Is(err, ErrProjectHasOpenWork).
func (e *ProjectHasOpenWorkError) Unwrap() error { return ErrProjectHasOpenWork }

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
func (c *Client) ProjectSet(ctx context.Context, caller Caller, in ProjectSetInput) (out ProjectSetOutput, err error) {
	defer func() {
		if err != nil || out.Status != ProjectSetStatusResolved {
			return
		}
		c.dispatchContextAudit(ctx, fetchAuditDispatchInput{
			Verb:        "project_set",
			ProjectID:   out.ResolvedProject.ID,
			WorkspaceID: out.ResolvedProject.WorkspaceID,
			ArgsMap:     map[string]any{"repo_url": in.RepoURL},
			Caller:      caller,
		}, sectionsForProjectSetOutput(out))
	}()
	canonical, cErr := canoniseGitRemote(in.RepoURL)
	if cErr != nil {
		err = cErr
		return
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out.RepoURLCanonical = canonical
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
	if p.Status == project.StatusArchived {
		return project.Project{}, false
	}
	return p, true
}

// ProjectAddInput captures the project_add request shape. The
// wire-layer caller pre-resolves the workspace id from the caller's
// session context and threads it in as WorkspaceID; Now overrides the
// timestamp for deterministic tests (zero falls back to time.Now().UTC()).
// RepoURL, when non-empty, triggers a paired RepoAdd that binds the new
// project to its canonical remote in one shot. Description, when
// non-empty, persists on the project row.
type ProjectAddInput struct {
	Name        string
	WorkspaceID string
	RepoURL     string
	Description string
	Memberships []string
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
	if in.RepoURL != "" {
		if _, err := project.CanonicaliseGitRemote(in.RepoURL); err != nil {
			return project.Project{}, ErrRepoURLInvalid
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	p, err := c.deps.Projects.Create(ctx, caller.UserID, in.WorkspaceID, in.Name, now)
	if err != nil {
		return project.Project{}, err
	}
	if in.Description != "" {
		updated, derr := c.deps.Projects.SetDescription(ctx, p.ID, in.Description, now)
		if derr != nil {
			return project.Project{}, derr
		}
		p = updated
	}
	if in.RepoURL != "" {
		if _, rerr := c.RepoAdd(ctx, caller, RepoAddInput{
			RawRemote:          in.RepoURL,
			RequestedProjectID: p.ID,
			ScopedProjectID:    p.ID,
			Memberships:        in.Memberships,
			Now:                now,
		}); rerr != nil {
			return project.Project{}, rerr
		}
	}
	return p, nil
}

// ProjectUpdateInput captures the project_update partial-mutation
// shape. Name is applied only when non-empty and different from the
// existing row. MCPURL / Description / Status use pointer semantics —
// nil means "not supplied"; a non-nil empty string clears (MCPURL,
// Description) or rejects (Status, validated against the active /
// archived enum). Memberships is pre-resolved by the wire layer for
// workspace scoping on GetByID.
type ProjectUpdateInput struct {
	ID          string
	Name        string
	MCPURL      *string
	Description *string
	Status      *string
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
	if in.Status != nil {
		switch *in.Status {
		case project.StatusActive, project.StatusArchived:
		default:
			return project.Project{}, ErrProjectStatusInvalid
		}
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
	if in.Description != nil && *in.Description != updated.Description {
		next, derr := c.deps.Projects.SetDescription(ctx, in.ID, *in.Description, now)
		if derr != nil {
			return project.Project{}, derr
		}
		updated = next
	}
	if in.Status != nil && *in.Status != updated.Status {
		next, serr := c.deps.Projects.SetStatus(ctx, in.ID, *in.Status, now)
		if serr != nil {
			return project.Project{}, serr
		}
		updated = next
	}
	return updated, nil
}

// ProjectDeleteInput captures the project_delete request shape.
// Memberships is pre-resolved by the wire layer. Default behaviour is
// soft-delete (status flip to archived); when Hard is true the substrate
// hard-purges the project row plus every dependent (stories, tasks,
// ledger, api-keys, repo rows) — operator-only path requiring the
// project to be archived first (sty_d357b28d).
type ProjectDeleteInput struct {
	ID          string
	Hard        bool
	Memberships []string
	Now         time.Time
}

// ProjectDeleteOutput pairs the project row with the cascade counts.
// Counters are populated according to the path taken:
//   - Hard=false (default soft-archive): StoriesCancelled + APIKeysArchived
//   - Hard=true (sty_d357b28d): StoriesPurged + TasksPurged + LedgerPurged +
//     APIKeysPurged + ReposPurged
//
// Both paths return the project row reflecting the post-mutation state
// (status=archived for soft; the row about to be deleted for hard — the
// wire layer just echoes the id).
type ProjectDeleteOutput struct {
	Project          project.Project `json:"project"`
	Hard             bool            `json:"hard"`
	StoriesCancelled int             `json:"stories_cancelled,omitempty"`
	APIKeysArchived  int             `json:"apikeys_archived,omitempty"`
	StoriesPurged    int             `json:"stories_purged,omitempty"`
	TasksPurged      int             `json:"tasks_purged,omitempty"`
	LedgerPurged     int             `json:"ledger_purged,omitempty"`
	APIKeysPurged    int             `json:"apikeys_purged,omitempty"`
	ReposPurged      int             `json:"repos_purged,omitempty"`
}

// ErrProjectNotArchived is returned by ProjectDelete when Hard=true is
// requested against a project whose status != archived. Operator must
// soft-delete first (see the cascade summary) before the hard purge.
// Sty_d357b28d.
var ErrProjectNotArchived = errors.New("project_not_archived")

// ProjectDelete soft-deletes a project owned by caller.UserID. The
// cascade follows the order documented in the plan's §3.3:
//
//  1. Pre-flight gate — reject with *ProjectHasOpenWorkError when any
//     non-terminal story still carries planned / published tasks.
//     `pr_story_terminal_gate` parity.
//  2. Archive every active API key bound to the project (auth.APIKeyStore.Delete
//     is the soft-delete path).
//  3. Cancel every non-terminal story via UpdateStatusDerived (the
//     reconciler-bypass path: gate already cleared, derived actor
//     stamped for audit).
//  4. Flip the project row to archived.
//
// Ledger rows survive — pr_no_unrequested_compat keeps the archive
// soft and the audit trail intact.
func (c *Client) ProjectDelete(ctx context.Context, caller Caller, in ProjectDeleteInput) (ProjectDeleteOutput, error) {
	if c.deps.Projects == nil {
		return ProjectDeleteOutput{}, ErrProjectStoreNotConfigured
	}
	if in.ID == "" {
		return ProjectDeleteOutput{}, ErrProjectIDRequired
	}
	existing, err := c.deps.Projects.GetByID(ctx, in.ID, in.Memberships)
	if err != nil || existing.OwnerUserID != caller.UserID {
		return ProjectDeleteOutput{}, ErrProjectNotFound
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if in.Hard {
		return c.projectDeleteHard(ctx, caller, in, existing)
	}
	openTaskIDs, blockingStoryIDs, err := c.projectOpenWork(ctx, in.ID, in.Memberships)
	if err != nil {
		return ProjectDeleteOutput{}, err
	}
	if len(openTaskIDs) > 0 {
		return ProjectDeleteOutput{}, &ProjectHasOpenWorkError{
			ProjectID:   in.ID,
			StoryIDs:    blockingStoryIDs,
			OpenTaskIDs: openTaskIDs,
		}
	}
	apikeysArchived, err := c.cascadeArchiveAPIKeys(ctx, caller, in.ID)
	if err != nil {
		return ProjectDeleteOutput{}, err
	}
	storiesCancelled, err := c.cascadeCancelStories(ctx, in.ID, in.Memberships, now)
	if err != nil {
		return ProjectDeleteOutput{}, err
	}
	archived, err := c.deps.Projects.SetStatus(ctx, in.ID, project.StatusArchived, now)
	if err != nil {
		return ProjectDeleteOutput{}, err
	}
	return ProjectDeleteOutput{
		Project:          archived,
		StoriesCancelled: storiesCancelled,
		APIKeysArchived:  apikeysArchived,
	}, nil
}

// projectDeleteHard hard-purges an archived project + every dependent
// row across stories, tasks, ledger, api-keys, and repos, then removes
// the project row itself. Sty_d357b28d. Refuses with
// ErrProjectNotArchived when the project's status isn't already
// archived — operator must soft-delete first to inspect the cascade
// summary before committing to the destructive op.
func (c *Client) projectDeleteHard(ctx context.Context, caller Caller, in ProjectDeleteInput, existing project.Project) (ProjectDeleteOutput, error) {
	if existing.Status != project.StatusArchived {
		return ProjectDeleteOutput{}, ErrProjectNotArchived
	}
	out := ProjectDeleteOutput{Project: existing, Hard: true}
	if c.deps.Tasks != nil {
		n, err := c.deps.Tasks.DeleteByProjectID(ctx, in.ID)
		if err != nil {
			return ProjectDeleteOutput{}, fmt.Errorf("project_delete hard: tasks purge: %w", err)
		}
		out.TasksPurged = n
	}
	if c.deps.Stories != nil {
		n, err := c.deps.Stories.DeleteByProjectID(ctx, in.ID)
		if err != nil {
			return ProjectDeleteOutput{}, fmt.Errorf("project_delete hard: stories purge: %w", err)
		}
		out.StoriesPurged = n
	}
	if c.deps.Ledger != nil {
		n, err := c.deps.Ledger.DeleteByProjectID(ctx, in.ID)
		if err != nil {
			return ProjectDeleteOutput{}, fmt.Errorf("project_delete hard: ledger purge: %w", err)
		}
		out.LedgerPurged = n
	}
	if c.deps.APIKeys != nil {
		n, err := c.deps.APIKeys.DeleteByProjectID(ctx, in.ID)
		if err != nil {
			return ProjectDeleteOutput{}, fmt.Errorf("project_delete hard: apikeys purge: %w", err)
		}
		out.APIKeysPurged = n
	}
	if c.deps.Repos != nil {
		n, err := c.deps.Repos.DeleteByProjectID(ctx, in.ID)
		if err != nil {
			return ProjectDeleteOutput{}, fmt.Errorf("project_delete hard: repos purge: %w", err)
		}
		out.ReposPurged = n
	}
	if err := c.deps.Projects.Delete(ctx, in.ID); err != nil {
		return ProjectDeleteOutput{}, fmt.Errorf("project_delete hard: project row: %w", err)
	}
	return out, nil
}

// ErrProjectMoveNotAdminSource is returned by ProjectMoveWorkspace when
// the caller is not an admin of the source workspace. Wire envelope:
// "not_admin_source". Sty_d357b28d.
var ErrProjectMoveNotAdminSource = errors.New("not_admin_source")

// ErrProjectMoveNotAdminTarget is returned by ProjectMoveWorkspace when
// the caller is not an admin of the target workspace. Wire envelope:
// "not_admin_target". Sty_d357b28d.
var ErrProjectMoveNotAdminTarget = errors.New("not_admin_target")

// ProjectMoveWorkspaceInput captures the project_move_workspace request
// shape. TargetWorkspaceID names the destination; the source is inferred
// from the project's current workspace_id. Sty_d357b28d.
type ProjectMoveWorkspaceInput struct {
	ID                string
	TargetWorkspaceID string
	Now               time.Time
}

// ProjectMoveWorkspaceOutput pairs the updated project row with the
// cascade counters so the wire layer can surface what the rewrite
// touched. Sty_d357b28d.
type ProjectMoveWorkspaceOutput struct {
	Project        project.Project `json:"project"`
	SourceWorkspaceID string `json:"source_workspace_id"`
	TargetWorkspaceID string `json:"target_workspace_id"`
	StoriesMoved   int             `json:"stories_moved"`
	TasksMoved     int             `json:"tasks_moved"`
	LedgerMoved    int             `json:"ledger_moved"`
	APIKeysMoved   int             `json:"apikeys_moved"`
	ReposMoved     int             `json:"repos_moved"`
}

// ProjectMoveWorkspace rewrites the project's workspace_id and cascades
// the rewrite across every dependent row (stories, tasks, ledger,
// api-keys, repo rows). Caller must be admin of BOTH the source and the
// target workspaces. Idempotent when source==target (returns the project
// unchanged with all counters at zero). Sty_d357b28d.
func (c *Client) ProjectMoveWorkspace(ctx context.Context, caller Caller, in ProjectMoveWorkspaceInput) (ProjectMoveWorkspaceOutput, error) {
	if c.deps.Projects == nil {
		return ProjectMoveWorkspaceOutput{}, ErrProjectStoreNotConfigured
	}
	if c.deps.Workspaces == nil {
		return ProjectMoveWorkspaceOutput{}, ErrWorkspaceStoreNotConfigured
	}
	if in.ID == "" {
		return ProjectMoveWorkspaceOutput{}, ErrProjectIDRequired
	}
	if in.TargetWorkspaceID == "" {
		return ProjectMoveWorkspaceOutput{}, errors.New("target_workspace_id required")
	}
	if caller.UserID == "" {
		return ProjectMoveWorkspaceOutput{}, ErrNoCallerIdentity
	}
	existing, err := c.deps.Projects.GetByID(ctx, in.ID, nil)
	if err != nil || existing.OwnerUserID != caller.UserID {
		return ProjectMoveWorkspaceOutput{}, ErrProjectNotFound
	}
	source := existing.WorkspaceID
	out := ProjectMoveWorkspaceOutput{
		Project:           existing,
		SourceWorkspaceID: source,
		TargetWorkspaceID: in.TargetWorkspaceID,
	}
	if source == in.TargetWorkspaceID {
		return out, nil
	}
	// Caller must be admin of BOTH workspaces (source for move-out
	// authority, target for move-in authority).
	sourceRole, err := c.deps.Workspaces.GetRole(ctx, source, caller.UserID)
	if err != nil || sourceRole != workspace.RoleAdmin {
		return ProjectMoveWorkspaceOutput{}, ErrProjectMoveNotAdminSource
	}
	targetRole, err := c.deps.Workspaces.GetRole(ctx, in.TargetWorkspaceID, caller.UserID)
	if err != nil || targetRole != workspace.RoleAdmin {
		return ProjectMoveWorkspaceOutput{}, ErrProjectMoveNotAdminTarget
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if c.deps.Stories != nil {
		n, err := c.deps.Stories.SetWorkspaceIDByProjectID(ctx, in.ID, in.TargetWorkspaceID, now)
		if err != nil {
			return ProjectMoveWorkspaceOutput{}, fmt.Errorf("project_move_workspace: stories: %w", err)
		}
		out.StoriesMoved = n
	}
	if c.deps.Tasks != nil {
		n, err := c.deps.Tasks.SetWorkspaceIDByProjectID(ctx, in.ID, in.TargetWorkspaceID, now)
		if err != nil {
			return ProjectMoveWorkspaceOutput{}, fmt.Errorf("project_move_workspace: tasks: %w", err)
		}
		out.TasksMoved = n
	}
	if c.deps.Ledger != nil {
		n, err := c.deps.Ledger.SetWorkspaceIDByProjectID(ctx, in.ID, in.TargetWorkspaceID)
		if err != nil {
			return ProjectMoveWorkspaceOutput{}, fmt.Errorf("project_move_workspace: ledger: %w", err)
		}
		out.LedgerMoved = n
	}
	if c.deps.APIKeys != nil {
		n, err := c.deps.APIKeys.SetWorkspaceIDByProjectID(ctx, in.ID, in.TargetWorkspaceID)
		if err != nil {
			return ProjectMoveWorkspaceOutput{}, fmt.Errorf("project_move_workspace: apikeys: %w", err)
		}
		out.APIKeysMoved = n
	}
	if c.deps.Repos != nil {
		n, err := c.deps.Repos.SetWorkspaceIDByProjectID(ctx, in.ID, in.TargetWorkspaceID, now)
		if err != nil {
			return ProjectMoveWorkspaceOutput{}, fmt.Errorf("project_move_workspace: repos: %w", err)
		}
		out.ReposMoved = n
	}
	updated, err := c.deps.Projects.SetWorkspaceID(ctx, in.ID, in.TargetWorkspaceID, now)
	if err != nil {
		return ProjectMoveWorkspaceOutput{}, fmt.Errorf("project_move_workspace: project row: %w", err)
	}
	out.Project = updated
	return out, nil
}

// projectOpenWork enumerates non-terminal stories on the project and
// returns the union of their planned / published task ids. Returns
// (nil, nil, nil) when nothing blocks the archive. Used as the
// project_delete pre-flight gate; mirrors the per-story open-tasks
// gate the substrate already enforces on story.UpdateStatus.
func (c *Client) projectOpenWork(ctx context.Context, projectID string, memberships []string) ([]string, []string, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil {
		return nil, nil, nil
	}
	stories, err := c.deps.Stories.List(ctx, projectID, story.ListOptions{Limit: 500}, memberships)
	if err != nil {
		return nil, nil, fmt.Errorf("project_delete: stories list: %w", err)
	}
	openTaskIDs := make([]string, 0)
	blockingStoryIDs := make([]string, 0)
	for _, s := range stories {
		if s.Status == story.StatusDone || s.Status == story.StatusCancelled {
			continue
		}
		tasks, terr := c.deps.Tasks.List(ctx, task.ListOptions{StoryID: s.ID, Limit: 500}, memberships)
		if terr != nil {
			return nil, nil, fmt.Errorf("project_delete: tasks list: %w", terr)
		}
		blocks := false
		for _, t := range tasks {
			if t.Status == task.StatusPublished || t.Status == task.StatusPlanned {
				openTaskIDs = append(openTaskIDs, t.ID)
				blocks = true
			}
		}
		if blocks {
			blockingStoryIDs = append(blockingStoryIDs, s.ID)
		}
	}
	return openTaskIDs, blockingStoryIDs, nil
}

// cascadeArchiveAPIKeys flips every active API key bound to projectID
// to APIKeyStatusArchived via auth.APIKeyStore.Delete (soft-delete).
// Returns the count touched. Nil-safe when the API key store isn't
// wired (test paths).
func (c *Client) cascadeArchiveAPIKeys(ctx context.Context, caller Caller, projectID string) (int, error) {
	if c.deps.APIKeys == nil {
		return 0, nil
	}
	rows, err := c.deps.APIKeys.List(ctx, "", projectID, false)
	if err != nil {
		return 0, fmt.Errorf("project_delete: apikeys list: %w", err)
	}
	archived := 0
	for _, k := range rows {
		if k.Status == auth.APIKeyStatusArchived {
			continue
		}
		if err := c.deps.APIKeys.Delete(ctx, k.ID); err != nil {
			return archived, fmt.Errorf("project_delete: apikey delete %s: %w", k.ID, err)
		}
		archived++
	}
	return archived, nil
}

// cascadeCancelStories walks non-terminal stories on the project and
// flips each to status=cancelled via UpdateStatusDerived — the
// reconciler-bypass path keeps the audit trail tagged
// `system:reconciler` and skips the ValidTransition forward-walk
// (the open-tasks gate has already cleared above).
func (c *Client) cascadeCancelStories(ctx context.Context, projectID string, memberships []string, now time.Time) (int, error) {
	if c.deps.Stories == nil {
		return 0, nil
	}
	stories, err := c.deps.Stories.List(ctx, projectID, story.ListOptions{Limit: 500}, memberships)
	if err != nil {
		return 0, fmt.Errorf("project_delete: stories list: %w", err)
	}
	cancelled := 0
	for _, s := range stories {
		if s.Status == story.StatusDone || s.Status == story.StatusCancelled {
			continue
		}
		if _, err := c.deps.Stories.UpdateStatusDerived(ctx, s.ID, story.StatusCancelled, now, memberships); err != nil {
			return cancelled, fmt.Errorf("project_delete: cancel story %s: %w", s.ID, err)
		}
		cancelled++
	}
	return cancelled, nil
}

package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// projectFixture wires the minimal store bundle the mutators need
// (project + workspace stores, a seeded workspace + an "u_alice"
// caller with admin membership). Mirrors story_test.go's storyFixture
// shape so the typed-surface tests stay uniform across nouns.
type projectFixture struct {
	t      *testing.T
	now    time.Time
	c      *Client
	wsID   string
	caller Caller
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	projStore := project.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	c := New(Deps{
		Projects:   projStore,
		Workspaces: wsStore,
	})

	return &projectFixture{
		t:      t,
		now:    now,
		c:      c,
		wsID:   ws.ID,
		caller: Caller{UserID: "u_alice", Memberships: []string{ws.ID}},
	}
}

func TestProjectAdd_HappyPath(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)
	assert.Equal(t, "satellites", p.Name)
	assert.Equal(t, "u_alice", p.OwnerUserID)
	assert.Equal(t, f.wsID, p.WorkspaceID)
	assert.NotEmpty(t, p.ID)
}

func TestProjectAdd_RejectsEmptyName(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		WorkspaceID: f.wsID,
	})
	require.ErrorIs(t, err, ErrProjectNameRequired)
}

func TestProjectAdd_RejectsMissingCaller(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectAdd(context.Background(), Caller{}, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
	})
	require.ErrorIs(t, err, ErrNoCallerIdentity)
}

func TestProjectAdd_RejectsMissingStore(t *testing.T) {
	c := New(Deps{})
	_, err := c.ProjectAdd(context.Background(), Caller{UserID: "u_alice"}, ProjectAddInput{Name: "x"})
	require.ErrorIs(t, err, ErrProjectStoreNotConfigured)
}

func TestProjectUpdate_RenamesAndStampsMCPURL(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)

	mcpURL := "https://satellites.example.com/mcp?project_id=" + p.ID
	updated, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		Name:        "satellites-renamed",
		MCPURL:      &mcpURL,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "satellites-renamed", updated.Name)
	assert.Equal(t, mcpURL, updated.MCPURL)
}

func TestProjectUpdate_MissingIDErrors(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{})
	require.ErrorIs(t, err, ErrProjectIDRequired)
}

func TestProjectUpdate_CrossOwnerHidesProject(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)

	_, err = f.c.ProjectUpdate(context.Background(), Caller{UserID: "u_bob", Memberships: []string{f.wsID}}, ProjectUpdateInput{
		ID:          p.ID,
		Name:        "should-fail",
		Memberships: []string{f.wsID},
	})
	require.ErrorIs(t, err, ErrProjectNotFound)
}

func TestProjectUpdate_MCPURLClearsWhenEmptyPointerSupplied(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)

	// First stamp a value.
	val := "https://example.com/mcp"
	updated, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		MCPURL:      &val,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, val, updated.MCPURL)

	// Now clear by supplying a non-nil empty pointer (mirrors the
	// MCP handler's GetArguments()["mcp_url"] present-but-empty
	// path).
	empty := ""
	cleared, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		MCPURL:      &empty,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "", cleared.MCPURL)
}

func TestProjectDelete_ArchivesProject(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)

	out, err := f.c.ProjectDelete(context.Background(), f.caller, ProjectDeleteInput{
		ID:          p.ID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, project.StatusArchived, out.Project.Status)
	assert.Equal(t, p.ID, out.Project.ID)
	assert.Equal(t, 0, out.StoriesCancelled)
	assert.Equal(t, 0, out.APIKeysArchived)
}

func TestProjectDelete_MissingIDErrors(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectDelete(context.Background(), f.caller, ProjectDeleteInput{})
	require.ErrorIs(t, err, ErrProjectIDRequired)
}

func TestProjectDelete_CrossOwnerHidesProject(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)

	_, err = f.c.ProjectDelete(context.Background(), Caller{UserID: "u_bob", Memberships: []string{f.wsID}}, ProjectDeleteInput{
		ID:          p.ID,
		Memberships: []string{f.wsID},
	})
	require.ErrorIs(t, err, ErrProjectNotFound)
}

// projectCascadeFixture wires the full dependency bundle the
// project_delete cascade needs: projects + stories + tasks + apikeys +
// ledger + repos + workspace, plus an authenticated alice caller.
// Stories live on a shared ledger so UpdateStatusDerived works.
type projectCascadeFixture struct {
	t        *testing.T
	now      time.Time
	c        *Client
	wsID     string
	caller   Caller
	stories  story.Store
	tasks    task.Store
	apikeys  auth.APIKeyStore
	projects project.Store
	repos    repo.Store
	ledger   ledger.Store
}

func newProjectCascadeFixture(t *testing.T) *projectCascadeFixture {
	t.Helper()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	led := ledger.NewMemoryStore()
	wsStore := workspace.NewMemoryStore()
	projStore := project.NewMemoryStore()
	stStore := story.NewMemoryStore(led)
	tkStore := task.NewMemoryStore()
	apkStore := auth.NewMemoryAgentAPIKeyStore()
	repoStore := repo.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	c := New(Deps{
		Projects:   projStore,
		Workspaces: wsStore,
		Stories:    stStore,
		Tasks:      tkStore,
		APIKeys:    apkStore,
		Ledger:     led,
		Repos:      repoStore,
	})

	return &projectCascadeFixture{
		t:        t,
		now:      now,
		c:        c,
		wsID:     ws.ID,
		caller:   Caller{UserID: "u_alice", Memberships: []string{ws.ID}},
		stories:  stStore,
		tasks:    tkStore,
		apikeys:  apkStore,
		projects: projStore,
		repos:    repoStore,
		ledger:   led,
	}
}

// TestProjectAdd_WithRepoURL_BindsCanonical verifies AC1 / R1.5 — a
// project_add with repo_url canonicalises the remote, mints a repo row,
// and the view's repo_url_canonical surfaces the bound remote.
func TestProjectAdd_WithRepoURL_BindsCanonical(t *testing.T) {
	f := newProjectCascadeFixture(t)
	view, p, err := f.c.ProjectAddView(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		RepoURL:     "git@github.com:bobmcallan/resumere.git",
		Memberships: f.caller.Memberships,
		Now:         f.now,
	}, "")
	require.NoError(t, err)
	require.NotEmpty(t, p.ID)
	assert.NotEmpty(t, view.RepoURLCanonical, "view must denormalise repo_url_canonical")
	// canonicaliser strips the .git suffix + collapses scheme
	rows, err := f.repos.List(context.Background(), p.ID, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly one repo row bound to project")
	assert.Equal(t, rows[0].GitRemote, view.RepoURLCanonical)
}

// TestProjectAdd_WithDescription verifies AC1 / R1.6 — description
// round-trips through ProjectAdd.
func TestProjectAdd_WithDescription(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Description: "a substrate test bench",
		Now:         f.now,
	})
	require.NoError(t, err)
	assert.Equal(t, "a substrate test bench", p.Description)
}

// TestProjectUpdate_DescriptionSet verifies AC2 / R2.7 — non-nil
// pointer with a value sets the field.
func TestProjectUpdate_DescriptionSet(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)
	desc := "fresh description"
	updated, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		Description: &desc,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "fresh description", updated.Description)
}

// TestProjectUpdate_DescriptionClear verifies AC2 / R2.7 — non-nil
// empty pointer clears the field (pointer semantics, not "leave alone").
func TestProjectUpdate_DescriptionClear(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Description: "seeded",
		Now:         f.now,
	})
	require.NoError(t, err)
	require.Equal(t, "seeded", p.Description)

	empty := ""
	cleared, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		Description: &empty,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "", cleared.Description)
}

// TestProjectUpdate_StatusFlip verifies AC2 / R2.7 — status pointer
// transitions between active and archived.
func TestProjectUpdate_StatusFlip(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)
	require.Equal(t, project.StatusActive, p.Status)

	archive := project.StatusArchived
	archived, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		Status:      &archive,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, project.StatusArchived, archived.Status)

	active := project.StatusActive
	restored, err := f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		Status:      &active,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, project.StatusActive, restored.Status)
}

// TestProjectUpdate_StatusInvalidRejected verifies AC2 / R2.6 — values
// outside {active, archived} return ErrProjectStatusInvalid.
func TestProjectUpdate_StatusInvalidRejected(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectAdd(context.Background(), f.caller, ProjectAddInput{
		Name: "x", WorkspaceID: f.wsID, Now: f.now,
	})
	require.NoError(t, err)
	bogus := "deleted"
	_, err = f.c.ProjectUpdate(context.Background(), f.caller, ProjectUpdateInput{
		ID:          p.ID,
		Status:      &bogus,
		Memberships: f.caller.Memberships,
	})
	require.ErrorIs(t, err, ErrProjectStatusInvalid)
}

// TestProjectDelete_CascadesStoriesAndAPIKeys verifies AC3 / R3.4 /
// R3.5 / R3.6 / R3.8 — non-terminal stories flip to cancelled, terminal
// stories untouched, active API keys flip to archived, project flips
// to archived, cascade counts surface on the response.
func TestProjectDelete_CascadesStoriesAndAPIKeys(t *testing.T) {
	f := newProjectCascadeFixture(t)
	ctx := context.Background()
	p, err := f.c.ProjectAdd(ctx, f.caller, ProjectAddInput{
		Name: "satellites", WorkspaceID: f.wsID, Now: f.now,
	})
	require.NoError(t, err)

	// Seed: one ready story, one already-done story.
	readyStory, err := f.stories.Create(ctx, story.Story{
		WorkspaceID: f.wsID, ProjectID: p.ID,
		Title: "active", Status: story.StatusReady, CreatedBy: "u_alice",
	}, f.now)
	require.NoError(t, err)

	doneStory, err := f.stories.Create(ctx, story.Story{
		WorkspaceID: f.wsID, ProjectID: p.ID,
		Title: "shipped", Status: story.StatusDone, CreatedBy: "u_alice",
	}, f.now)
	require.NoError(t, err)

	// Seed: one active API key bound to the project.
	apk := auth.APIKey{
		ID:          auth.NewAPIKeyID(),
		Prefix:      "sat_test",
		KeyHash:     "abc",
		KeySalt:     "deadbeef",
		OwnerUserID: "u_alice",
		ProjectID:   p.ID,
		Status:      auth.APIKeyStatusActive,
		CreatedAt:   f.now,
	}
	require.NoError(t, f.apikeys.Create(ctx, apk))

	out, err := f.c.ProjectDelete(ctx, f.caller, ProjectDeleteInput{
		ID:          p.ID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, project.StatusArchived, out.Project.Status)
	assert.Equal(t, 1, out.StoriesCancelled, "only the non-terminal story should flip")
	assert.Equal(t, 1, out.APIKeysArchived)

	post, err := f.stories.GetByID(ctx, readyStory.ID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, story.StatusCancelled, post.Status, "ready story cascades to cancelled")

	postDone, err := f.stories.GetByID(ctx, doneStory.ID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, story.StatusDone, postDone.Status, "done story untouched")

	apkRow, err := f.apikeys.Get(ctx, apk.ID)
	require.NoError(t, err)
	assert.Equal(t, auth.APIKeyStatusArchived, apkRow.Status)
}

// TestProjectDelete_RejectsWhenOpenWork verifies AC3 / R3.2 / R3.3 —
// pre-flight gate fires on any planned / published task, and NO rows
// are mutated.
func TestProjectDelete_RejectsWhenOpenWork(t *testing.T) {
	f := newProjectCascadeFixture(t)
	ctx := context.Background()
	p, err := f.c.ProjectAdd(ctx, f.caller, ProjectAddInput{
		Name: "satellites", WorkspaceID: f.wsID, Now: f.now,
	})
	require.NoError(t, err)

	st, err := f.stories.Create(ctx, story.Story{
		WorkspaceID: f.wsID, ProjectID: p.ID,
		Title: "blocking", Status: story.StatusInProgress, CreatedBy: "u_alice",
	}, f.now)
	require.NoError(t, err)
	tk, err := f.tasks.Enqueue(ctx, task.Task{
		WorkspaceID: f.wsID, ProjectID: p.ID, StoryID: st.ID,
		Action: "contract:develop", Description: "blocking", Status: task.StatusPublished,
		Origin: task.OriginStoryStage, Priority: task.PriorityMedium,
	}, f.now)
	require.NoError(t, err)

	apk := auth.APIKey{
		ID: auth.NewAPIKeyID(), Prefix: "sat_test", KeyHash: "abc", KeySalt: "d",
		OwnerUserID: "u_alice", ProjectID: p.ID, Status: auth.APIKeyStatusActive, CreatedAt: f.now,
	}
	require.NoError(t, f.apikeys.Create(ctx, apk))

	_, err = f.c.ProjectDelete(ctx, f.caller, ProjectDeleteInput{
		ID:          p.ID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Hour),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrProjectHasOpenWork)

	var typed *ProjectHasOpenWorkError
	require.True(t, errors.As(err, &typed))
	assert.Contains(t, typed.StoryIDs, st.ID)
	assert.Contains(t, typed.OpenTaskIDs, tk.ID)

	// No row mutated on reject.
	postProj, err := f.projects.GetByID(ctx, p.ID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, project.StatusActive, postProj.Status)

	postStory, err := f.stories.GetByID(ctx, st.ID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, story.StatusInProgress, postStory.Status)

	postKey, err := f.apikeys.Get(ctx, apk.ID)
	require.NoError(t, err)
	assert.Equal(t, auth.APIKeyStatusActive, postKey.Status)
}

// TestProjectSet_SkipsArchived verifies AC3 / R3.7 — project_set
// returns no_project_for_remote rather than resolving an archived
// project's repo row.
func TestProjectSet_SkipsArchived(t *testing.T) {
	f := newProjectCascadeFixture(t)
	ctx := context.Background()
	p, err := f.c.ProjectAdd(ctx, f.caller, ProjectAddInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		RepoURL:     "git@github.com:bobmcallan/satellites.git",
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)

	// Sanity: project_set resolves while active.
	resolved, err := f.c.ProjectSet(ctx, f.caller, ProjectSetInput{
		RepoURL: "git@github.com:bobmcallan/satellites.git", WorkspaceID: f.wsID, Now: f.now,
	})
	require.NoError(t, err)
	require.Equal(t, ProjectSetStatusResolved, resolved.Status)

	// Archive the project (no open work; cascade clean).
	_, err = f.c.ProjectDelete(ctx, f.caller, ProjectDeleteInput{
		ID:          p.ID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Hour),
	})
	require.NoError(t, err)

	// After archive: same repo_url returns no_project_for_remote.
	skipped, err := f.c.ProjectSet(ctx, f.caller, ProjectSetInput{
		RepoURL: "git@github.com:bobmcallan/satellites.git", WorkspaceID: f.wsID,
		Now: f.now.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, ProjectSetStatusNoProject, skipped.Status)
}

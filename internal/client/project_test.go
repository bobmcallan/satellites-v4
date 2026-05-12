package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/project"
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

func TestProjectCreate_HappyPath(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
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

func TestProjectCreate_RejectsEmptyName(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
		WorkspaceID: f.wsID,
	})
	require.ErrorIs(t, err, ErrProjectNameRequired)
}

func TestProjectCreate_RejectsMissingCaller(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectCreate(context.Background(), Caller{}, ProjectCreateInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
	})
	require.ErrorIs(t, err, ErrNoCallerIdentity)
}

func TestProjectCreate_RejectsMissingStore(t *testing.T) {
	c := New(Deps{})
	_, err := c.ProjectCreate(context.Background(), Caller{UserID: "u_alice"}, ProjectCreateInput{Name: "x"})
	require.ErrorIs(t, err, ErrProjectStoreNotConfigured)
}

func TestProjectUpdate_RenamesAndStampsMCPURL(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
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
	p, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
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
	p, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
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
	p, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
		Name:        "satellites",
		WorkspaceID: f.wsID,
		Now:         f.now,
	})
	require.NoError(t, err)

	archived, err := f.c.ProjectDelete(context.Background(), f.caller, ProjectDeleteInput{
		ID:          p.ID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, project.StatusArchived, archived.Status)
	assert.Equal(t, p.ID, archived.ID)
}

func TestProjectDelete_MissingIDErrors(t *testing.T) {
	f := newProjectFixture(t)
	_, err := f.c.ProjectDelete(context.Background(), f.caller, ProjectDeleteInput{})
	require.ErrorIs(t, err, ErrProjectIDRequired)
}

func TestProjectDelete_CrossOwnerHidesProject(t *testing.T) {
	f := newProjectFixture(t)
	p, err := f.c.ProjectCreate(context.Background(), f.caller, ProjectCreateInput{
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

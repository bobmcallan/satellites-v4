package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

type storyFixture struct {
	t       *testing.T
	now     time.Time
	c       *Client
	wsID    string
	storyID string
	caller  Caller
}

func newStoryFixture(t *testing.T) *storyFixture {
	t.Helper()
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)

	ws, err := wsStore.Create(ctx, "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	st, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   "proj_test",
		Title:       "parent",
		Category:    "improvement",
	}, now)
	require.NoError(t, err)

	c := New(Deps{
		Documents:  docStore,
		Stories:    storyStore,
		Ledger:     ledStore,
		Workspaces: wsStore,
	})

	return &storyFixture{
		t:       t,
		now:     now,
		c:       c,
		wsID:    ws.ID,
		storyID: st.ID,
		caller:  Caller{UserID: "u_alice", Memberships: []string{ws.ID}},
	}
}

// TestStoryUpdate_StatusBacklogToReady exercises the status-transition
// path through the consolidated verb (sty_4db0e025 slice D1). The same
// store.UpdateStatus call fires under the hood — pr_story_terminal_gate
// is preserved.
func TestStoryUpdate_StatusBacklogToReady(t *testing.T) {
	f := newStoryFixture(t)
	target := story.StatusReady
	got, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Status:      &target,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, story.StatusReady, got.Status)
}

func TestStoryUpdate_RejectsEmptyStatus(t *testing.T) {
	f := newStoryFixture(t)
	empty := ""
	_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Status:      &empty,
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status is required")
}

func TestStoryUpdate_StatusCrossWorkspaceHides(t *testing.T) {
	f := newStoryFixture(t)
	target := story.StatusReady
	_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Status:      &target,
		Memberships: []string{"wksp_other"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "story not found")
}

func TestStoryUpdate_FieldsWriteWhenNoTemplate(t *testing.T) {
	f := newStoryFixture(t)
	// No story_template seeded for "improvement" category — template
	// gate is pass-through, the store accepts the field as a free-form
	// k/v on the story row.
	val := "test scope"
	got, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Fields:      map[string]*string{"scope": &val},
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "test scope", got.Fields["scope"])
}

func TestStoryUpdate_FieldsRejectsUnknownWithTemplate(t *testing.T) {
	f := newStoryFixture(t)
	// Seed a story_template with only "scope" declared so any other
	// field gets rejected with the typed message.
	_, err := f.c.deps.Documents.Create(context.Background(), document.Document{
		Type:   document.TypeStoryTemplate,
		Scope:  document.ScopeSystem,
		Name:   "improvement",
		Body:   "story template",
		Status: document.StatusActive,
		Structured: []byte(`{
			"category":"improvement",
			"fields":[{"name":"scope","type":"text","required":true,"prompt":"scope"}]
		}`),
	}, f.now)
	require.NoError(t, err)

	val := "anything"
	_, err = f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Fields:      map[string]*string{"nonsense_field": &val},
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared by the")
}

// TestStoryUpdate_OpenTasksGateFiresOnConsolidatedPath verifies
// pr_story_terminal_gate (the load-bearing substrate invariant)
// continues to fire when story_update is called with status=done while
// a published/planned task is open. Folding story_update_status into
// story_update preserves the gate by routing the call through the same
// MemoryStore.UpdateStatus method that holds the check. Sty_4db0e025 D1.
func TestStoryUpdate_OpenTasksGateFiresOnConsolidatedPath(t *testing.T) {
	f := newStoryFixture(t)
	f.c.deps.Stories.(*story.MemoryStore).SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return []string{"task_open1"}, nil
	})
	// Walk to a state from which `done` is a valid transition target so
	// the gate (not ValidTransition) is the failure mode under test.
	for _, target := range []string{story.StatusReady, story.StatusInProgress} {
		tgt := target
		_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
			ID:          f.storyID,
			Status:      &tgt,
			Memberships: f.caller.Memberships,
			Now:         f.now.Add(time.Minute),
		})
		require.NoError(t, err)
	}
	target := story.StatusDone
	_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Status:      &target,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(2 * time.Minute),
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, story.ErrStoryHasOpenTasks), "expected ErrStoryHasOpenTasks, got %v", err)
	var openErr *story.StoryHasOpenTasksError
	require.True(t, errors.As(err, &openErr), "error must unwrap to *StoryHasOpenTasksError, got %T", err)
	assert.Equal(t, []string{"task_open1"}, openErr.OpenTaskIDs)
}

func TestStoryAdd_HappyPathDefaultsPriorityAndCategory(t *testing.T) {
	f := newStoryFixture(t)
	st, err := f.c.StoryAdd(context.Background(), f.caller, StoryAddInput{
		ProjectID:   "proj_test",
		WorkspaceID: f.wsID,
		Title:       "ship widgets",
		Now:         f.now,
	})
	require.NoError(t, err)
	assert.Equal(t, "ship widgets", st.Title)
	assert.Equal(t, "medium", st.Priority)
	assert.Equal(t, "feature", st.Category)
	assert.Equal(t, "u_alice", st.CreatedBy)
	assert.Equal(t, f.wsID, st.WorkspaceID)
	assert.NotEmpty(t, st.ID)
}

func TestStoryAdd_RejectsMissingProjectID(t *testing.T) {
	f := newStoryFixture(t)
	_, err := f.c.StoryAdd(context.Background(), f.caller, StoryAddInput{
		WorkspaceID: f.wsID,
		Title:       "x",
	})
	require.ErrorIs(t, err, ErrStoryProjectIDRequired)
}

func TestStoryAdd_RejectsMissingTitle(t *testing.T) {
	f := newStoryFixture(t)
	_, err := f.c.StoryAdd(context.Background(), f.caller, StoryAddInput{
		ProjectID:   "proj_test",
		WorkspaceID: f.wsID,
	})
	require.ErrorIs(t, err, ErrStoryTitleRequired)
}

func TestStoryUpdate_AppliesProvidedFields(t *testing.T) {
	f := newStoryFixture(t)
	newTitle := "renamed"
	newPrio := "high"
	updated, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Title:       &newTitle,
		Priority:    &newPrio,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Title)
	assert.Equal(t, "high", updated.Priority)
}

func TestStoryUpdate_RejectsInvalidCategory(t *testing.T) {
	f := newStoryFixture(t)
	bogus := "nonsense_category"
	_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Category:    &bogus,
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid category")
}

func TestStoryUpdate_MissingIDErrors(t *testing.T) {
	f := newStoryFixture(t)
	_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{})
	require.ErrorIs(t, err, ErrStoryIDRequired)
}

func TestStoryUpdate_CrossWorkspaceHidesStory(t *testing.T) {
	f := newStoryFixture(t)
	newTitle := "renamed"
	_, err := f.c.StoryUpdate(context.Background(), f.caller, StoryUpdateInput{
		ID:          f.storyID,
		Title:       &newTitle,
		Memberships: []string{"wksp_other"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "story not found")
}

package client

import (
	"context"
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

func TestStoryUpdateStatus_BacklogToReady(t *testing.T) {
	f := newStoryFixture(t)
	got, err := f.c.StoryUpdateStatus(context.Background(), f.caller, StoryUpdateStatusInput{
		ID:          f.storyID,
		Status:      story.StatusReady,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, story.StatusReady, got.Status)
}

func TestStoryUpdateStatus_RejectsMissingStatus(t *testing.T) {
	f := newStoryFixture(t)
	_, err := f.c.StoryUpdateStatus(context.Background(), f.caller, StoryUpdateStatusInput{
		ID:          f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status is required")
}

func TestStoryUpdateStatus_StoryNotFoundOnCrossWorkspace(t *testing.T) {
	f := newStoryFixture(t)
	_, err := f.c.StoryUpdateStatus(context.Background(), f.caller, StoryUpdateStatusInput{
		ID:          f.storyID,
		Status:      story.StatusReady,
		Memberships: []string{"wksp_other"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "story not found")
}

func TestStoryFieldSet_WritesValueWhenNoTemplate(t *testing.T) {
	f := newStoryFixture(t)
	// No story_template seeded for "improvement" category — template
	// gate is pass-through, the store accepts the field as a free-form
	// k/v on the story row.
	got, err := f.c.StoryFieldSet(context.Background(), f.caller, StoryFieldSetInput{
		ID:          f.storyID,
		Field:       "scope",
		Value:       "test scope",
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "test scope", got.Fields["scope"])
}

func TestStoryFieldSet_RejectsUnknownFieldWithTemplate(t *testing.T) {
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

	_, err = f.c.StoryFieldSet(context.Background(), f.caller, StoryFieldSetInput{
		ID:          f.storyID,
		Field:       "nonsense_field",
		Value:       "anything",
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared by the")
}

func TestStoryFieldSet_RejectsMissingField(t *testing.T) {
	f := newStoryFixture(t)
	_, err := f.c.StoryFieldSet(context.Background(), f.caller, StoryFieldSetInput{
		ID:          f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field required")
}

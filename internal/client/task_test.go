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
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// taskFixture provides in-memory stores wired into a Client. Mirrors
// the seed of fixtures_test.go in mcpserver but stays inside the
// client package so the typed methods can be exercised without the
// wire layer.
type taskFixture struct {
	t       *testing.T
	now     time.Time
	caller  Caller
	c       *Client
	devID   string
	wsID    string
	projID  string
	storyID string
}

func newTaskFixture(t *testing.T) *taskFixture {
	t.Helper()
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	taskStore := task.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	st, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   "proj_test",
		Title:       "parent",
	}, now)
	require.NoError(t, err)

	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		PermissionPatterns: []string{"Read:**"},
		Delivers:           []string{task.ContractAction("develop")},
	})
	devDoc, err := docStore.Create(ctx, document.Document{
		Type:        document.TypeAgent,
		Scope:       document.ScopeSystem,
		Name:        "developer_agent",
		Body:        "agent body",
		Status:      document.StatusActive,
		Structured:  settings,
	}, now)
	require.NoError(t, err)

	c := New(Deps{
		Documents:  docStore,
		Stories:    storyStore,
		Ledger:     ledStore,
		Tasks:      taskStore,
		Workspaces: wsStore,
		StartedAt:  now,
	})

	return &taskFixture{
		t:       t,
		now:     now,
		caller:  Caller{UserID: "u_alice", Email: "google:alice@example.com", Memberships: []string{ws.ID}},
		c:       c,
		devID:   devDoc.ID,
		wsID:    ws.ID,
		projID:  "proj_test",
		storyID: st.ID,
	}
}

// TestTaskAdd_HappyPath checks the canonical develop dispatch shape.
// The verb projects an explicit story_id, accepts the agent's delivers
// list, and stamps a kind:task-published ledger row.
func TestTaskAdd_HappyPath(t *testing.T) {
	f := newTaskFixture(t)

	out, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "Implement the verb.",
		StoryID:     f.storyID,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, out.TaskID)
	assert.Equal(t, f.storyID, out.StoryID)
	assert.False(t, out.StoryMinted)
	assert.Equal(t, task.StatusPublished, out.Status)
	assert.Equal(t, f.devID, out.AgentID)
}

// TestTaskAdd_RejectsEmptyPrompt mirrors the wire error from
// handleTaskAdd: prompt is required.
func TestTaskAdd_RejectsEmptyPrompt(t *testing.T) {
	f := newTaskFixture(t)
	_, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID: f.devID,
		Prompt:  "   ",
		StoryID: f.storyID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt must not be empty")
}

// TestTaskAdd_RejectsBadAgentID locks the agent_not_found error path.
func TestTaskAdd_RejectsBadAgentID(t *testing.T) {
	f := newTaskFixture(t)
	_, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     "doc_does_not_exist",
		Prompt:      "anything",
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_not_found")
}

// TestTaskGet_RoundtripsAddedTask: TaskAdd → TaskGet → identical id.
func TestTaskGet_RoundtripsAddedTask(t *testing.T) {
	f := newTaskFixture(t)
	add, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "x",
		StoryID:     f.storyID,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)

	got, err := f.c.TaskGet(context.Background(), f.caller, TaskGetInput{
		ID:          add.TaskID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Equal(t, add.TaskID, got.ID)
	assert.Equal(t, task.StatusPublished, got.Status)
}

// TestTaskClaim_PicksPublishedTask: TaskAdd publishes; TaskClaim picks.
func TestTaskClaim_PicksPublishedTask(t *testing.T) {
	f := newTaskFixture(t)
	add, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "x",
		StoryID:     f.storyID,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)

	claimed, err := f.c.TaskClaim(context.Background(), f.caller, TaskClaimInput{
		WorkerID:    "worker_a",
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Second),
	})
	require.NoError(t, err)
	assert.Equal(t, add.TaskID, claimed.ID)
	assert.Equal(t, "worker_a", claimed.ClaimedBy)
}

// TestTaskUpdate_ClosesPublishedTask: status=closed flips the row.
func TestTaskUpdate_ClosesPublishedTask(t *testing.T) {
	f := newTaskFixture(t)
	add, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "x",
		StoryID:     f.storyID,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)
	// Claim so the task is at a non-terminal status the Close path
	// expects (memory store transitions published → active on claim).
	_, err = f.c.TaskClaim(context.Background(), f.caller, TaskClaimInput{
		WorkerID:    "worker_a",
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Second),
	})
	require.NoError(t, err)

	out, err := f.c.TaskUpdate(context.Background(), f.caller, TaskUpdateInput{
		ID:          add.TaskID,
		Status:      task.StatusClosed,
		Outcome:     task.OutcomeSuccess,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(2 * time.Second),
	})
	require.NoError(t, err)
	assert.Equal(t, task.StatusClosed, out.Status)
	assert.Equal(t, task.OutcomeSuccess, out.Outcome)
}

// TestTaskUpdate_RejectsBadOutcome locks the invalid_outcome error.
func TestTaskUpdate_RejectsBadOutcome(t *testing.T) {
	f := newTaskFixture(t)
	add, _ := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID: f.devID, Prompt: "x", StoryID: f.storyID,
		Action: task.ContractAction("develop"), Memberships: f.caller.Memberships, Now: f.now,
	})
	_, err := f.c.TaskUpdate(context.Background(), f.caller, TaskUpdateInput{
		ID:          add.TaskID,
		Status:      task.StatusClosed,
		Outcome:     "wibble",
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Second),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_outcome")
}

// TestTaskWalk_BuildsChainForStory walks the story task chain after a
// single TaskAdd: 1 task in the chain, no terminal status, action
// summary tallies one work task.
func TestTaskWalk_BuildsChainForStory(t *testing.T) {
	f := newTaskFixture(t)
	_, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "x",
		StoryID:     f.storyID,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)

	out, err := f.c.TaskWalk(context.Background(), f.caller, TaskWalkInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Equal(t, f.storyID, out.Story.ID)
	require.Len(t, out.Tasks, 1)
	assert.Equal(t, out.Tasks[0].ID, out.CurrentTaskID)
	require.Len(t, out.ActionSummary, 1)
	assert.Equal(t, task.ContractAction("develop"), out.ActionSummary[0].Action)
	assert.Equal(t, 1, out.ActionSummary[0].WorkTotal)
	assert.Equal(t, 1, out.ActionSummary[0].WorkOpen)
	assert.Equal(t, 0, out.ActionSummary[0].WorkClosed)
}

// TestTaskWalk_StoryNotFound returns the typed sentinel for unknown
// story ids so the wire layer can map to its own envelope.
func TestTaskWalk_StoryNotFound(t *testing.T) {
	f := newTaskFixture(t)
	_, err := f.c.TaskWalk(context.Background(), f.caller, TaskWalkInput{
		StoryID:     "sty_does_not_exist",
		Memberships: f.caller.Memberships,
	})
	require.ErrorIs(t, err, ErrStoryNotFound)
}

// TestTaskPlan_WritesPlannedStatus locks the plan-tier creation shape:
// origin required; status=planned; ledger root id returned when ledger
// is wired.
func TestTaskPlan_WritesPlannedStatus(t *testing.T) {
	f := newTaskFixture(t)
	out, err := f.c.TaskPlan(context.Background(), f.caller, TaskPlanInput{
		Origin:      task.OriginStoryStage,
		WorkspaceID: f.wsID,
		ProjectID:   f.projID,
		Kind:        task.KindWork,
		AgentID:     f.devID,
		Priority:    task.PriorityMedium,
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, out.TaskID)
	assert.Equal(t, task.StatusPlanned, out.Status)
	assert.Equal(t, task.OriginStoryStage, out.Origin)
	assert.NotEmpty(t, out.LedgerRootID, "ledger root id should be present when ledger store is wired")
}

// TestTaskPlan_RejectsMissingOrigin locks the validation gate.
func TestTaskPlan_RejectsMissingOrigin(t *testing.T) {
	f := newTaskFixture(t)
	_, err := f.c.TaskPlan(context.Background(), f.caller, TaskPlanInput{
		WorkspaceID: f.wsID,
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin")
}

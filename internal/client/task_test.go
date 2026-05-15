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
	t         *testing.T
	now       time.Time
	caller    Caller
	c         *Client
	devID     string
	wsID      string
	projID    string
	storyID   string
	taskStore *task.MemoryStore
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
		Type:       document.TypeAgent,
		Scope:      document.ScopeSystem,
		Name:       "developer_agent",
		Body:       "agent body",
		Status:     document.StatusActive,
		Structured: settings,
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
		t:         t,
		now:       now,
		caller:    Caller{UserID: "u_alice", Email: "google:alice@example.com", Memberships: []string{ws.ID}},
		c:         c,
		devID:     devDoc.ID,
		wsID:      ws.ID,
		projID:    "proj_test",
		storyID:   st.ID,
		taskStore: taskStore,
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

// TestTaskClaim_EmptyQueueReturnsErrNoTaskAvailable: claiming against an
// empty queue surfaces the re-exported sentinel (client.ErrNoTaskAvailable
// == task.ErrNoTaskAvailable). Transport handlers branch on the client
// sentinel without importing internal/task (pr_mcp_cli_shared_path).
func TestTaskClaim_EmptyQueueReturnsErrNoTaskAvailable(t *testing.T) {
	f := newTaskFixture(t)
	_, err := f.c.TaskClaim(context.Background(), f.caller, TaskClaimInput{
		WorkerID:    "worker_empty",
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	assert.ErrorIs(t, err, ErrNoTaskAvailable)
	assert.ErrorIs(t, err, task.ErrNoTaskAvailable, "re-export must alias the substrate sentinel")
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

// seedClosedFailureWork enqueues a closed=failure kind=work task on
// the fixture's story matching the supplied action — the orphan that
// the auto-supersession detection in TaskAdd should pick up.
func (f *taskFixture) seedClosedFailureWork(action string, createdAt time.Time) task.Task {
	f.t.Helper()
	ctx := context.Background()
	orphan, err := f.taskStore.Enqueue(ctx, task.Task{
		WorkspaceID: f.wsID,
		ProjectID:   f.projID,
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      action,
		AgentID:     f.devID,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
	}, createdAt)
	require.NoError(f.t, err)
	closed, err := f.taskStore.Close(ctx, orphan.ID, task.OutcomeFailure, createdAt.Add(time.Second), f.caller.Memberships)
	require.NoError(f.t, err)
	return closed
}

// TestTaskAdd_AutoSupersession_StampsPriorTaskID asserts AC1 happy
// path: a closed=failure work develop predecessor on the story is
// auto-linked to the new mint via PriorTaskID. sty_9d046bc7.
func TestTaskAdd_AutoSupersession_StampsPriorTaskID(t *testing.T) {
	f := newTaskFixture(t)
	orphan := f.seedClosedFailureWork(task.ContractAction("develop"), f.now)

	out, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "retry",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)

	got, err := f.c.TaskGet(context.Background(), f.caller, TaskGetInput{
		ID:          out.TaskID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Equal(t, orphan.ID, got.PriorTaskID, "auto-supersession must stamp prior_task_id on the new mint")
}

// TestTaskAdd_AutoSupersession_NoOrphan_LeavesEmpty asserts the
// detection is a no-op when there is no closed=failure predecessor on
// the story.
func TestTaskAdd_AutoSupersession_NoOrphan_LeavesEmpty(t *testing.T) {
	f := newTaskFixture(t)

	out, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "first attempt",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)

	got, err := f.c.TaskGet(context.Background(), f.caller, TaskGetInput{
		ID:          out.TaskID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Empty(t, got.PriorTaskID, "no orphan ⇒ prior_task_id stays empty")
}

// TestTaskAdd_AutoSupersession_AlreadyLinked_Skips asserts the
// detection skips orphans already pointed at by another successor
// on the chain.
func TestTaskAdd_AutoSupersession_AlreadyLinked_Skips(t *testing.T) {
	f := newTaskFixture(t)
	orphan := f.seedClosedFailureWork(task.ContractAction("develop"), f.now)

	// Successor already pointing at orphan via PriorTaskID.
	_, err := f.taskStore.Enqueue(context.Background(), task.Task{
		WorkspaceID: f.wsID,
		ProjectID:   f.projID,
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		AgentID:     f.devID,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
		PriorTaskID: orphan.ID,
	}, f.now.Add(30*time.Second))
	require.NoError(t, err)

	out, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "third attempt",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)

	got, err := f.c.TaskGet(context.Background(), f.caller, TaskGetInput{
		ID:          out.TaskID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Empty(t, got.PriorTaskID, "orphan already linked ⇒ new mint stays unlinked")
}

// TestTaskAdd_AutoSupersession_ActionMismatch_LeavesEmpty asserts
// the detection only fires on (kind, action) match — an orphan with
// a different action does not auto-link.
func TestTaskAdd_AutoSupersession_ActionMismatch_LeavesEmpty(t *testing.T) {
	f := newTaskFixture(t)
	_ = f.seedClosedFailureWork(task.ContractAction("plan"), f.now)

	out, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "develop attempt",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Minute),
	})
	require.NoError(t, err)

	got, err := f.c.TaskGet(context.Background(), f.caller, TaskGetInput{
		ID:          out.TaskID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Empty(t, got.PriorTaskID, "action mismatch ⇒ prior_task_id stays empty")
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

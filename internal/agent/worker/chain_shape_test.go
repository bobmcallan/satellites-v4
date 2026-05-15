package worker

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// chainShapeFixture wires the minimum Client fixture needed to
// exercise both authoring paths (explicit prior_task_id + auto-
// supersession) and project the resulting chain into the
// taskWalkResponse shape verifyChainPriorWorkSuccess consumes.
type chainShapeFixture struct {
	t       *testing.T
	now     time.Time
	caller  client.Caller
	c       *client.Client
	tasks   *task.MemoryStore
	devID   string
	wsID    string
	storyID string
}

func newChainShapeFixture(t *testing.T) *chainShapeFixture {
	t.Helper()
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
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
		ProjectID:   "proj_chain",
		Title:       "chain-shape fixture",
	}, now)
	require.NoError(t, err)

	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers: []string{task.ContractAction("develop")},
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

	c := client.New(client.Deps{
		Documents:  docStore,
		Stories:    storyStore,
		Ledger:     ledStore,
		Tasks:      taskStore,
		Workspaces: wsStore,
		StartedAt:  now,
	})

	return &chainShapeFixture{
		t:       t,
		now:     now,
		caller:  client.Caller{UserID: "u_alice", Memberships: []string{ws.ID}},
		c:       c,
		tasks:   taskStore,
		devID:   devDoc.ID,
		wsID:    ws.ID,
		storyID: st.ID,
	}
}

// chainAsWalk lists the fixture's story chain in creation order and
// projects it onto the taskWalkResponse shape consumed by
// verifyChainPriorWorkSuccess. The gate reads ID/Action/Kind/Status/
// Outcome/PriorTaskID — every other field is incidental.
func (f *chainShapeFixture) chainAsWalk() taskWalkResponse {
	f.t.Helper()
	rows, err := f.tasks.List(context.Background(), task.ListOptions{
		StoryID: f.storyID,
		Limit:   500,
	}, f.caller.Memberships)
	require.NoError(f.t, err)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	tw := taskWalkResponse{Tasks: make([]taskWalkTask, 0, len(rows))}
	for _, r := range rows {
		tw.Tasks = append(tw.Tasks, taskWalkTask{
			ID:          r.ID,
			Action:      r.Action,
			Kind:        r.Kind,
			Status:      r.Status,
			Outcome:     r.Outcome,
			PriorTaskID: r.PriorTaskID,
		})
	}
	return tw
}

// closeFailure flips id → closed/failure (the predecessor shape the
// gate refuses to ignore unless a successor stamps its prior_task_id).
func (f *chainShapeFixture) closeFailure(id string, at time.Time) {
	f.t.Helper()
	_, err := f.tasks.Close(context.Background(), id, task.OutcomeFailure, at, f.caller.Memberships)
	require.NoError(f.t, err)
}

// TestMergeToMain_ChainShapeAcceptsExplicitPriorTaskID (sty_27516920
// AC4): a retry minted via task_add(prior_task_id=<failed>) stamps the
// substrate row with the explicit linkage; verifyChainPriorWorkSuccess
// then accepts the chain because the failed predecessor has a successor
// pointing at it via prior_task_id. The merge_to_main runner is the
// ignoreID — its own claimed status is exempt from the gate.
func TestMergeToMain_ChainShapeAcceptsExplicitPriorTaskID(t *testing.T) {
	f := newChainShapeFixture(t)
	ctx := context.Background()

	// Iter-1 develop: mint via task_add, then close=failure to make it
	// a chain orphan absent a retry pointer.
	dev1, err := f.c.TaskAdd(ctx, f.caller, client.TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "iter-1 develop (will fail)",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)
	f.closeFailure(dev1.TaskID, f.now.Add(time.Second))

	// Iter-2 develop: caller supplies prior_task_id explicitly so the
	// caller wins (and the auto-supersession block is skipped). The
	// resulting row carries the caller's pointer verbatim.
	dev2, err := f.c.TaskAdd(ctx, f.caller, client.TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "iter-2 develop (explicit prior)",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		PriorTaskID: dev1.TaskID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(2 * time.Second),
	})
	require.NoError(t, err)

	// Close iter-2 success so the develop slot has a successful tail.
	_, err = f.c.TaskUpdate(ctx, f.caller, client.TaskUpdateInput{
		ID:          dev2.TaskID,
		Status:      task.StatusClosed,
		Outcome:     task.OutcomeSuccess,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(3 * time.Second),
	})
	require.NoError(t, err)

	// merge_to_main slot — the gate's ignoreID — sits at claimed.
	mergeSeed, err := f.tasks.Enqueue(ctx, task.Task{
		WorkspaceID: f.wsID,
		ProjectID:   "proj_chain",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("merge_to_main"),
		AgentID:     f.devID,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
	}, f.now.Add(4*time.Second))
	require.NoError(t, err)
	mergeTask, err := f.tasks.ClaimByID(ctx, mergeSeed.ID, "worker_a", f.now.Add(5*time.Second), f.caller.Memberships)
	require.NoError(t, err)

	// Sanity: the persisted dev2 row carries the explicit pointer.
	dev2Row, err := f.tasks.GetByID(ctx, dev2.TaskID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, dev1.TaskID, dev2Row.PriorTaskID, "explicit prior_task_id must persist on the iter-2 row")

	tw := f.chainAsWalk()
	require.NoError(t, verifyChainPriorWorkSuccess(tw, mergeTask.ID),
		"chain with explicit prior_task_id must clear the gate")
}

// TestMergeToMain_ChainShapeAcceptsAutoSupersededPriorTaskID
// (sty_27516920 AC4): a retry minted via task_add WITHOUT an explicit
// prior_task_id triggers the substrate's auto-supersession detection
// (sty_9d046bc7); the row's PriorTaskID is stamped to the orphan id.
// verifyChainPriorWorkSuccess is provenance-agnostic — same shape, same
// pass.
func TestMergeToMain_ChainShapeAcceptsAutoSupersededPriorTaskID(t *testing.T) {
	f := newChainShapeFixture(t)
	ctx := context.Background()

	dev1, err := f.c.TaskAdd(ctx, f.caller, client.TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "iter-1 develop (will fail)",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now,
	})
	require.NoError(t, err)
	f.closeFailure(dev1.TaskID, f.now.Add(time.Second))

	// Iter-2 develop: no explicit prior — substrate detects the
	// (story_id, kind, action) orphan and stamps PriorTaskID itself.
	dev2, err := f.c.TaskAdd(ctx, f.caller, client.TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "iter-2 develop (auto-superseded)",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(2 * time.Second),
	})
	require.NoError(t, err)

	_, err = f.c.TaskUpdate(ctx, f.caller, client.TaskUpdateInput{
		ID:          dev2.TaskID,
		Status:      task.StatusClosed,
		Outcome:     task.OutcomeSuccess,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(3 * time.Second),
	})
	require.NoError(t, err)

	mergeSeed, err := f.tasks.Enqueue(ctx, task.Task{
		WorkspaceID: f.wsID,
		ProjectID:   "proj_chain",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("merge_to_main"),
		AgentID:     f.devID,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
	}, f.now.Add(4*time.Second))
	require.NoError(t, err)
	mergeTask, err := f.tasks.ClaimByID(ctx, mergeSeed.ID, "worker_a", f.now.Add(5*time.Second), f.caller.Memberships)
	require.NoError(t, err)

	// Substrate auto-stamped the iter-2 row with the orphan id.
	dev2Row, err := f.tasks.GetByID(ctx, dev2.TaskID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, dev1.TaskID, dev2Row.PriorTaskID, "substrate auto-supersession must stamp PriorTaskID")

	tw := f.chainAsWalk()
	require.NoError(t, verifyChainPriorWorkSuccess(tw, mergeTask.ID),
		"chain with substrate-stamped prior_task_id must clear the gate")
}

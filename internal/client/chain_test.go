package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/task"
)

// Build a TaskWalkTask fixture row with sensible defaults.
func mkTask(id, action, kind, status, outcome string, createdAt time.Time) TaskWalkTask {
	return TaskWalkTask{
		ID:        id,
		Action:    action,
		Kind:      kind,
		Status:    status,
		Outcome:   outcome,
		CreatedAt: createdAt,
	}
}

// TestSelectLiveTask_PrefersClosedSuccess locks rule §3.1.
func TestSelectLiveTask_PrefersClosedSuccess(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	rows := []TaskWalkTask{
		mkTask("t1", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeFailure, base),
		mkTask("t2", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(time.Minute)),
		mkTask("t3", "contract:develop", task.KindWork, task.StatusPublished, "", base.Add(2*time.Minute)),
	}
	got := selectLiveTask(rows)
	require.NotNil(t, got)
	assert.Equal(t, "t2", got.ID, "closed=success must win even over a newer published row")
}

// TestSelectLiveTask_PublishedBeatsFailure locks rule §3.2 — the
// auto-supersession case where a closed=failure orphan has a fresh
// published sibling.
func TestSelectLiveTask_PublishedBeatsFailure(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	rows := []TaskWalkTask{
		mkTask("t1", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeFailure, base),
		mkTask("t2", "contract:develop", task.KindWork, task.StatusPublished, "", base.Add(time.Minute)),
	}
	got := selectLiveTask(rows)
	require.NotNil(t, got)
	assert.Equal(t, "t2", got.ID)
}

// TestSelectLiveTask_FailOnly locks rule §3.3 — the chain is blocked
// at a closed/failure leaf.
func TestSelectLiveTask_FailOnly(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	rows := []TaskWalkTask{
		mkTask("t1", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeFailure, base),
	}
	got := selectLiveTask(rows)
	require.NotNil(t, got)
	assert.Equal(t, "t1", got.ID)
}

// TestSelectLiveTask_Empty returns nil for an empty bucket.
func TestSelectLiveTask_Empty(t *testing.T) {
	got := selectLiveTask(nil)
	assert.Nil(t, got)
}

// TestComputeChainStatus_CleanChain returns the plan/work task as
// the first dispatchable when nothing has run yet.
func TestComputeChainStatus_CleanChain(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	walk := TaskWalkOutput{
		Story: TaskWalkStory{ID: "sty_demo", Status: "in_progress"},
		Tasks: []TaskWalkTask{
			mkTask("t_plan", "contract:plan", task.KindWork, task.StatusPublished, "", base),
		},
	}
	out := computeChainStatus(walk)
	assert.Equal(t, "t_plan", out.NextTaskID)
	assert.False(t, out.Terminal)
	require.Len(t, out.Phases, len(canonicalPhases))
	assert.Equal(t, "contract:plan", out.Phases[0].Action)
	assert.Equal(t, "t_plan", out.Phases[0].LiveTaskID)
}

// TestComputeChainStatus_AutoSuperseded locks AC4: the failed
// develop/work predecessor is bypassed in favour of its published
// sibling.
func TestComputeChainStatus_AutoSuperseded(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	walk := TaskWalkOutput{
		Story: TaskWalkStory{ID: "sty_demo", Status: "in_progress"},
		Tasks: []TaskWalkTask{
			mkTask("t_plan", "contract:plan", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base),
			mkTask("t_dev1", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeFailure, base.Add(time.Minute)),
			mkTask("t_dev2", "contract:develop", task.KindWork, task.StatusPublished, "", base.Add(2*time.Minute)),
		},
	}
	out := computeChainStatus(walk)
	assert.Equal(t, "t_dev2", out.NextTaskID, "router must skip the failed predecessor")
}

// TestComputeChainStatus_MidReview confirms the chain advances to
// develop/review once develop/work closes success.
func TestComputeChainStatus_MidReview(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	walk := TaskWalkOutput{
		Story: TaskWalkStory{ID: "sty_demo", Status: "in_progress"},
		Tasks: []TaskWalkTask{
			mkTask("t_plan", "contract:plan", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base),
			mkTask("t_dev", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(time.Minute)),
			mkTask("t_devrev", "contract:develop", task.KindReview, task.StatusPublished, "", base.Add(2*time.Minute)),
		},
	}
	out := computeChainStatus(walk)
	assert.Equal(t, "t_devrev", out.NextTaskID)
}

// TestComputeChainStatus_TerminalAllClosed marks Terminal=true when
// no phase has a dispatchable row.
func TestComputeChainStatus_TerminalAllClosed(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	walk := TaskWalkOutput{
		Story: TaskWalkStory{ID: "sty_demo", Status: "done"},
		Tasks: []TaskWalkTask{
			mkTask("t_plan", "contract:plan", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base),
			mkTask("t_dev", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(time.Minute)),
			mkTask("t_devrev", "contract:develop", task.KindReview, task.StatusClosed, task.OutcomeSuccess, base.Add(2*time.Minute)),
			mkTask("t_sr", "contract:story_review", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(3*time.Minute)),
			mkTask("t_commit", "contract:commit", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(4*time.Minute)),
			mkTask("t_merge", "contract:merge_to_main", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(5*time.Minute)),
		},
	}
	out := computeChainStatus(walk)
	assert.True(t, out.Terminal)
	assert.Empty(t, out.NextTaskID)
}

// TestComputeChainStatus_AnomalyStuck flags a closed/failure leaf
// with no successor on a non-terminal story.
func TestComputeChainStatus_AnomalyStuck(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	walk := TaskWalkOutput{
		Story: TaskWalkStory{ID: "sty_demo", Status: "in_progress"},
		Tasks: []TaskWalkTask{
			mkTask("t_plan", "contract:plan", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base),
			mkTask("t_dev", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeFailure, base.Add(time.Minute)),
		},
	}
	out := computeChainStatus(walk)
	assert.True(t, out.Terminal)
	require.NotEmpty(t, out.Anomalies)
	assert.Contains(t, out.Anomalies[0], "phase_blocked:contract:develop:work")
}

// TestComputeChainStatus_CommitAcceptsReview locks the "work or
// review" wildcard the commit phase uses for story_review.
func TestComputeChainStatus_CommitAcceptsReview(t *testing.T) {
	base := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	walk := TaskWalkOutput{
		Story: TaskWalkStory{ID: "sty_demo", Status: "in_progress"},
		Tasks: []TaskWalkTask{
			mkTask("t_plan", "contract:plan", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base),
			mkTask("t_dev", "contract:develop", task.KindWork, task.StatusClosed, task.OutcomeSuccess, base.Add(time.Minute)),
			mkTask("t_devrev", "contract:develop", task.KindReview, task.StatusClosed, task.OutcomeSuccess, base.Add(2*time.Minute)),
			mkTask("t_sr_rev", "contract:story_review", task.KindReview, task.StatusClosed, task.OutcomeSuccess, base.Add(3*time.Minute)),
			mkTask("t_commit", "contract:commit", task.KindWork, task.StatusPublished, "", base.Add(4*time.Minute)),
		},
	}
	out := computeChainStatus(walk)
	assert.Equal(t, "t_commit", out.NextTaskID)
}

// seedTask authors a task row directly via the memory store so chain
// tests can stage chains without paying the agent-capability checks
// TaskAdd enforces. Returns the persisted row.
func (f *taskFixture) seedTask(action, kind, status, outcome string, createdAt time.Time) task.Task {
	f.t.Helper()
	ctx := context.Background()
	enq, err := f.taskStore.Enqueue(ctx, task.Task{
		WorkspaceID: f.wsID,
		ProjectID:   f.projID,
		StoryID:     f.storyID,
		Kind:        kind,
		Action:      action,
		AgentID:     f.devID,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
	}, createdAt)
	require.NoError(f.t, err)
	if status == task.StatusPublished || status == "" {
		return enq
	}
	if status == task.StatusClosed {
		closed, err := f.taskStore.Close(ctx, enq.ID, outcome, createdAt.Add(time.Second), f.caller.Memberships)
		require.NoError(f.t, err)
		return closed
	}
	return enq
}

// TestChainAdvance_NilHookReturnsNextTaskID validates the MCP-friendly
// branch: no DispatchHook ⇒ output names the next task but does not
// fire side effects.
func TestChainAdvance_NilHookReturnsNextTaskID(t *testing.T) {
	f := newTaskFixture(t)
	plan := f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusPublished, "", f.now)

	out, err := f.c.ChainAdvance(context.Background(), f.caller, ChainAdvanceInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Equal(t, plan.ID, out.NextTaskID)
	assert.False(t, out.Dispatched)
	assert.False(t, out.Terminal)
}

// TestChainAdvance_HookFiresOnPublishedRow validates the CLI branch:
// hook is invoked exactly once with the dispatchable row's id, ack is
// mirrored back.
func TestChainAdvance_HookFiresOnPublishedRow(t *testing.T) {
	f := newTaskFixture(t)
	plan := f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusPublished, "", f.now)

	var dispatched []string
	hook := func(_ context.Context, id string) (DispatchAck, error) {
		dispatched = append(dispatched, id)
		return DispatchAck{Dispatched: true, DaemonPID: 12345, QueuePosition: 1}, nil
	}
	out, err := f.c.ChainAdvance(context.Background(), f.caller, ChainAdvanceInput{
		StoryID:      f.storyID,
		Memberships:  f.caller.Memberships,
		DispatchHook: hook,
	})
	require.NoError(t, err)
	require.Len(t, dispatched, 1)
	assert.Equal(t, plan.ID, dispatched[0])
	assert.Equal(t, plan.ID, out.NextTaskID)
	assert.True(t, out.Dispatched)
	assert.Equal(t, 12345, out.Ack.DaemonPID)
}

// TestChainAdvance_SkipsAlreadyInFlight asserts the idempotency
// guarantee: a claimed/in_flight live row is not redispatched.
func TestChainAdvance_SkipsAlreadyInFlight(t *testing.T) {
	f := newTaskFixture(t)
	_ = f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusPublished, "", f.now)
	_, err := f.c.TaskClaim(context.Background(), f.caller, TaskClaimInput{
		WorkerID:    "worker_a",
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Second),
	})
	require.NoError(t, err)

	called := false
	hook := func(_ context.Context, _ string) (DispatchAck, error) {
		called = true
		return DispatchAck{Dispatched: true}, nil
	}
	out, err := f.c.ChainAdvance(context.Background(), f.caller, ChainAdvanceInput{
		StoryID:      f.storyID,
		Memberships:  f.caller.Memberships,
		DispatchHook: hook,
	})
	require.NoError(t, err)
	assert.False(t, called, "hook must not fire when live row is already claimed")
	assert.False(t, out.Dispatched)
	assert.NotEmpty(t, out.NextTaskID)
	assert.Equal(t, "already in flight", out.Detail)
}

// TestChainAdvance_HookPropagatesError surfaces the hook's error to
// the caller and leaves Dispatched=false.
func TestChainAdvance_HookPropagatesError(t *testing.T) {
	f := newTaskFixture(t)
	_ = f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusPublished, "", f.now)

	sentinel := errors.New("daemon not running")
	hook := func(_ context.Context, _ string) (DispatchAck, error) {
		return DispatchAck{}, sentinel
	}
	_, err := f.c.ChainAdvance(context.Background(), f.caller, ChainAdvanceInput{
		StoryID:      f.storyID,
		Memberships:  f.caller.Memberships,
		DispatchHook: hook,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

// TestChainRun_DefaultsAndTerminal exercises the loop body. After one
// iteration the hook closes the plan task and the next iteration sees
// the chain as no-dispatchable (no successor authored).
func TestChainRun_DefaultsAndTerminal(t *testing.T) {
	f := newTaskFixture(t)
	plan := f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusPublished, "", f.now)

	hookCalls := 0
	hook := func(ctx context.Context, id string) (DispatchAck, error) {
		hookCalls++
		_, _ = f.c.TaskClaim(ctx, f.caller, TaskClaimInput{
			WorkerID:    "worker_a",
			Memberships: f.caller.Memberships,
			Now:         f.now.Add(time.Second),
		})
		_, _ = f.c.TaskUpdate(ctx, f.caller, TaskUpdateInput{
			ID:          id,
			Status:      task.StatusClosed,
			Outcome:     task.OutcomeSuccess,
			Memberships: f.caller.Memberships,
			Now:         f.now.Add(2 * time.Second),
		})
		return DispatchAck{Dispatched: true}, nil
	}

	out, err := f.c.ChainRun(context.Background(), f.caller, ChainRunInput{
		StoryID:      f.storyID,
		Memberships:  f.caller.Memberships,
		PollInterval: time.Millisecond,
		DispatchHook: hook,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, out.Iterations, 1)
	assert.Equal(t, 1, hookCalls)
	assert.Contains(t, out.Dispatched, plan.ID)
	assert.Equal(t, TerminalStateNoDispatchable, out.TerminalState)
}

// TestChainRun_TimeoutShortCircuits asserts the deadline branch.
func TestChainRun_TimeoutShortCircuits(t *testing.T) {
	f := newTaskFixture(t)
	_ = f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusPublished, "", f.now)
	// Claim so the live row is in_flight — the hook should not fire
	// and the loop reschedules until the clock jumps past Timeout.
	_, err := f.c.TaskClaim(context.Background(), f.caller, TaskClaimInput{
		WorkerID:    "worker_a",
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(time.Second),
	})
	require.NoError(t, err)

	tick := 0
	clock := func() time.Time {
		tick++
		if tick == 1 {
			return f.now
		}
		return f.now.Add(time.Hour)
	}

	out, err := f.c.ChainRun(context.Background(), f.caller, ChainRunInput{
		StoryID:      f.storyID,
		Memberships:  f.caller.Memberships,
		PollInterval: time.Millisecond,
		Timeout:      time.Minute,
		Now:          clock,
		DispatchHook: func(_ context.Context, _ string) (DispatchAck, error) {
			t.Fatalf("hook must not fire when live row is in_flight")
			return DispatchAck{}, nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, TerminalStateTimeout, out.TerminalState)
}

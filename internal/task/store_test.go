package task_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/task"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		task    task.Task
		wantErr bool
	}{
		{
			name: "valid enqueued",
			task: task.Task{
				WorkspaceID: "w",
				Origin:      task.OriginScheduled,
				Status:      task.StatusEnqueued,
				Priority:    task.PriorityMedium,
			},
		},
		{
			name: "invalid origin",
			task: task.Task{
				WorkspaceID: "w",
				Origin:      "bogus",
				Status:      task.StatusEnqueued,
				Priority:    task.PriorityMedium,
			},
			wantErr: true,
		},
		{
			name: "invalid priority",
			task: task.Task{
				WorkspaceID: "w",
				Origin:      task.OriginScheduled,
				Status:      task.StatusEnqueued,
				Priority:    "urgent",
			},
			wantErr: true,
		},
		{
			name: "outcome without closed",
			task: task.Task{
				WorkspaceID: "w",
				Origin:      task.OriginScheduled,
				Status:      task.StatusEnqueued,
				Priority:    task.PriorityMedium,
				Outcome:     task.OutcomeSuccess,
			},
			wantErr: true,
		},
		{
			name: "closed without outcome",
			task: task.Task{
				WorkspaceID: "w",
				Origin:      task.OriginScheduled,
				Status:      task.StatusClosed,
				Priority:    task.PriorityMedium,
			},
			wantErr: true,
		},
		{
			name: "claimed without claimer",
			task: task.Task{
				WorkspaceID: "w",
				Origin:      task.OriginScheduled,
				Status:      task.StatusClaimed,
				Priority:    task.PriorityMedium,
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.task.Validate()
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidTransition(t *testing.T) {
	t.Parallel()
	assert.True(t, task.ValidTransition(task.StatusEnqueued, task.StatusClaimed))
	assert.True(t, task.ValidTransition(task.StatusClaimed, task.StatusInFlight))
	assert.True(t, task.ValidTransition(task.StatusClaimed, task.StatusEnqueued)) // reclaim
	assert.True(t, task.ValidTransition(task.StatusInFlight, task.StatusClosed))
	assert.False(t, task.ValidTransition(task.StatusClosed, task.StatusEnqueued))
	assert.False(t, task.ValidTransition(task.StatusEnqueued, task.StatusInFlight))
	assert.False(t, task.ValidTransition("bogus", task.StatusEnqueued))
}

func TestMemoryStore_EnqueueAndGet(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	enq, err := store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityHigh,
	}, now)
	require.NoError(t, err)
	assert.NotEmpty(t, enq.ID)
	assert.Equal(t, task.StatusEnqueued, enq.Status)
	got, err := store.GetByID(context.Background(), enq.ID, []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, enq.ID, got.ID)
}

func TestMemoryStore_ClaimPriorityOrder(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	// Enqueue a medium task first, then a critical task.
	medium, err := store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)
	critical, err := store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityCritical,
	}, now.Add(time.Second))
	require.NoError(t, err)

	claimed, err := store.Claim(context.Background(), "worker_a", []string{"w"}, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, critical.ID, claimed.ID, "critical should win over medium despite later created_at")

	// Medium is next.
	claimed2, err := store.Claim(context.Background(), "worker_a", []string{"w"}, now.Add(3*time.Second))
	require.NoError(t, err)
	assert.Equal(t, medium.ID, claimed2.ID)

	// Queue empty.
	_, err = store.Claim(context.Background(), "worker_a", []string{"w"}, now.Add(4*time.Second))
	assert.ErrorIs(t, err, task.ErrNoTaskAvailable)
}

func TestMemoryStore_Claim_DoubleClaim_Race(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	enq, err := store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityHigh,
	}, now)
	require.NoError(t, err)

	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := store.Claim(context.Background(), "worker", []string{"w"}, now.Add(time.Second))
			if err == nil && got.ID == enq.ID {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(1), atomic.LoadInt64(&wins), "exactly one goroutine should win the claim")
}

func TestMemoryStore_WorkspaceIsolation(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	taskA, _ := store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "wksp_A",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityMedium,
	}, now)
	_, _ = store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "wksp_B",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityMedium,
	}, now)

	// Worker in wksp_A cannot claim wksp_B's task.
	picked, err := store.Claim(context.Background(), "worker", []string{"wksp_A"}, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, taskA.ID, picked.ID)

	// After claiming A's task, claim returns no-task for wksp_A only.
	_, err = store.Claim(context.Background(), "worker", []string{"wksp_A"}, now.Add(2*time.Second))
	assert.ErrorIs(t, err, task.ErrNoTaskAvailable)

	// GetByID with wrong membership returns not-found.
	_, err = store.GetByID(context.Background(), taskA.ID, []string{"wksp_B"})
	assert.ErrorIs(t, err, task.ErrNotFound)
}

func TestMemoryStore_CloseAndReclaim(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	enq, err := store.Enqueue(context.Background(), task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)
	claimed, err := store.Claim(context.Background(), "worker", []string{"w"}, now.Add(time.Second))
	require.NoError(t, err)

	// Reclaim a claimed task back to enqueued.
	reclaimed, err := store.Reclaim(context.Background(), claimed.ID, "watchdog", now.Add(2*time.Second), []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, task.StatusEnqueued, reclaimed.Status)
	assert.Empty(t, reclaimed.ClaimedBy)

	// Close from enqueued directly.
	closed, err := store.Close(context.Background(), enq.ID, task.OutcomeSuccess, now.Add(3*time.Second), []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, task.StatusClosed, closed.Status)
	assert.Equal(t, task.OutcomeSuccess, closed.Outcome)

	// Close a closed task is rejected.
	_, err = store.Close(context.Background(), enq.ID, task.OutcomeFailure, now.Add(4*time.Second), []string{"w"})
	assert.ErrorIs(t, err, task.ErrInvalidTransition)

	// Invalid outcome rejected before transition check.
	_, err = store.Close(context.Background(), enq.ID, "partial", now.Add(5*time.Second), []string{"w"})
	require.Error(t, err)
}

// TestMemoryStore_StoryAndKindBinding: tasks persist StoryID and Kind,
// and List filters on both. sty_c6d76a5b checkpoint 14 retired the
// ContractInstanceID binding.
func TestMemoryStore_StoryAndKindBinding(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	ctx := context.Background()

	planXWork, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		StoryID:     "sty_x",
		Kind:        task.KindWork,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)
	planXRev, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		StoryID:     "sty_x",
		Kind:        task.KindReview,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)
	planYWork, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		StoryID:     "sty_y",
		Kind:        task.KindWork,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)

	got, err := store.GetByID(ctx, planXWork.ID, []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, "sty_x", got.StoryID)
	assert.Equal(t, task.KindWork, got.Kind)

	xRows, err := store.List(ctx, task.ListOptions{StoryID: "sty_x"}, []string{"w"})
	require.NoError(t, err)
	require.Len(t, xRows, 2)
	xIDs := map[string]bool{xRows[0].ID: true, xRows[1].ID: true}
	assert.True(t, xIDs[planXWork.ID])
	assert.True(t, xIDs[planXRev.ID])
	assert.False(t, xIDs[planYWork.ID])

	workRows, err := store.List(ctx, task.ListOptions{Kind: task.KindWork}, []string{"w"})
	require.NoError(t, err)
	require.Len(t, workRows, 2)

	combined, err := store.List(ctx, task.ListOptions{
		StoryID: "sty_x",
		Kind:    task.KindReview,
	}, []string{"w"})
	require.NoError(t, err)
	require.Len(t, combined, 1)
	assert.Equal(t, planXRev.ID, combined[0].ID)
}

// TestMemoryStore_IterationDefault verifies sty_78ddc67b — Enqueue
// stamps Iteration=1 when the caller doesn't supply one, and preserves
// caller-supplied iteration values verbatim.
func TestMemoryStore_IterationDefault(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	ctx := context.Background()

	noIter, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)
	assert.Equal(t, 1, noIter.Iteration)

	loop2, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Iteration:   2,
	}, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, 2, loop2.Iteration)

	// Round-trip: GetByID surfaces the stamped iteration.
	got, err := store.GetByID(ctx, loop2.ID, []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, 2, got.Iteration)
}

// TestMemoryStore_SetPriorTaskID verifies sty_9d046bc7 — the
// SetPriorTaskID backfill primitive stamps PriorTaskID on the active
// row and refuses to overwrite a non-empty value (idempotence). Used
// by tools/migrate_prior_task_id; not exposed via any MCP/HTTP/CLI
// verb.
func TestMemoryStore_SetPriorTaskID(t *testing.T) {
	t.Parallel()
	store := task.NewMemoryStore()
	now := time.Now().UTC()
	ctx := context.Background()

	orphan, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
	}, now)
	require.NoError(t, err)
	closed, err := store.Close(ctx, orphan.ID, task.OutcomeFailure, now.Add(time.Second), []string{"w"})
	require.NoError(t, err)

	active, err := store.Enqueue(ctx, task.Task{
		WorkspaceID: "w",
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPublished,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
	}, now.Add(2*time.Second))
	require.NoError(t, err)

	// Happy path: stamp the linkage.
	updated, err := store.SetPriorTaskID(ctx, active.ID, closed.ID, now.Add(3*time.Second), []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, closed.ID, updated.PriorTaskID)

	got, err := store.GetByID(ctx, active.ID, []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, closed.ID, got.PriorTaskID)

	// Idempotence: a second call against the already-linked row is a
	// no-op and returns the row unchanged.
	again, err := store.SetPriorTaskID(ctx, active.ID, "task_other", now.Add(4*time.Second), []string{"w"})
	require.NoError(t, err)
	assert.Equal(t, closed.ID, again.PriorTaskID, "SetPriorTaskID must not overwrite an already-linked row")

	// Membership scoping: a caller without the workspace cannot mutate.
	_, err = store.SetPriorTaskID(ctx, active.ID, closed.ID, now.Add(5*time.Second), []string{"other"})
	assert.ErrorIs(t, err, task.ErrNotFound)
}

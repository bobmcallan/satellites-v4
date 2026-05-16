package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/task"
)

// TestChainAdvance_AutoSupersession_PicksSibling reproduces the
// sty_690b06ee chain shape (AC4). The substrate auto-supersession
// from sty_9d046bc7 stamps prior_task_id on the iter-2 mint when a
// closed=failure predecessor exists; ChainAdvance must select the
// *successor* even though it sorts later than the closed/failure row.
//
// Shape:
//
//	plan/work          closed=success      base
//	develop/work iter1 closed=failure      base+1m   (orphan)
//	develop/work iter2 published           base+2m   (auto-stamped: prior=iter1)
//
// Expected: ChainAdvance returns iter2's id, not iter1.
func TestChainAdvance_AutoSupersession_PicksSibling(t *testing.T) {
	f := newTaskFixture(t)
	base := f.now

	// plan/work closed=success — the develop predecessor gate.
	_ = f.seedTask(task.ContractAction("plan"), task.KindWork, task.StatusClosed, task.OutcomeSuccess, base)

	// develop/work iter-1 closed=failure (the orphan).
	iter1 := f.seedTask(task.ContractAction("develop"), task.KindWork, task.StatusClosed, task.OutcomeFailure, base.Add(time.Minute))

	// develop/work iter-2 published — mints via TaskAdd to exercise
	// the substrate's auto-supersession (sty_9d046bc7 stamps
	// prior_task_id=iter1). The fixture's developer_agent.Delivers
	// includes contract:develop so the capability check passes.
	add, err := f.c.TaskAdd(context.Background(), f.caller, TaskAddInput{
		AgentID:     f.devID,
		Prompt:      "retry develop",
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		Memberships: f.caller.Memberships,
		Now:         base.Add(2 * time.Minute),
	})
	require.NoError(t, err)

	got, err := f.c.TaskGet(context.Background(), f.caller, TaskGetInput{
		ID:          add.TaskID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	require.Equal(t, iter1.ID, got.PriorTaskID, "substrate must stamp prior_task_id on iter-2 — sty_9d046bc7")

	// ChainAdvance must pick iter-2 even though iter-1 sorts later
	// in the bash helper's "latest row" sense pre-rule §3.2.
	out, err := f.c.ChainAdvance(context.Background(), f.caller, ChainAdvanceInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	assert.Equal(t, add.TaskID, out.NextTaskID, "router must select the published successor, not the failed predecessor")

	// Confirm the selector returned the published row directly.
	status, err := f.c.ChainStatus(context.Background(), f.caller, ChainStatusInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
	})
	require.NoError(t, err)
	var developPhase ChainPhase
	for _, p := range status.Phases {
		if p.Action == task.ContractAction("develop") && p.Kind == task.KindWork {
			developPhase = p
			break
		}
	}
	require.Equal(t, add.TaskID, developPhase.LiveTaskID, "selectLiveTask must return the published row")
	assert.Equal(t, task.StatusPublished, developPhase.LiveStatus)
	assert.Empty(t, status.Anomalies, "the supersession case must NOT report a phase_blocked anomaly")
}

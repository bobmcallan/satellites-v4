package contract

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captured struct {
	topic       string
	kind        string
	workspaceID string
	data        any
}

type recorder struct {
	mu     sync.Mutex
	events []captured
}

func (r *recorder) Publish(_ context.Context, topic, kind, workspaceID string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, captured{topic: topic, kind: kind, workspaceID: workspaceID, data: data})
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.events) }

type panickingPub struct{}

func (panickingPub) Publish(context.Context, string, string, string, any) {
	panic("subscriber exploded")
}

// makeCI returns a fixture + pre-created ContractInstance id in wsA.
func makeCI(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	ci, err := f.contracts.Create(f.ctx, ContractInstance{
		StoryID:      f.parentStory.ID,
		ContractID:   f.contractDoc.ID,
		ContractName: "preplan",
		Status:       StatusReady,
		Sequence:     0,
	}, f.now)
	require.NoError(t, err)
	return f, ci.ID
}

func TestContract_UpdateStatus_Publishes(t *testing.T) {
	f, ciID := makeCI(t)
	rec := &recorder{}
	f.contracts.SetPublisher(rec)

	ci, err := f.contracts.UpdateStatus(f.ctx, ciID, StatusClaimed, "alice", f.now, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())

	got := rec.events[0]
	assert.Equal(t, "ws:"+wsA, got.topic)
	assert.Equal(t, "contract_instance."+StatusClaimed, got.kind)
	assert.Equal(t, wsA, got.workspaceID)
	payload := got.data.(map[string]any)
	assert.Equal(t, ci.ID, payload["ci_id"])
	assert.Equal(t, "preplan", payload["contract_name"])
	assert.Equal(t, f.parentStory.ID, payload["story_id"])
}

func TestContract_Claim_Publishes(t *testing.T) {
	f, ciID := makeCI(t)
	rec := &recorder{}
	f.contracts.SetPublisher(rec)

	_, err := f.contracts.Claim(f.ctx, ciID, "grant-1", f.now, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())
	assert.Equal(t, "contract_instance.claimed", rec.events[0].kind)
}

func TestContract_ClearClaim_Publishes(t *testing.T) {
	f, ciID := makeCI(t)
	_, err := f.contracts.Claim(f.ctx, ciID, "grant-1", f.now, nil)
	require.NoError(t, err)

	rec := &recorder{}
	f.contracts.SetPublisher(rec)
	_, err = f.contracts.ClearClaim(f.ctx, ciID, f.now, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())
	assert.Equal(t, "contract_instance.ready", rec.events[0].kind)
}

func TestContract_InvalidTransition_NoPublish(t *testing.T) {
	f, ciID := makeCI(t)
	rec := &recorder{}
	f.contracts.SetPublisher(rec)
	// ready → passed is invalid; claimed must come first.
	_, err := f.contracts.UpdateStatus(f.ctx, ciID, StatusPassed, "alice", f.now, nil)
	assert.Error(t, err)
	assert.Equal(t, 0, rec.count(), "no publish on failed transition")
}

func TestContract_PanicRecovered(t *testing.T) {
	f, ciID := makeCI(t)
	f.contracts.SetPublisher(panickingPub{})
	_, err := f.contracts.UpdateStatus(f.ctx, ciID, StatusClaimed, "alice", f.now, nil)
	assert.NoError(t, err, "hook panic must not abort the mutation")
}

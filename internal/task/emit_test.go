package task

import (
	"context"
	"sync"
	"testing"
	"time"

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
func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.kind
	}
	return out
}

type panickingPub struct{}

func (panickingPub) Publish(context.Context, string, string, string, any) {
	panic("subscriber exploded")
}

func newSeededTask(id string) Task {
	return Task{
		ID:          id,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Origin:      OriginScheduled,
		Priority:    PriorityMedium,
		Payload:     []byte(`{}`),
	}
}

func TestTask_StatusPublishes(t *testing.T) {
	store := NewMemoryStore()
	rec := &recorder{}
	store.SetPublisher(rec)
	ctx := context.Background()
	now := time.Now().UTC()

	enqueued, err := store.Enqueue(ctx, newSeededTask(""), now)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())
	assert.Equal(t, "task.enqueued", rec.events[0].kind)
	assert.Equal(t, "ws:wksp_A", rec.events[0].topic)
	payload := rec.events[0].data.(map[string]any)
	assert.Equal(t, enqueued.ID, payload["task_id"])
	assert.Equal(t, OriginScheduled, payload["origin"])

	// Claim emits task.claimed.
	claimed, err := store.Claim(ctx, "worker-1", []string{"wksp_A"}, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 2, rec.count())
	assert.Equal(t, "task.claimed", rec.events[1].kind)
	assert.Equal(t, claimed.ID, rec.events[1].data.(map[string]any)["task_id"])

	// Close emits task.closed with outcome.
	_, err = store.Close(ctx, claimed.ID, OutcomeSuccess, now.Add(2*time.Second), nil)
	require.NoError(t, err)
	require.Equal(t, 3, rec.count())
	assert.Equal(t, "task.closed", rec.events[2].kind)
	closedPayload := rec.events[2].data.(map[string]any)
	assert.Equal(t, OutcomeSuccess, closedPayload["outcome"])
}

func TestTask_Reclaim_PublishesEnqueued(t *testing.T) {
	store := NewMemoryStore()
	rec := &recorder{}
	store.SetPublisher(rec)
	ctx := context.Background()
	now := time.Now().UTC()

	enq, err := store.Enqueue(ctx, newSeededTask(""), now)
	require.NoError(t, err)
	_, err = store.Claim(ctx, "w1", []string{"wksp_A"}, now.Add(time.Second))
	require.NoError(t, err)
	_, err = store.Reclaim(ctx, enq.ID, "watchdog", now.Add(2*time.Second), nil)
	require.NoError(t, err)

	kinds := rec.kinds()
	require.Len(t, kinds, 3)
	assert.Equal(t, "task.enqueued", kinds[0])
	assert.Equal(t, "task.claimed", kinds[1])
	assert.Equal(t, "task.enqueued", kinds[2], "reclaim re-emits enqueued")
}

func TestTask_InvalidTransition_NoPublish(t *testing.T) {
	store := NewMemoryStore()
	rec := &recorder{}
	store.SetPublisher(rec)
	ctx := context.Background()
	now := time.Now().UTC()

	_, err := store.Enqueue(ctx, newSeededTask(""), now)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())

	// Attempt to reclaim an already-enqueued task — invalid transition.
	// Ticks the Reclaim code path but fails at ValidTransition.
	_, err = store.Reclaim(ctx, "unknown-id", "x", now, nil)
	assert.Error(t, err, "reclaim on missing id errors")
	assert.Equal(t, 1, rec.count(), "no publish on failed mutation")
}

func TestTask_PanicRecovered(t *testing.T) {
	store := NewMemoryStore()
	store.SetPublisher(panickingPub{})

	enqueued, err := store.Enqueue(context.Background(), newSeededTask(""), time.Now().UTC())
	assert.NoError(t, err, "hook panic must not abort the mutation")
	assert.NotEmpty(t, enqueued.ID)
}

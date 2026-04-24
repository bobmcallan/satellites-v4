package story

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/ledger"
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

func TestStory_UpdateStatus_Publishes(t *testing.T) {
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	rec := &recorder{}
	store.SetPublisher(rec)
	ctx := context.Background()
	now := time.Now().UTC()

	s, err := store.Create(ctx, Story{
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Title:       "parent",
	}, now)
	require.NoError(t, err)
	// Create does not emit — only UpdateStatus. Confirm zero events so far.
	require.Equal(t, 0, rec.count())

	_, err = store.UpdateStatus(ctx, s.ID, StatusReady, "alice", now.Add(time.Second), nil)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())

	got := rec.events[0]
	assert.Equal(t, "ws:wksp_A", got.topic)
	assert.Equal(t, "story.ready", got.kind)
	payload := got.data.(map[string]any)
	assert.Equal(t, s.ID, payload["story_id"])
	assert.Equal(t, "parent", payload["title"])
}

func TestStory_InvalidTransition_NoPublish(t *testing.T) {
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	rec := &recorder{}
	store.SetPublisher(rec)
	ctx := context.Background()
	now := time.Now().UTC()

	s, err := store.Create(ctx, Story{
		WorkspaceID: "wksp_A", ProjectID: "proj_1", Title: "t",
	}, now)
	require.NoError(t, err)

	// backlog → done is invalid.
	_, err = store.UpdateStatus(ctx, s.ID, StatusDone, "alice", now, nil)
	assert.Error(t, err)
	assert.Equal(t, 0, rec.count(), "no publish on failed transition")
}

func TestStory_PanicRecovered(t *testing.T) {
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetPublisher(panickingPub{})
	ctx := context.Background()
	now := time.Now().UTC()

	s, err := store.Create(ctx, Story{
		WorkspaceID: "wksp_A", ProjectID: "proj_1", Title: "t",
	}, now)
	require.NoError(t, err)

	_, err = store.UpdateStatus(ctx, s.ID, StatusReady, "alice", now.Add(time.Second), nil)
	assert.NoError(t, err, "hook panic must not abort the mutation")
}

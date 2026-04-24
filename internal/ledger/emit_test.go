package ledger

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

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recorder) last() captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

type panickingPublisher struct{}

func (panickingPublisher) Publish(context.Context, string, string, string, any) {
	panic("subscriber exploded")
}

func TestLedger_Append_Publishes(t *testing.T) {
	store := NewMemoryStore()
	rec := &recorder{}
	store.SetPublisher(rec)

	entry, err := store.Append(context.Background(), LedgerEntry{
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Type:        TypeEvidence,
		Content:     "body",
		Tags:        []string{"kind:test"},
	}, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())

	got := rec.last()
	assert.Equal(t, "ws:wksp_A", got.topic)
	assert.Equal(t, EventKindAppended, got.kind)
	assert.Equal(t, "wksp_A", got.workspaceID)

	payload, ok := got.data.(map[string]any)
	require.True(t, ok, "payload is map[string]any")
	assert.Equal(t, entry.ID, payload["ledger_id"])
	assert.Equal(t, "wksp_A", payload["workspace_id"])
	assert.Equal(t, "proj_1", payload["project_id"])
	assert.Equal(t, TypeEvidence, payload["type"])
}

func TestLedger_Dereference_Publishes(t *testing.T) {
	store := NewMemoryStore()
	rec := &recorder{}
	store.SetPublisher(rec)
	now := time.Now().UTC()

	target, err := store.Append(context.Background(), LedgerEntry{
		WorkspaceID: "wksp_A", ProjectID: "proj_1", Type: TypeEvidence, Content: "target",
	}, now)
	require.NoError(t, err)
	// one append event so far
	require.Equal(t, 1, rec.count())

	_, err = store.Dereference(context.Background(), target.ID, "obsoleted", "alice", now, nil)
	require.NoError(t, err)

	// Expect two additional events: the audit-row Append, then the Dereference.
	require.Eventually(t, func() bool { return rec.count() >= 3 }, time.Second, 10*time.Millisecond)

	events := rec.events
	// The final event must be the Dereference.
	var deref *captured
	for i := range events {
		if events[i].kind == EventKindDereferenced {
			e := events[i]
			deref = &e
		}
	}
	require.NotNil(t, deref, "dereference event recorded")
	assert.Equal(t, "ws:wksp_A", deref.topic)
	payload := deref.data.(map[string]any)
	assert.Equal(t, target.ID, payload["ledger_id"])
	assert.Equal(t, "obsoleted", payload["reason"])
}

func TestLedger_Append_PanicRecovered(t *testing.T) {
	store := NewMemoryStore()
	store.SetPublisher(panickingPublisher{})

	entry, err := store.Append(context.Background(), LedgerEntry{
		WorkspaceID: "wksp_A", ProjectID: "p", Type: TypeEvidence, Content: "x",
	}, time.Now().UTC())
	assert.NoError(t, err, "hook panic must not abort the mutation")
	assert.NotEmpty(t, entry.ID)
	// Row must be readable afterwards — mutation actually happened.
	got, err := store.GetByID(context.Background(), entry.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, entry.ID, got.ID)
}

func TestLedger_Emit_NoWorkspaceID_Skips(t *testing.T) {
	// Entries without WorkspaceID (e.g. system rows written before workspace
	// scoping was introduced) must not publish — there is no valid topic.
	store := NewMemoryStore()
	rec := &recorder{}
	store.SetPublisher(rec)

	_, err := store.Append(context.Background(), LedgerEntry{
		WorkspaceID: "", ProjectID: "p", Type: TypeEvidence, Content: "x",
	}, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 0, rec.count(), "no publish for missing workspace_id")
}

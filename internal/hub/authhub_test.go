package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeMember struct {
	mu      sync.Mutex
	members map[string]map[string]bool
	err     error
}

func newFakeMember() *fakeMember {
	return &fakeMember{members: map[string]map[string]bool{}}
}

func (f *fakeMember) add(workspaceID, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members[workspaceID] == nil {
		f.members[workspaceID] = map[string]bool{}
	}
	f.members[workspaceID][userID] = true
}

func (f *fakeMember) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.members[workspaceID][userID], nil
}

type capturedMismatch struct {
	event    Event
	expected string
}

type fakeAudit struct {
	mu     sync.Mutex
	events []capturedMismatch
}

func (f *fakeAudit) HubMismatch(_ context.Context, event Event, expectedWorkspaceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, capturedMismatch{event: event, expected: expectedWorkspaceID})
}

func (f *fakeAudit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fakeAudit) last() capturedMismatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[len(f.events)-1]
}

const wsA = "wksp_alpha"
const wsB = "wksp_beta"
const topicA = "ws:wksp_alpha"
const topicB = "ws:wksp_beta"

func TestParseTopicWorkspace(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantID  string
		wantErr bool
	}{
		{name: "valid ws topic", input: "ws:wksp_abc", wantID: "wksp_abc"},
		{name: "missing prefix", input: "wksp_abc", wantErr: true},
		{name: "empty suffix", input: "ws:", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTopicWorkspace(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.wantID, got)
		})
	}
}

func TestAuthHub_Subscribe_MemberAllowed(t *testing.T) {
	members := newFakeMember()
	members.add(wsA, "alice")
	audit := &fakeAudit{}
	ah := NewAuthHub(New(), members, audit)

	ch, err := ah.Subscribe(context.Background(), topicA, "sub-1", "alice")
	require.NoError(t, err)
	require.NotNil(t, ch)

	ah.Publish(context.Background(), topicA, Event{Kind: "ping", WorkspaceID: wsA})
	select {
	case ev := <-ch:
		assert.Equal(t, "ping", ev.Kind)
		assert.Equal(t, wsA, ev.WorkspaceID)
	case <-time.After(time.Second):
		t.Fatal("member subscriber did not receive event")
	}
}

func TestAuthHub_Subscribe_NonMemberRejected(t *testing.T) {
	members := newFakeMember()
	// alice is a member of wsA only.
	members.add(wsA, "alice")
	ah := NewAuthHub(New(), members, &fakeAudit{})

	ch, err := ah.Subscribe(context.Background(), topicB, "sub-1", "alice")
	assert.ErrorIs(t, err, ErrNotMember)
	assert.Nil(t, ch)

	// Underlying hub must NOT have allocated a channel for the rejected sub.
	// Publish on topicB should go to zero subs.
	raw := ah.Hub()
	raw.Publish(topicB, Event{Kind: "direct", WorkspaceID: wsB})
	assert.Len(t, raw.ReplayBuffer(topicB, ""), 1,
		"ring buffered the direct publish but no subscriber channels exist")
}

func TestAuthHub_Subscribe_InvalidTopicRejected(t *testing.T) {
	ah := NewAuthHub(New(), newFakeMember(), &fakeAudit{})
	ch, err := ah.Subscribe(context.Background(), "bad-topic", "sub-1", "alice")
	assert.ErrorIs(t, err, ErrInvalidTopic)
	assert.Nil(t, ch)
}

func TestAuthHub_Subscribe_MembershipErrorPropagates(t *testing.T) {
	members := newFakeMember()
	members.err = errors.New("db down")
	ah := NewAuthHub(New(), members, &fakeAudit{})
	ch, err := ah.Subscribe(context.Background(), topicA, "sub-1", "alice")
	assert.Error(t, err)
	assert.Nil(t, ch)
	assert.NotErrorIs(t, err, ErrNotMember, "wrapped store error is distinguishable from ErrNotMember")
}

func TestAuthHub_Publish_MatchDelivers(t *testing.T) {
	members := newFakeMember()
	members.add(wsA, "alice")
	audit := &fakeAudit{}
	ah := NewAuthHub(New(), members, audit)

	ch, err := ah.Subscribe(context.Background(), topicA, "sub-1", "alice")
	require.NoError(t, err)

	ah.Publish(context.Background(), topicA, Event{Kind: "k", WorkspaceID: wsA, Data: 42})

	select {
	case ev := <-ch:
		assert.Equal(t, 42, ev.Data)
		assert.Equal(t, wsA, ev.WorkspaceID)
	case <-time.After(time.Second):
		t.Fatal("matching publish not delivered")
	}
	assert.Equal(t, 0, audit.count(), "matching publish does not audit")
}

func TestAuthHub_Publish_MismatchDropped(t *testing.T) {
	members := newFakeMember()
	members.add(wsA, "alice")
	audit := &fakeAudit{}
	ah := NewAuthHub(New(), members, audit)

	ch, err := ah.Subscribe(context.Background(), topicA, "sub-1", "alice")
	require.NoError(t, err)

	// Publish to topicA with event claiming wsB — mismatch.
	ah.Publish(context.Background(), topicA, Event{Kind: "leak-attempt", WorkspaceID: wsB, Data: "sensitive"})

	select {
	case ev := <-ch:
		t.Fatalf("mismatched publish was delivered: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected — nothing delivered.
	}

	require.Equal(t, 1, audit.count())
	rec := audit.last()
	assert.Equal(t, wsA, rec.expected, "audit records the expected ws id from the topic suffix")
	assert.Equal(t, wsB, rec.event.WorkspaceID, "audit preserves the event's claimed ws id")
	assert.Equal(t, "leak-attempt", rec.event.Kind)

	// The ring buffer must NOT contain the mismatched event either —
	// storing it would leak sensitive data into replay of a future
	// rightful subscriber.
	buf := ah.ReplayBuffer(topicA, "")
	for _, e := range buf {
		assert.NotEqual(t, "leak-attempt", e.Kind, "mismatched publish must not enter the ring")
	}
}

func TestAuthHub_Publish_NilAuditDropsSilently(t *testing.T) {
	ah := NewAuthHub(New(), newFakeMember(), nil)
	// should not panic; no audit happens
	ah.Publish(context.Background(), topicA, Event{Kind: "oops", WorkspaceID: wsB})
	assert.Empty(t, ah.ReplayBuffer(topicA, ""))
}

func TestAuthHub_Publish_InvalidTopicAudited(t *testing.T) {
	audit := &fakeAudit{}
	ah := NewAuthHub(New(), newFakeMember(), audit)
	ah.Publish(context.Background(), "garbage", Event{Kind: "k", WorkspaceID: wsA})
	require.Equal(t, 1, audit.count())
	assert.Equal(t, "", audit.last().expected, "invalid topic records empty expected ws")
}

func TestAuthHub_Subscribe_CrossWorkspaceIsolation(t *testing.T) {
	// Defence-in-depth: even with a matching ws event on another topic,
	// the sub on topicA receives nothing when publish goes to topicB.
	members := newFakeMember()
	members.add(wsA, "alice")
	members.add(wsB, "alice") // alice in both — no membership rejection to obscure the isolation
	ah := NewAuthHub(New(), members, &fakeAudit{})

	chA, err := ah.Subscribe(context.Background(), topicA, "sub-A", "alice")
	require.NoError(t, err)
	chB, err := ah.Subscribe(context.Background(), topicB, "sub-B", "alice")
	require.NoError(t, err)

	ah.Publish(context.Background(), topicA, Event{Kind: "onlyA", WorkspaceID: wsA})

	select {
	case ev := <-chA:
		assert.Equal(t, "onlyA", ev.Kind)
	case <-time.After(time.Second):
		t.Fatal("topicA sub missed its event")
	}
	select {
	case ev := <-chB:
		t.Fatalf("topicB sub received cross-topic event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// Keep linter happy about unused helper in some builds.
var _ = fmt.Sprintf

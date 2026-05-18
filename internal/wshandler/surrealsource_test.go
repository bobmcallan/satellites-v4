// Targeted coverage for the project_id filter at the subscription
// gate (sty_fbcde932). The four cases come straight from the story's
// acceptance criteria: match → delivered, mismatch → dropped, empty
// → all-workspace fall-through, and missing project_id key →
// delivered when projectID is empty (backward compat).
package wshandler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type membershipFunc func(ctx context.Context, workspaceID, userID string) (bool, error)

func (f membershipFunc) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	return f(ctx, workspaceID, userID)
}

func TestSurrealLiveSource_ProjectFilter(t *testing.T) {
	const (
		topic  = "ws:wksp_A"
		userID = "u_alice"
		projA  = "proj_A"
		projB  = "proj_B"
	)
	members := membershipFunc(func(_ context.Context, _, _ string) (bool, error) {
		return true, nil
	})

	t.Run("match -> delivered", func(t *testing.T) {
		s := NewSurrealLiveSource(nil, members, nil)
		ch, err := s.Subscribe(context.Background(), topic, "sub-1", userID, projA)
		require.NoError(t, err)
		s.fanout([]WireEvent{{
			ID: "ev-1", Topic: topic, Kind: "ledger.append",
			WorkspaceID: "wksp_A",
			Data:        map[string]any{"project_id": projA},
		}})
		select {
		case got := <-ch:
			assert.Equal(t, "ev-1", got.ID)
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected event with matching project_id to be delivered")
		}
	})

	t.Run("mismatch -> dropped", func(t *testing.T) {
		s := NewSurrealLiveSource(nil, members, nil)
		ch, err := s.Subscribe(context.Background(), topic, "sub-2", userID, projA)
		require.NoError(t, err)
		s.fanout([]WireEvent{{
			ID: "ev-2", Topic: topic, Kind: "ledger.append",
			WorkspaceID: "wksp_A",
			Data:        map[string]any{"project_id": projB},
		}})
		select {
		case got := <-ch:
			t.Fatalf("expected mismatched event to be dropped, got %+v", got)
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("empty projectID -> all-workspace fall-through", func(t *testing.T) {
		s := NewSurrealLiveSource(nil, members, nil)
		ch, err := s.Subscribe(context.Background(), topic, "sub-3", userID, "")
		require.NoError(t, err)
		s.fanout([]WireEvent{
			{
				ID: "ev-3a", Topic: topic, Kind: "ledger.append",
				WorkspaceID: "wksp_A",
				Data:        map[string]any{"project_id": projA},
			},
			{
				ID: "ev-3b", Topic: topic, Kind: "ledger.append",
				WorkspaceID: "wksp_A",
				Data:        map[string]any{"project_id": projB},
			},
		})
		var ids []string
		for i := 0; i < 2; i++ {
			select {
			case got := <-ch:
				ids = append(ids, got.ID)
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("expected both events on empty-projectID fall-through, got %v", ids)
			}
		}
		assert.ElementsMatch(t, []string{"ev-3a", "ev-3b"}, ids)
	})

	t.Run("missing project_id key -> delivered when projectID empty", func(t *testing.T) {
		s := NewSurrealLiveSource(nil, members, nil)
		ch, err := s.Subscribe(context.Background(), topic, "sub-4", userID, "")
		require.NoError(t, err)
		s.fanout([]WireEvent{{
			ID: "ev-4", Topic: topic, Kind: "ledger.append",
			WorkspaceID: "wksp_A",
			Data:        map[string]any{"task_id": "t_1"},
		}})
		select {
		case got := <-ch:
			assert.Equal(t, "ev-4", got.ID)
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected event lacking project_id key to be delivered for empty-projectID subscriber")
		}
	})
}

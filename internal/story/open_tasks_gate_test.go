package story

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// TestMemoryStore_OpenTasksGate_RejectsTerminalWhilePublished asserts
// the gate fires for status=published tasks. Sty_0233fabd.
func TestMemoryStore_OpenTasksGate_RejectsTerminalWhilePublished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return []string{"task_pub1"}, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	_, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil)
	if !errors.Is(err, ErrStoryHasOpenTasks) {
		t.Fatalf("want ErrStoryHasOpenTasks, got %v", err)
	}
	var openErr *StoryHasOpenTasksError
	if !errors.As(err, &openErr) {
		t.Fatalf("error must unwrap to *StoryHasOpenTasksError, got %T", err)
	}
	if openErr.StoryID != s.ID {
		t.Errorf("StoryID = %q, want %q", openErr.StoryID, s.ID)
	}
	if len(openErr.OpenTaskIDs) != 1 || openErr.OpenTaskIDs[0] != "task_pub1" {
		t.Errorf("OpenTaskIDs = %v, want [task_pub1]", openErr.OpenTaskIDs)
	}
	// Status must remain unchanged.
	got, _ := store.GetByID(ctx, s.ID, nil)
	if got.Status != StatusBacklog {
		t.Errorf("status leaked through gate: got %q, want backlog", got.Status)
	}
}

// TestMemoryStore_OpenTasksGate_RejectsCancelledWhilePlanned asserts
// the gate also covers status=planned tasks (subscriber-invisible but
// still claimable future work) AND the cancelled target.
func TestMemoryStore_OpenTasksGate_RejectsCancelledWhilePlanned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return []string{"task_planned1"}, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	_, err := store.UpdateStatus(ctx, s.ID, StatusCancelled, "u_alice", now.Add(time.Second), nil)
	if !errors.Is(err, ErrStoryHasOpenTasks) {
		t.Fatalf("want ErrStoryHasOpenTasks, got %v", err)
	}
}

// TestMemoryStore_OpenTasksGate_AllowsAfterClose asserts the gate
// permits the transition once the lookup returns no open ids — the
// regression check for sty_01f75142's zombie state reconciliation.
func TestMemoryStore_OpenTasksGate_AllowsAfterClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)

	open := []string{"task_pub1"}
	store.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return open, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	if _, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil); !errors.Is(err, ErrStoryHasOpenTasks) {
		t.Fatalf("first attempt: want ErrStoryHasOpenTasks, got %v", err)
	}
	// Close the open task — gate now lets the transition through.
	open = nil
	got, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(2*time.Second), nil)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if got.Status != StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
}

// TestMemoryStore_OpenTasksGate_NonTerminalNotGated asserts the gate
// only guards the two terminal targets. ready / in_progress / blocked
// transitions are unaffected.
func TestMemoryStore_OpenTasksGate_NonTerminalNotGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		t.Fatal("openTasksFn must not be called for non-terminal targets")
		return nil, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	for _, tgt := range []string{StatusReady, StatusInProgress, StatusBlocked} {
		fresh, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: tgt}, now)
		if _, err := store.UpdateStatus(ctx, fresh.ID, tgt, "u_alice", now.Add(time.Second), nil); err != nil {
			t.Errorf("backlog → %s should pass: %v", tgt, err)
		}
	}
	_ = s
}

// TestMemoryStore_OpenTasksGate_DerivedBypassesGate asserts
// UpdateStatusDerived ignores the gate — derivation-driven flips
// (storystatus reconciler) must complete even if planned/published
// tasks remain. The reconciler's input is itself a closed-task event,
// so the rows it observes are necessarily consistent with the target.
func TestMemoryStore_OpenTasksGate_DerivedBypassesGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		t.Fatal("openTasksFn must not be called by UpdateStatusDerived")
		return nil, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	got, err := store.UpdateStatusDerived(ctx, s.ID, StatusDone, now.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("UpdateStatusDerived: %v", err)
	}
	if got.Status != StatusDone {
		t.Errorf("derived status = %q, want done", got.Status)
	}
}

// TestMemoryStore_OpenTasksGate_NilFnSkipsGate asserts the gate is a
// no-op when the function isn't wired. Boot ordering (storyStore is
// constructible before the task store) and unit tests rely on this.
func TestMemoryStore_OpenTasksGate_NilFnSkipsGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	got, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("UpdateStatus with nil gate: %v", err)
	}
	if got.Status != StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
}

// TestMemoryStore_OpenTasksGate_LookupErrorPropagates asserts the gate
// returns the lookup error verbatim — silent skip would mask data-store
// failures.
func TestMemoryStore_OpenTasksGate_LookupErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	sentinel := errors.New("task store down")
	store.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return nil, sentinel
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	_, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("lookup error must propagate, got %v", err)
	}
}

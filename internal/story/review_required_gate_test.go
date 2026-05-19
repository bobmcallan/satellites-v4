package story

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// TestMemoryStore_ReviewRequiredGate_RejectsTerminalWhenReviewMissing
// covers the no-review-pair branch: a closed develop work task on the
// chain with no paired review task → done is rejected with the
// offending work task id in the error payload. Sty_f49c378d.
func TestMemoryStore_ReviewRequiredGate_RejectsTerminalWhenReviewMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetReviewRequiredFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return []string{"task_develop1"}, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	_, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil)
	if !errors.Is(err, ErrReviewRequiredGateMissing) {
		t.Fatalf("want ErrReviewRequiredGateMissing, got %v", err)
	}
	var gateErr *ReviewRequiredGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error must unwrap to *ReviewRequiredGateError, got %T", err)
	}
	if gateErr.StoryID != s.ID {
		t.Errorf("StoryID = %q, want %q", gateErr.StoryID, s.ID)
	}
	if len(gateErr.WorkTaskIDsMissingPass) != 1 || gateErr.WorkTaskIDsMissingPass[0] != "task_develop1" {
		t.Errorf("WorkTaskIDsMissingPass = %v, want [task_develop1]", gateErr.WorkTaskIDsMissingPass)
	}
	got, _ := store.GetByID(ctx, s.ID, nil)
	if got.Status != StatusBacklog {
		t.Errorf("status leaked through gate: got %q, want backlog", got.Status)
	}
}

// TestMemoryStore_ReviewRequiredGate_RejectsTerminalWhenVerdictFail
// covers the fail-verdict branch: review task exists and closed-success
// but the ledger row carries verdict:fail (no verdict:pass row) — the
// gate still rejects. The closure-side filter is what enforces the
// pass/fail distinction; this test pins the contract that the gate's
// caller (here the test's closure) returns the work task id when no
// matching verdict:pass row exists.
func TestMemoryStore_ReviewRequiredGate_RejectsTerminalWhenVerdictFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetReviewRequiredFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return []string{"task_develop_fail"}, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	_, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil)
	if !errors.Is(err, ErrReviewRequiredGateMissing) {
		t.Fatalf("want ErrReviewRequiredGateMissing, got %v", err)
	}
	var gateErr *ReviewRequiredGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error must unwrap to *ReviewRequiredGateError, got %T", err)
	}
	if len(gateErr.WorkTaskIDsMissingPass) != 1 || gateErr.WorkTaskIDsMissingPass[0] != "task_develop_fail" {
		t.Errorf("WorkTaskIDsMissingPass = %v, want [task_develop_fail]", gateErr.WorkTaskIDsMissingPass)
	}
}

// TestMemoryStore_ReviewRequiredGate_AllowsTerminalWhenVerdictPass
// covers the verdict:pass branch: the closure returns an empty slice
// (every required review carries a verdict:pass row) → the transition
// is accepted. This is the canonical green-path.
func TestMemoryStore_ReviewRequiredGate_AllowsTerminalWhenVerdictPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	missing := []string{"task_develop_pending"}
	store.SetReviewRequiredFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return missing, nil
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	if _, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil); !errors.Is(err, ErrReviewRequiredGateMissing) {
		t.Fatalf("first attempt: want ErrReviewRequiredGateMissing, got %v", err)
	}
	// Verdict pass arrives — the closure now returns empty.
	missing = nil
	got, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(2*time.Second), nil)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if got.Status != StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
}

// TestMemoryStore_ReviewRequiredGate_NonTerminalNotGated asserts the
// gate only guards the two terminal targets. ready / in_progress /
// blocked transitions are unaffected.
func TestMemoryStore_ReviewRequiredGate_NonTerminalNotGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetReviewRequiredFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		t.Fatal("reviewRequiredFn must not be called for non-terminal targets")
		return nil, nil
	})
	now := time.Now().UTC()
	for _, tgt := range []string{StatusReady, StatusInProgress, StatusBlocked} {
		fresh, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: tgt}, now)
		if _, err := store.UpdateStatus(ctx, fresh.ID, tgt, "u_alice", now.Add(time.Second), nil); err != nil {
			t.Errorf("backlog → %s should pass: %v", tgt, err)
		}
	}
}

// TestMemoryStore_ReviewRequiredGate_DerivedBypassesGate asserts AC4 —
// UpdateStatusDerived (storystatus reconciler path) ignores the gate.
// Derivation-driven flips are themselves consequences of an observed
// terminal task event, so by construction the chain is consistent with
// the target.
func TestMemoryStore_ReviewRequiredGate_DerivedBypassesGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	store.SetReviewRequiredFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		t.Fatal("reviewRequiredFn must not be called by UpdateStatusDerived")
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

// TestMemoryStore_ReviewRequiredGate_NilFnSkipsGate asserts the gate
// is a no-op when the function isn't wired. Boot ordering (storyStore
// is constructible before the task / document / ledger stores are
// available) and unit tests rely on this.
func TestMemoryStore_ReviewRequiredGate_NilFnSkipsGate(t *testing.T) {
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

// TestMemoryStore_ReviewRequiredGate_LookupErrorPropagates asserts the
// gate returns the lookup error verbatim — silent skip would mask
// data-store failures.
func TestMemoryStore_ReviewRequiredGate_LookupErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	sentinel := errors.New("task store down")
	store.SetReviewRequiredFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return nil, sentinel
	})
	now := time.Now().UTC()
	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	_, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now.Add(time.Second), nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("lookup error must propagate, got %v", err)
	}
}

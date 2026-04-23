package story

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// erroringLedger stubs ledger.Store so UpdateStatus tests can exercise the
// rollback path. Calls counts rows attempted; errOn is the 1-based index
// at which Append returns an error. 0 = never.
type erroringLedger struct {
	calls   int
	errOn   int
	backing ledger.Store
}

func (e *erroringLedger) Append(ctx context.Context, entry ledger.LedgerEntry, now time.Time) (ledger.LedgerEntry, error) {
	e.calls++
	if e.errOn != 0 && e.calls == e.errOn {
		return ledger.LedgerEntry{}, errors.New("stub: forced failure")
	}
	if e.backing == nil {
		entry.ID = "stub_" + entry.Type
		entry.CreatedAt = now
		return entry, nil
	}
	return e.backing.Append(ctx, entry, now)
}

func (e *erroringLedger) List(ctx context.Context, projectID string, opts ledger.ListOptions, memberships []string) ([]ledger.LedgerEntry, error) {
	if e.backing == nil {
		return []ledger.LedgerEntry{}, nil
	}
	return e.backing.List(ctx, projectID, opts, memberships)
}

func (e *erroringLedger) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error) {
	if e.backing == nil {
		return 0, nil
	}
	return e.backing.BackfillWorkspaceID(ctx, projectID, workspaceID)
}

func TestNewID_Format(t *testing.T) {
	t.Parallel()
	id := NewID()
	if !strings.HasPrefix(id, "sty_") || len(id) != len("sty_")+8 {
		t.Errorf("id %q has wrong shape", id)
	}
	if NewID() == id {
		t.Error("NewID must mint unique ids")
	}
}

func TestValidTransition_Matrix(t *testing.T) {
	t.Parallel()
	allowed := map[[2]string]bool{
		{StatusBacklog, StatusReady}:         true,
		{StatusBacklog, StatusCancelled}:     true,
		{StatusReady, StatusInProgress}:      true,
		{StatusReady, StatusCancelled}:       true,
		{StatusInProgress, StatusDone}:       true,
		{StatusInProgress, StatusCancelled}:  true,
	}
	all := []string{StatusBacklog, StatusReady, StatusInProgress, StatusDone, StatusCancelled}
	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]string{from, to}]
			got := ValidTransition(from, to)
			if got != want {
				t.Errorf("ValidTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	// Self-transitions rejected.
	for _, s := range all {
		if ValidTransition(s, s) {
			t.Errorf("self-transition %q must be rejected", s)
		}
	}
	// Unknown states rejected.
	if ValidTransition("unknown", StatusReady) {
		t.Error("unknown source must be rejected")
	}
}

func TestMemoryStore_PanicsOnNilLedger(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewMemoryStore(nil) must panic (ledger required)")
		}
	}()
	NewMemoryStore(nil)
}

func TestMemoryStore_CreateAndGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(ledger.NewMemoryStore())
	now := time.Now().UTC()

	s, err := store.Create(ctx, Story{ProjectID: "proj_a", Title: "hello"}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(s.ID, "sty_") {
		t.Errorf("id = %q", s.ID)
	}
	if s.Status != StatusBacklog {
		t.Errorf("default status = %q, want backlog", s.Status)
	}
	if s.Tags == nil {
		t.Error("tags should be non-nil slice")
	}
	got, err := store.GetByID(ctx, s.ID, nil)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Title != "hello" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestMemoryStore_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(ledger.NewMemoryStore())
	if _, err := store.GetByID(ctx, "sty_missing", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_Create_UnknownStatusRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(ledger.NewMemoryStore())
	_, err := store.Create(ctx, Story{ProjectID: "proj_a", Status: "garbage"}, time.Now())
	if err == nil {
		t.Error("expected rejection of unknown status")
	}
}

func TestMemoryStore_ListFiltersAndOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(ledger.NewMemoryStore())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	older, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "older", Priority: "low", Tags: []string{"epic:v4-stories"}}, t0)
	newer, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "newer", Priority: "high", Tags: []string{"epic:v4-stories", "portal"}}, t0.Add(time.Hour))
	_, _ = store.Create(ctx, Story{ProjectID: "proj_b", Title: "other-proj", Priority: "high"}, t0.Add(2*time.Hour))

	// Project isolation + newest first.
	list, _ := store.List(ctx, "proj_a", ListOptions{}, nil)
	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Errorf("list order/isolation wrong: %v", list)
	}

	// Priority filter.
	high, _ := store.List(ctx, "proj_a", ListOptions{Priority: "high"}, nil)
	if len(high) != 1 || high[0].ID != newer.ID {
		t.Errorf("priority filter: %v", high)
	}

	// Tag filter.
	portal, _ := store.List(ctx, "proj_a", ListOptions{Tag: "portal"}, nil)
	if len(portal) != 1 || portal[0].ID != newer.ID {
		t.Errorf("tag filter: %v", portal)
	}

	// Status filter.
	backlog, _ := store.List(ctx, "proj_a", ListOptions{Status: StatusBacklog}, nil)
	if len(backlog) != 2 {
		t.Errorf("status filter: want 2 backlog, got %d", len(backlog))
	}
}

func TestMemoryStore_UpdateStatus_EmitsLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	now := time.Now().UTC()

	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	// backlog → ready → in_progress → done at strictly increasing times.
	for i, next := range []string{StatusReady, StatusInProgress, StatusDone} {
		var err error
		s, err = store.UpdateStatus(ctx, s.ID, next, "u_alice", now.Add(time.Duration(i+1)*time.Second), nil)
		if err != nil {
			t.Fatalf("UpdateStatus %q: %v", next, err)
		}
	}
	if s.Status != StatusDone {
		t.Errorf("final status = %q, want done", s.Status)
	}

	entries, _ := led.List(ctx, "proj_a", ledger.ListOptions{Type: LedgerEntryType}, nil)
	if len(entries) != 3 {
		t.Fatalf("ledger rows: got %d, want 3", len(entries))
	}
	// Entries are newest-first; the oldest (at len-1) is the first transition.
	oldest := entries[len(entries)-1]
	var p transitionPayload
	if err := json.Unmarshal([]byte(oldest.Content), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.StoryID != s.ID || p.From != StatusBacklog || p.To != StatusReady || p.Actor != "u_alice" {
		t.Errorf("first transition payload wrong: %+v", p)
	}
	// Newest (at 0) is the last transition.
	newest := entries[0]
	var p2 transitionPayload
	if err := json.Unmarshal([]byte(newest.Content), &p2); err != nil {
		t.Fatalf("decode newest payload: %v", err)
	}
	if p2.From != StatusInProgress || p2.To != StatusDone {
		t.Errorf("last transition payload wrong: %+v", p2)
	}
}

func TestMemoryStore_UpdateStatus_RejectsInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	store := NewMemoryStore(led)
	now := time.Now().UTC()

	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)

	// backlog → done is invalid (must pass through ready + in_progress).
	if _, err := store.UpdateStatus(ctx, s.ID, StatusDone, "u_alice", now, nil); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("want ErrInvalidTransition, got %v", err)
	}
	entries, _ := led.List(ctx, "proj_a", ledger.ListOptions{}, nil)
	if len(entries) != 0 {
		t.Errorf("ledger must not be written on invalid transition; got %d entries", len(entries))
	}
}

func TestMemoryStore_UpdateStatus_RollbackOnLedgerError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led := &erroringLedger{errOn: 1}
	store := NewMemoryStore(led)
	now := time.Now().UTC()

	s, _ := store.Create(ctx, Story{ProjectID: "proj_a", Title: "x"}, now)
	if s.Status != StatusBacklog {
		t.Fatalf("precondition: status = %q, want backlog", s.Status)
	}

	_, err := store.UpdateStatus(ctx, s.ID, StatusReady, "u_alice", now, nil)
	if err == nil {
		t.Fatal("expected rollback error")
	}

	// Status must be reverted to backlog.
	after, _ := store.GetByID(ctx, s.ID, nil)
	if after.Status != StatusBacklog {
		t.Errorf("status not reverted: got %q, want backlog", after.Status)
	}
}

func TestMemoryStore_UpdateStatus_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(ledger.NewMemoryStore())
	if _, err := store.UpdateStatus(ctx, "sty_missing", StatusReady, "u_alice", time.Now(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

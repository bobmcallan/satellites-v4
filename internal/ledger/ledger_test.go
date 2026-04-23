package ledger

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewID_Format(t *testing.T) {
	t.Parallel()
	id := NewID()
	if !strings.HasPrefix(id, "ldg_") || len(id) != len("ldg_")+8 {
		t.Errorf("id %q has wrong shape", id)
	}
	if NewID() == id {
		t.Error("NewID must mint unique ids")
	}
}

// TestStoreInterface_AppendOnly pins the Store surface: a change that adds
// Update/Delete/GetByID to the interface would fail this test (via the
// reflect walk) and the compile-time `var _ Store = ...` assertion in
// store.go / surreal.go.
//
// BackfillWorkspaceID is allow-listed — it only stamps workspace_id on
// rows where it was empty and is scoped to the feature-order:2 migration.
func TestStoreInterface_AppendOnly(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"Append": true, "List": true, "BackfillWorkspaceID": true}
	typ := reflect.TypeOf((*Store)(nil)).Elem()
	if typ.NumMethod() != len(want) {
		t.Fatalf("Store declares %d methods; want exactly %d (%v)", typ.NumMethod(), len(want), want)
	}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i).Name
		if !want[m] {
			t.Errorf("unexpected method on Store: %q (append-only interface: Append + List + BackfillWorkspaceID)", m)
		}
	}
}

func TestMemoryStore_AppendStampsIDAndTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	e, err := store.Append(ctx, LedgerEntry{
		ProjectID: "proj_a",
		Type:      "story.created",
		Content:   "hello",
		Actor:     "u_1",
	}, now)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !strings.HasPrefix(e.ID, "ldg_") {
		t.Errorf("id %q not stamped", e.ID)
	}
	if !e.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", e.CreatedAt, now)
	}
	if e.ProjectID != "proj_a" || e.Type != "story.created" || e.Content != "hello" || e.Actor != "u_1" {
		t.Errorf("fields round-trip mismatch: %+v", e)
	}
}

func TestMemoryStore_ListNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "a", Actor: "u_1"}, t0)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "b", Actor: "u_1"}, t0.Add(time.Hour))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "c", Actor: "u_1"}, t0.Add(2*time.Hour))

	got, err := store.List(ctx, "proj_a", ListOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Type != "c" || got[1].Type != "b" || got[2].Type != "a" {
		t.Errorf("unexpected order: %v", []string{got[0].Type, got[1].Type, got[2].Type})
	}
}

func TestMemoryStore_ListTypeFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "story.status_change"}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "story.status_change"}, now.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "other.event"}, now.Add(2*time.Second))

	got, _ := store.List(ctx, "proj_a", ListOptions{Type: "story.status_change"}, nil)
	if len(got) != 2 {
		t.Errorf("type filter returned %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Type != "story.status_change" {
			t.Errorf("leaked %q", e.Type)
		}
	}
}

func TestMemoryStore_ListLimitClamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	for i := 0; i < 600; i++ {
		_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "t"}, now.Add(time.Duration(i)*time.Microsecond))
	}

	// Default (0) clamps up to 100.
	got, _ := store.List(ctx, "proj_a", ListOptions{}, nil)
	if len(got) != DefaultListLimit {
		t.Errorf("default limit returned %d, want %d", len(got), DefaultListLimit)
	}

	// Explicit 2 returns 2 newest-first.
	got, _ = store.List(ctx, "proj_a", ListOptions{Limit: 2}, nil)
	if len(got) != 2 {
		t.Errorf("limit 2 returned %d, want 2", len(got))
	}

	// Above ceiling clamps down.
	got, _ = store.List(ctx, "proj_a", ListOptions{Limit: 9999}, nil)
	if len(got) != MaxListLimit {
		t.Errorf("ceiling clamp returned %d, want %d", len(got), MaxListLimit)
	}
}

func TestMemoryStore_ProjectIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "x"}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_b", Type: "x"}, now)

	a, _ := store.List(ctx, "proj_a", ListOptions{}, nil)
	b, _ := store.List(ctx, "proj_b", ListOptions{}, nil)
	c, _ := store.List(ctx, "proj_missing", ListOptions{}, nil)
	if len(a) != 1 || len(b) != 1 {
		t.Errorf("per-project counts wrong: a=%d b=%d", len(a), len(b))
	}
	if len(c) != 0 {
		t.Errorf("missing project should return empty, got %d", len(c))
	}
}

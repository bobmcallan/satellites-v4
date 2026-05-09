// sty_2ba48616 — exercise the bus-parity verifier's correlation
// rules against an in-process ledger.MemoryStore. Coverage:
//
//   - Two observations on the same (table, row_id) from different
//     buses are matched and bump the matched count, no ledger row.
//   - A single hub observation that ages past ExpiryWindow without
//     a live peer becomes a hub_only mismatch ledger row.
//   - A single live observation that ages out becomes a live_only
//     mismatch ledger row.
//   - Same-bus reassertions update the timestamp without matching.
//   - EmitStats writes a kind:bus-parity-stats row carrying the
//     window's totals and resets them.
package busparity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/busparity"
	"github.com/bobmcallan/satellites/internal/ledger"
)

func newFixture(t *testing.T, window time.Duration) (*busparity.Verifier, *ledger.MemoryStore) {
	t.Helper()
	store := ledger.NewMemoryStore()
	v := busparity.New(store, busparity.Config{
		ExpiryWindow:  window,
		StatsInterval: window,
		ProjectID:     "proj_test",
	}, nil)
	return v, store
}

func countTagged(t *testing.T, store *ledger.MemoryStore, tag string) int {
	t.Helper()
	rows, err := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{tag}, Limit: 100}}, nil)
	if err != nil {
		t.Fatalf("search %s: %v", tag, err)
	}
	return len(rows)
}

func TestVerifier_MatchedPair(t *testing.T) {
	t.Parallel()
	v, store := newFixture(t, 100*time.Millisecond)

	now := time.Now().UTC()
	v.Observe(busparity.Observation{Source: busparity.SourceHub, Table: "tasks", RowID: "task_1", WorkspaceID: "wksp", SeenAt: now})
	v.Observe(busparity.Observation{Source: busparity.SourceLive, Table: "tasks", RowID: "task_1", WorkspaceID: "wksp", SeenAt: now})

	v.Sweep(context.Background(), now.Add(50*time.Millisecond))
	v.EmitStats(context.Background(), now.Add(50*time.Millisecond))

	if got := countTagged(t, store, "kind:bus-parity-mismatch"); got != 0 {
		t.Fatalf("expected 0 mismatch rows for matched pair, got %d", got)
	}
	if got := countTagged(t, store, "kind:bus-parity-stats"); got != 1 {
		t.Fatalf("expected 1 stats row, got %d", got)
	}
	rows, _ := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{"kind:bus-parity-stats"}, Limit: 1}}, nil)
	if len(rows) != 1 || !strings.Contains(rows[0].Content, "matched=1") {
		t.Fatalf("stats row missing matched=1: %v", rows)
	}
}

func TestVerifier_HubOnlyMismatch(t *testing.T) {
	t.Parallel()
	v, store := newFixture(t, 50*time.Millisecond)

	now := time.Now().UTC()
	v.Observe(busparity.Observation{Source: busparity.SourceHub, Table: "tasks", RowID: "task_2", WorkspaceID: "wksp", SeenAt: now})

	// Sweep past the window — the orphaned hub observation expires.
	v.Sweep(context.Background(), now.Add(200*time.Millisecond))

	if got := countTagged(t, store, "kind:bus-parity-mismatch"); got != 1 {
		t.Fatalf("expected 1 mismatch row, got %d", got)
	}
	rows, _ := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{"kind:bus-parity-mismatch"}, Limit: 1}}, nil)
	if !strings.Contains(rows[0].Content, "hub_only") {
		t.Fatalf("expected hub_only classification, got %s", rows[0].Content)
	}

	// Stats row reflects the hub_only count.
	v.EmitStats(context.Background(), now.Add(200*time.Millisecond))
	statsRows, _ := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{"kind:bus-parity-stats"}, Limit: 1}}, nil)
	if !strings.Contains(statsRows[0].Content, "hub_only=1") {
		t.Fatalf("stats row missing hub_only=1: %s", statsRows[0].Content)
	}
}

func TestVerifier_LiveOnlyMismatch(t *testing.T) {
	t.Parallel()
	v, store := newFixture(t, 50*time.Millisecond)

	now := time.Now().UTC()
	v.Observe(busparity.Observation{Source: busparity.SourceLive, Table: "tasks", RowID: "task_3", WorkspaceID: "wksp", SeenAt: now})
	v.Sweep(context.Background(), now.Add(200*time.Millisecond))

	rows, _ := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{"kind:bus-parity-mismatch"}, Limit: 1}}, nil)
	if len(rows) != 1 || !strings.Contains(rows[0].Content, "live_only") {
		t.Fatalf("expected live_only mismatch, got %v", rows)
	}
}

func TestVerifier_SameBusReassertionUpdates(t *testing.T) {
	t.Parallel()
	v, store := newFixture(t, 100*time.Millisecond)

	t0 := time.Now().UTC()
	v.Observe(busparity.Observation{Source: busparity.SourceHub, Table: "tasks", RowID: "task_4", WorkspaceID: "wksp", SeenAt: t0})
	// Same bus reassertion at t0+50ms should refresh the timestamp.
	v.Observe(busparity.Observation{Source: busparity.SourceHub, Table: "tasks", RowID: "task_4", WorkspaceID: "wksp", SeenAt: t0.Add(50 * time.Millisecond)})

	// Sweep at t0+90ms — under window from refresh, no expiry yet.
	v.Sweep(context.Background(), t0.Add(90*time.Millisecond))
	if got := countTagged(t, store, "kind:bus-parity-mismatch"); got != 0 {
		t.Fatalf("expected 0 mismatches before window expiry, got %d", got)
	}

	// Sweep at t0+200ms — past window from the refresh; expires.
	v.Sweep(context.Background(), t0.Add(200*time.Millisecond))
	if got := countTagged(t, store, "kind:bus-parity-mismatch"); got != 1 {
		t.Fatalf("expected 1 mismatch after expiry, got %d", got)
	}
}

func TestVerifier_EmitStatsResetsCounters(t *testing.T) {
	t.Parallel()
	v, store := newFixture(t, 50*time.Millisecond)

	now := time.Now().UTC()
	v.Observe(busparity.Observation{Source: busparity.SourceHub, Table: "tasks", RowID: "task_5a", WorkspaceID: "wksp", SeenAt: now})
	v.Observe(busparity.Observation{Source: busparity.SourceLive, Table: "tasks", RowID: "task_5a", WorkspaceID: "wksp", SeenAt: now})
	v.EmitStats(context.Background(), now.Add(60*time.Millisecond))
	rows, _ := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{"kind:bus-parity-stats"}, Limit: 5}}, nil)
	if len(rows) != 1 || !strings.Contains(rows[0].Content, "matched=1") {
		t.Fatalf("expected matched=1 stats row, got %v", rows)
	}

	// Second observation, then EmitStats — counters reset → matched=0.
	v.Observe(busparity.Observation{Source: busparity.SourceHub, Table: "tasks", RowID: "task_5b", WorkspaceID: "wksp", SeenAt: now.Add(120 * time.Millisecond)})
	v.EmitStats(context.Background(), now.Add(120*time.Millisecond))
	rows2, _ := store.Search(context.Background(), "proj_test", ledger.SearchOptions{ListOptions: ledger.ListOptions{Tags: []string{"kind:bus-parity-stats"}, Limit: 5}}, nil)
	if len(rows2) != 2 {
		t.Fatalf("expected 2 stats rows, got %d", len(rows2))
	}
	// Newest first — verify the second matches=0.
	if !strings.Contains(rows2[0].Content, "matched=0") {
		t.Fatalf("expected matched=0 on second stats row, got %s", rows2[0].Content)
	}
}

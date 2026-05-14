package clientdaemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadState_MissingFile returns an empty zero-State + nil error.
func TestLoadState_MissingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(s.Running) != 0 || len(s.Queued) != 0 {
		t.Fatalf("zero-State expected, got %+v", s)
	}
}

// TestWriteThenLoadStateRoundTrip is the atomic-rename writer's
// happy path.
func TestWriteThenLoadStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := State{
		Running: []RunningEntry{{TaskID: "task_a", PID: 4242, StartedAt: time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC), ProjectID: "proj_x"}},
		Queued:  []QueuedEntry{{TaskID: "task_b", EnqueuedAt: time.Date(2026, 5, 14, 10, 1, 0, 0, time.UTC), QueuePosition: 1}},
	}
	if err := WriteState(path, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got.Running) != 1 || got.Running[0].TaskID != "task_a" || got.Running[0].PID != 4242 || got.Running[0].ProjectID != "proj_x" {
		t.Errorf("Running mismatch: %+v", got.Running)
	}
	if len(got.Queued) != 1 || got.Queued[0].TaskID != "task_b" {
		t.Errorf("Queued mismatch: %+v", got.Queued)
	}
}

// TestReconcileBoot_OrphanEvidenceAndQueueRetained is sty_5aa20f1b
// AC7's gate: a `running` entry observed at boot time is dropped
// from in-memory state AND a kind:daemon-orphaned-subprocess ledger
// row is emitted; queued entries copy verbatim.
func TestReconcileBoot_OrphanEvidenceAndQueueRetained(t *testing.T) {
	stub := newStub(t)
	d := newTestDaemon(t, stub, nil)

	seed := State{
		Running: []RunningEntry{
			{TaskID: "task_orphan", PID: 1, StartedAt: time.Now().Add(-1 * time.Hour), ProjectID: "proj_x"},
		},
		Queued: []QueuedEntry{
			{TaskID: "task_q1", EnqueuedAt: time.Now()},
			{TaskID: "task_q2", EnqueuedAt: time.Now()},
		},
	}

	if err := d.ReconcileBoot(context.Background(), seed); err != nil {
		t.Fatalf("ReconcileBoot: %v", err)
	}

	st := d.Status()
	if len(st.Running) != 0 {
		t.Errorf("running entries cleared after reconcile, got %+v", st.Running)
	}
	if len(st.Queued) != 2 {
		t.Errorf("queued retained, want 2 got %d (%+v)", len(st.Queued), st.Queued)
	}

	ledger := stub.ledger()
	var orphan map[string]any
	for _, l := range ledger {
		tags, _ := l["tags"].([]any)
		for _, tg := range tags {
			if tg == "kind:daemon-orphaned-subprocess" {
				orphan = l
				break
			}
		}
	}
	if orphan == nil {
		t.Fatalf("no orphan ledger row emitted (got %d ledger rows)", len(ledger))
	}
	if proj, _ := orphan["project_id"].(string); proj != "proj_x" {
		t.Errorf("orphan project_id = %q, want proj_x", proj)
	}
	structuredRaw, _ := orphan["structured"].(string)
	if structuredRaw == "" {
		// structured is decoded as a json.RawMessage which the stub
		// stores as a map; re-marshal for the contains check.
		if asMap, ok := orphan["structured"].(map[string]any); ok {
			b, _ := json.Marshal(asMap)
			structuredRaw = string(b)
		}
	}
	if structuredRaw == "" {
		t.Fatalf("orphan structured payload missing (orphan=%v)", orphan)
	}

	// State.json on disk should now reflect the reconciled (empty
	// running) shape.
	persisted, err := LoadState(d.opts.StatePath)
	if err != nil {
		t.Fatalf("LoadState persisted: %v", err)
	}
	if len(persisted.Running) != 0 {
		t.Errorf("persisted state still holds running entries: %+v", persisted.Running)
	}
	if len(persisted.Queued) != 2 {
		t.Errorf("persisted state queued len=%d, want 2", len(persisted.Queued))
	}
}

// TestReconcileBoot_NoProjectIDSkipsLedgerRow guards the defensive
// branch when persisted state lacks project_id (e.g. crash before
// persistRunningWithProject ran).
func TestReconcileBoot_NoProjectIDSkipsLedgerRow(t *testing.T) {
	stub := newStub(t)
	d := newTestDaemon(t, stub, nil)
	seed := State{Running: []RunningEntry{{TaskID: "task_x", PID: 1, StartedAt: time.Now()}}}
	if err := d.ReconcileBoot(context.Background(), seed); err != nil {
		t.Fatalf("ReconcileBoot: %v", err)
	}
	if n := len(stub.ledger()); n != 0 {
		t.Errorf("expected no ledger rows when project_id absent, got %d", n)
	}
	if len(d.Status().Running) != 0 {
		t.Errorf("running entries cleared even when ledger row skipped")
	}
}

// TestPidAlive sanity-covers both branches.
func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Errorf("pidAlive(self)=false")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Errorf("pidAlive(non-positive)=true")
	}
}

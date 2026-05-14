package clientdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bobmcallan/satellites/internal/cliremote"
)

// State is the shape persisted to ~/.satellites/daemon/state.json
// (atomic-rename writer). On boot the daemon LoadState's it +
// reconcileBoot's any orphaned running entries to evidence rows.
type State struct {
	Running []RunningEntry `json:"running"`
	Queued  []QueuedEntry  `json:"queued"`
}

// LoadState reads + parses state.json. Missing file returns a zero
// State + nil error so the daemon's first boot is not an error case.
func LoadState(path string) (State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{Running: []RunningEntry{}, Queued: []QueuedEntry{}}, nil
		}
		return State{}, fmt.Errorf("clientdaemon: read state %q: %w", path, err)
	}
	var s State
	if len(raw) == 0 {
		return State{Running: []RunningEntry{}, Queued: []QueuedEntry{}}, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, fmt.Errorf("clientdaemon: decode state %q: %w", path, err)
	}
	if s.Running == nil {
		s.Running = []RunningEntry{}
	}
	if s.Queued == nil {
		s.Queued = []QueuedEntry{}
	}
	return s, nil
}

// WriteState atomically replaces path with a JSON encoding of s.
func WriteState(path string, s State) error {
	if s.Running == nil {
		s.Running = []RunningEntry{}
	}
	if s.Queued == nil {
		s.Queued = []QueuedEntry{}
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("clientdaemon: encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("clientdaemon: mkdir %q: %w", filepath.Dir(path), err)
	}
	// Use os.CreateTemp so concurrent WriteState calls don't race on
	// a shared "<path>.tmp" filename. Each writer gets a unique tmp,
	// then atomically Renames into place.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state.json-")
	if err != nil {
		return fmt.Errorf("clientdaemon: create state tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("clientdaemon: write state tmp %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("clientdaemon: close state tmp %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("clientdaemon: rename state %q: %w", path, err)
	}
	return nil
}

// persistState snapshots the daemon's in-memory queue + running map
// and atomically writes it to disk. Called after every state-changing
// operation (enqueue, cancel, runOne entry/exit).
func (d *Daemon) persistState() error {
	d.mu.Lock()
	state := State{
		Running: make([]RunningEntry, 0, len(d.running)),
		Queued:  make([]QueuedEntry, len(d.queue)),
	}
	for _, h := range d.running {
		state.Running = append(state.Running, RunningEntry{TaskID: h.TaskID, PID: h.PID, StartedAt: h.StartedAt})
	}
	for i, q := range d.queue {
		state.Queued[i] = q
		state.Queued[i].QueuePosition = i + 1
	}
	d.mu.Unlock()
	return WriteState(d.opts.StatePath, state)
}

// ReconcileBoot replays state.json into the in-memory daemon. Any
// `running` entry observed at boot time is treated as orphaned —
// v1 does NOT adopt the subprocess (anchor §3.7 + §5 open question 1).
// For each orphaned entry the daemon emits a kind:daemon-orphaned-
// subprocess evidence row and clears the entry. Queued entries copy
// verbatim into the in-memory FIFO.
func (d *Daemon) ReconcileBoot(ctx context.Context, s State) error {
	d.mu.Lock()
	d.queue = append([]QueuedEntry(nil), s.Queued...)
	d.running = map[string]*runningHandle{}
	orphans := append([]RunningEntry(nil), s.Running...)
	d.mu.Unlock()

	for _, e := range orphans {
		alive := pidAlive(e.PID)
		d.appendOrphanEvidence(ctx, e, alive)
	}

	return d.persistState()
}

// pidAlive reports whether pid is currently alive (kill(pid, 0)).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// appendOrphanEvidence writes a `kind:daemon-orphaned-subprocess` row
// for an entry observed in the persisted-running set at boot. The row
// records the original PID, started_at, and whether the pid was still
// alive when reconciliation ran.
func (d *Daemon) appendOrphanEvidence(ctx context.Context, entry RunningEntry, wasAlive bool) {
	if d.opts.API == nil {
		return
	}
	structured, err := json.Marshal(map[string]any{
		"task_id":          entry.TaskID,
		"orphaned_pid":     entry.PID,
		"orphan_was_alive": wasAlive,
		"started_at":       entry.StartedAt.UTC().Format(time.RFC3339Nano),
		"reconciled_at":    d.opts.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		d.warn("orphan evidence marshal", err)
		return
	}
	if entry.ProjectID == "" {
		// Without a project_id we cannot route the row; surface a
		// warning and skip. The orphaned task entry is still
		// dropped from in-memory state via ReconcileBoot.
		d.warn("orphan evidence skipped (no project_id)", fmt.Errorf("task_id=%s", entry.TaskID))
		return
	}
	args := map[string]any{
		"project_id": entry.ProjectID,
		"type":       "evidence",
		"tags": []string{
			"task_id:" + entry.TaskID,
			"kind:daemon-orphaned-subprocess",
		},
		"content":    fmt.Sprintf("daemon orphan: task=%s pid=%d alive=%v", entry.TaskID, entry.PID, wasAlive),
		"structured": json.RawMessage(structured),
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.opts.API.Call(cctx, "ledger_append", args, nil); err != nil {
		d.warn("orphan ledger_append", err)
	}
}

// Restated to silence "imported and not used" if cliremote is not
// referenced from this file in a future variant.
var _ = cliremote.New

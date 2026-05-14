// Package tasklog is the substrate primitive for high-volume task
// telemetry rows — lifecycle (start / heartbeat / stop) plus stdout/
// stderr chunks emitted by `satellites-client task run`. The substrate
// keeps these rows OUT of the ledger (volume too high) and writes a
// single ledger pointer row on task close that anchors the audit chain
// to the task_log row range.
//
// Sty_8c17b89d. Producer: `satellites-client task run`. Consumer: the
// SSE endpoint `GET /api/v1/task/log/stream?task_id=…` (portal-ui).
// Append-only — no UPDATE / DELETE on the production path.
package tasklog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Kind enum values name the lifecycle frame a row carries.
const (
	KindStart     = "start"
	KindHeartbeat = "heartbeat"
	KindStdout    = "stdout"
	KindStderr    = "stderr"
	KindStop      = "stop"

	// sty_090f6183: six new server-side semantic-event kinds. Existing
	// five process-lifecycle kinds (above) are emitted by the CLI-side
	// dispatchteam wrapper; these six are emitted by the substrate's
	// typed *client.Client methods + the HTTP middleware that wraps
	// dispatched-agent /api/v1/* calls.
	KindClaim         = "claim"
	KindToolCallStart = "tool_call_start"
	KindToolCallEnd   = "tool_call_end"
	KindLedgerAppend  = "ledger_append"
	KindStatusChange  = "status_change"
	KindClose         = "close"
)

// validKinds bounds the producer surface so the SSE handler can render
// any row it pulls back without an enum-mismatch surprise.
var validKinds = map[string]struct{}{
	KindStart:         {},
	KindHeartbeat:     {},
	KindStdout:        {},
	KindStderr:        {},
	KindStop:          {},
	KindClaim:         {},
	KindToolCallStart: {},
	KindToolCallEnd:   {},
	KindLedgerAppend:  {},
	KindStatusChange:  {},
	KindClose:         {},
}

// Entry is one task_log row. Field shape mirrors the Surreal table
// defined at boot in surreal.go. `seq` is producer-supplied (the running
// task is the single producer per task_id; ordering is its
// responsibility) which keeps Append idempotent and lets the SSE
// handler honour `Last-Event-ID: <seq>` reconnects without consulting a
// server-side counter.
type Entry struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	WorkspaceID string    `json:"workspace_id"`
	ProjectID   string    `json:"project_id,omitempty"`
	Seq         int64     `json:"seq"`
	TS          time.Time `json:"ts"`
	Kind        string    `json:"kind"`
	Payload     []byte    `json:"payload,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewID returns a fresh task_log id in canonical `tlog_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("tlog_%s", uuid.NewString()[:8])
}

// ErrNotFound is returned when a task_log lookup misses.
var ErrNotFound = errors.New("task_log: not found")

// ErrInvalidKind names a row whose Kind is outside the canonical set.
var ErrInvalidKind = errors.New("task_log: invalid kind")

// Validate returns the first invariant violation on e, or nil if e is
// well-formed. Used at the store layer before every write.
func (e Entry) Validate() error {
	if e.TaskID == "" {
		return errors.New("task_log: task_id required")
	}
	if e.WorkspaceID == "" {
		return errors.New("task_log: workspace_id required")
	}
	if e.Seq < 0 {
		return errors.New("task_log: seq must be non-negative")
	}
	if _, ok := validKinds[e.Kind]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidKind, e.Kind)
	}
	return nil
}

// ListOptions narrows List. FromSeq is inclusive; Limit ≤ 0 returns
// every row visible to the caller's workspace memberships.
type ListOptions struct {
	TaskID      string
	FromSeq     int64
	Limit       int
	Memberships []string
}

// Store is the persistence surface for task_log rows. Append-only.
// Subscribe is the fan-out seam the SSE handler consumes.
type Store interface {
	// Append writes one row and fans it out to live subscribers for
	// the same task_id. Validates enum + tenancy fields.
	Append(ctx context.Context, e Entry, now time.Time) (Entry, error)

	// List returns rows for opts.TaskID, ordered by seq ASC. Filters
	// rows below opts.FromSeq. Honours opts.Memberships if non-nil.
	List(ctx context.Context, opts ListOptions) ([]Entry, error)

	// Subscribe returns a channel that fans out future rows for
	// taskID. The caller must drain the channel; missing rows below
	// fromSeq are NOT replayed (callers replay via List first). Cancel
	// is called by the consumer when done — releases the underlying
	// channel slot. Append after Cancel is a no-op for this consumer.
	Subscribe(ctx context.Context, taskID string, memberships []string) (ch <-chan Entry, cancel func(), err error)

	// NextSeq allocates the next monotonically-increasing seq value for
	// taskID. Server-side emitters (sty_090f6183: claim, tool_call_*,
	// ledger_append, status_change, close) use this to avoid colliding
	// with the CLI-side dispatchteam's per-process atomic.Int64. Seeded
	// from len(rows[taskID]) on first call so server-side rows interleave
	// correctly with any pre-existing producer-supplied seqs.
	NextSeq(ctx context.Context, taskID string) (int64, error)
}

// MemoryStore is the in-process implementation backing unit tests and
// the SSE path under test fixtures. Concurrency-safe.
type MemoryStore struct {
	mu          sync.Mutex
	rows        map[string][]Entry  // task_id -> rows (append order == seq order, stable on monotonic Seq)
	subs        map[string][]*subFn // task_id -> live subscribers
	wsByID      map[string]string   // task_id -> workspace_id, for membership scoping at Subscribe time
	nextSeqByID map[string]int64    // task_id -> next seq to hand out for server-side emitters
}

// subFn carries one fan-out channel + a cancelled flag the producer
// checks before pushing. A nil-receive on cancelled does NOT panic
// thanks to the channel-not-closed shape: cancellation is a flag not
// a close, so Append never writes onto a closed channel.
type subFn struct {
	ch        chan Entry
	cancelled bool
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		rows:        make(map[string][]Entry),
		subs:        make(map[string][]*subFn),
		wsByID:      make(map[string]string),
		nextSeqByID: make(map[string]int64),
	}
}

// Append implements Store for MemoryStore.
func (m *MemoryStore) Append(ctx context.Context, e Entry, now time.Time) (Entry, error) {
	if err := e.Validate(); err != nil {
		return Entry{}, err
	}
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.TS.IsZero() {
		e.TS = now
	}
	e.CreatedAt = now

	m.mu.Lock()
	m.rows[e.TaskID] = append(m.rows[e.TaskID], e)
	m.wsByID[e.TaskID] = e.WorkspaceID
	if cur := m.nextSeqByID[e.TaskID]; e.Seq+1 > cur {
		m.nextSeqByID[e.TaskID] = e.Seq + 1
	}
	live := append([]*subFn(nil), m.subs[e.TaskID]...)
	m.mu.Unlock()

	// Fan out to live subscribers under no-lock so a slow consumer
	// cannot block the producer (Append must stay fast). Channels are
	// buffered; a full channel drops the frame for that subscriber,
	// which the SSE handler tolerates by replaying via List on
	// reconnect.
	for _, sub := range live {
		if sub == nil || sub.cancelled {
			continue
		}
		select {
		case sub.ch <- e:
		default:
		}
	}
	return e, nil
}

// List implements Store for MemoryStore.
func (m *MemoryStore) List(ctx context.Context, opts ListOptions) ([]Entry, error) {
	if opts.TaskID == "" {
		return nil, errors.New("task_log: task_id required")
	}
	m.mu.Lock()
	all := append([]Entry(nil), m.rows[opts.TaskID]...)
	ws := m.wsByID[opts.TaskID]
	m.mu.Unlock()
	if !workspaceVisible(ws, opts.Memberships) {
		return []Entry{}, nil
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	out := make([]Entry, 0, len(all))
	for _, r := range all {
		if r.Seq < opts.FromSeq {
			continue
		}
		out = append(out, r)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Subscribe implements Store for MemoryStore.
//
// The returned channel buffers 256 rows. Cancellation flips the
// cancelled flag (Append checks it under no-lock); a follow-up sweep
// trims the slot from m.subs.
func (m *MemoryStore) Subscribe(ctx context.Context, taskID string, memberships []string) (<-chan Entry, func(), error) {
	if taskID == "" {
		return nil, nil, errors.New("task_log: task_id required")
	}
	m.mu.Lock()
	ws := m.wsByID[taskID]
	m.mu.Unlock()
	if !workspaceVisible(ws, memberships) {
		return nil, nil, ErrNotFound
	}
	sub := &subFn{ch: make(chan Entry, 256)}
	m.mu.Lock()
	m.subs[taskID] = append(m.subs[taskID], sub)
	m.mu.Unlock()
	cancel := func() {
		m.mu.Lock()
		sub.cancelled = true
		// Tighten the slice so repeated Subscribe/Cancel doesn't grow
		// unbounded for the same task.
		slots := m.subs[taskID]
		kept := slots[:0]
		for _, s := range slots {
			if !s.cancelled {
				kept = append(kept, s)
			}
		}
		m.subs[taskID] = kept
		m.mu.Unlock()
	}
	return sub.ch, cancel, nil
}

// NextSeq implements Store for MemoryStore. Allocator monotonically
// advances past both prior server-allocated values and the highest
// producer-supplied seq Append has observed. Seeds from len(rows[taskID])
// on first call so server-side rows interleave correctly with any
// pre-existing producer-supplied seqs.
func (m *MemoryStore) NextSeq(ctx context.Context, taskID string) (int64, error) {
	if taskID == "" {
		return 0, errors.New("task_log: task_id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.nextSeqByID[taskID]
	if !ok {
		cur = int64(len(m.rows[taskID]))
	}
	m.nextSeqByID[taskID] = cur + 1
	return cur, nil
}

// workspaceVisible mirrors task.workspaceVisible: nil memberships =
// no scoping (server-internal callers); empty = deny-all; non-empty =
// workspace_id must appear.
func workspaceVisible(workspaceID string, memberships []string) bool {
	if memberships == nil {
		return true
	}
	if workspaceID == "" {
		// Row with no stamped workspace is server-only metadata; only
		// nil-memberships callers see it.
		return false
	}
	for _, m := range memberships {
		if m == workspaceID {
			return true
		}
	}
	return false
}

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

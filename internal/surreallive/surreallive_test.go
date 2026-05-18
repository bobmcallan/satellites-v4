// sty_c7c3850f — Subscriber against a stub DB. Coverage:
//
//   - Live + LiveNotifications happy path forwards events for an
//     allowed workspace.
//   - CREATE/UPDATE/DELETE notifications all surface with a Kind of
//     `<table>.<action>`.
//   - Notifications for a workspace not in allowed are dropped.
//   - When the notification channel closes mid-stream, Subscribe
//     reconnects (the dial loop opens a new Live + LiveNotifications
//     pair and continues forwarding).
//   - ctx cancel returns ctx.Err() and CloseLiveNotifications is
//     called for the active liveID.
//   - Empty workspaceIDs forwards everything (system subscriber).
package surreallive_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/surreallive"
)

// stubDB is the testable replacement for the production
// *surrealdb.DB adapter. The fixture controls when Live succeeds,
// when LiveNotifications closes, and what Notifications stream.
type stubDB struct {
	mu sync.Mutex

	// liveCalls counts the number of times Live() has been invoked.
	// The dial loop's reconnect path bumps this when it re-registers.
	liveCalls atomic.Int32

	// channels is a list of pre-baked channels the stub returns from
	// LiveNotifications, one per Live call. Tests prepare the queue
	// before invoking Subscribe.
	channels []chan surreallive.Notification

	// closedIDs records the live ids CloseLiveNotifications was called
	// against, so tests can assert cleanup after ctx cancel.
	closedIDs []string
}

func (s *stubDB) push(ch chan surreallive.Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels = append(s.channels, ch)
}

func (s *stubDB) Live(ctx context.Context, table string, diff bool) (string, error) {
	idx := s.liveCalls.Add(1)
	return "lq_" + table + "_" + itoa(int(idx)), nil
}

func (s *stubDB) LiveNotifications(liveID string) (<-chan surreallive.Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.channels) == 0 {
		// No prepared channel — return one that never receives, the
		// test should ctx-cancel.
		idle := make(chan surreallive.Notification)
		return idle, nil
	}
	ch := s.channels[0]
	s.channels = s.channels[1:]
	return ch, nil
}

func (s *stubDB) CloseLiveNotifications(liveID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closedIDs = append(s.closedIDs, liveID)
	return nil
}

// itoa is a tiny base-10 int formatter so the test file doesn't pull
// strconv into its lonely import set.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [16]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestSubscriber_ForwardsAllowedWorkspace(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch := make(chan surreallive.Notification, 4)
	stub.push(ch)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	got := make(chan surreallive.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = sub.Subscribe(ctx, "tasks", []string{"wksp_a"}, func(ev surreallive.Event) {
			got <- ev
		})
	}()

	ch <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"workspace_id": "wksp_a", "project_id": "p1", "task_id": "t1"},
	}
	ch <- surreallive.Notification{
		Action: surreallive.ActionUpdate,
		Result: map[string]any{"workspace_id": "wksp_a", "task_id": "t1", "status": "claimed"},
	}
	ch <- surreallive.Notification{
		Action: surreallive.ActionDelete,
		Result: map[string]any{"workspace_id": "wksp_a", "task_id": "t1"},
	}

	want := []struct {
		kind   string
		action surreallive.Action
	}{
		{"tasks.create", surreallive.ActionCreate},
		{"tasks.update", surreallive.ActionUpdate},
		{"tasks.delete", surreallive.ActionDelete},
	}
	for _, w := range want {
		select {
		case ev := <-got:
			if ev.Kind != w.kind || ev.Action != w.action || ev.Table != "tasks" {
				t.Fatalf("bad event: kind=%s action=%s table=%s want kind=%s action=%s",
					ev.Kind, ev.Action, ev.Table, w.kind, w.action)
			}
			if ev.WorkspaceID != "wksp_a" {
				t.Fatalf("workspace mismatch: %s", ev.WorkspaceID)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %s", w.kind)
		}
	}
}

func TestSubscriber_DropsForeignWorkspace(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch := make(chan surreallive.Notification, 4)
	stub.push(ch)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	got := make(chan surreallive.Event, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	go func() {
		_ = sub.Subscribe(ctx, "tasks", []string{"wksp_a"}, func(ev surreallive.Event) {
			got <- ev
		})
	}()

	// Send a foreign-workspace notification; it must NOT surface.
	ch <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"workspace_id": "wksp_b", "task_id": "t2"},
	}

	select {
	case ev := <-got:
		t.Fatalf("expected no event for wksp_b, got %+v", ev)
	case <-time.After(80 * time.Millisecond):
		// Good — nothing surfaced.
	}
}

func TestSubscriber_EmptyWorkspaceIDsForwardsAll(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch := make(chan surreallive.Notification, 4)
	stub.push(ch)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	got := make(chan surreallive.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = sub.Subscribe(ctx, "tasks", nil, func(ev surreallive.Event) { got <- ev })
	}()

	ch <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"workspace_id": "wksp_anything", "task_id": "t3"},
	}

	select {
	case ev := <-got:
		if ev.WorkspaceID != "wksp_anything" {
			t.Fatalf("workspace mismatch: %s", ev.WorkspaceID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected event with empty workspace filter")
	}
}

func TestSubscriber_ReconnectsAfterChannelClose(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch1 := make(chan surreallive.Notification, 1)
	ch2 := make(chan surreallive.Notification, 1)
	stub.push(ch1)
	stub.push(ch2)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	got := make(chan surreallive.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = sub.Subscribe(ctx, "tasks", []string{"wksp_a"}, func(ev surreallive.Event) {
			got <- ev
		})
	}()

	ch1 <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"workspace_id": "wksp_a", "task_id": "t-a"},
	}
	// First event lands.
	select {
	case ev := <-got:
		if ev.Kind != "tasks.create" {
			t.Fatalf("bad event before close: %s", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("first event timed out")
	}
	// Close ch1 to simulate server-side disconnect.
	close(ch1)

	// Wait briefly for reconnect (backoff-bounded).
	time.Sleep(50 * time.Millisecond)
	if got := stub.liveCalls.Load(); got < 2 {
		t.Fatalf("expected reconnect to call Live again; liveCalls=%d", got)
	}

	// Second event on the new channel should land.
	ch2 <- surreallive.Notification{
		Action: surreallive.ActionUpdate,
		Result: map[string]any{"workspace_id": "wksp_a", "task_id": "t-a", "status": "claimed"},
	}
	select {
	case ev := <-got:
		if ev.Kind != "tasks.update" {
			t.Fatalf("bad event after reconnect: %s", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("post-reconnect event timed out")
	}
}

func TestSubscriber_CtxCancelClosesLive(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch := make(chan surreallive.Notification, 1)
	stub.push(ch)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sub.Subscribe(ctx, "tasks", nil, func(ev surreallive.Event) {})
	}()

	time.Sleep(20 * time.Millisecond) // let Subscribe register
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe didn't return on ctx cancel")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closedIDs) == 0 {
		t.Fatalf("CloseLiveNotifications was never called")
	}
}

func TestSubscriber_PanicInOnEventDoesNotKillLoop(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch := make(chan surreallive.Notification, 2)
	stub.push(ch)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	calls := atomic.Int32{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = sub.Subscribe(ctx, "tasks", nil, func(ev surreallive.Event) {
			calls.Add(1)
			if calls.Load() == 1 {
				panic("first call panics")
			}
		})
	}()

	ch <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"task_id": "t1"},
	}
	ch <- surreallive.Notification{
		Action: surreallive.ActionUpdate,
		Result: map[string]any{"task_id": "t1"},
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected loop to survive panic and process 2 events, got %d", calls.Load())
	}
}

// sty_bc732746 regression — the wshandler now subscribes to four
// additional tables (documents, repos, commits, projects), so a
// translator that panics on a malformed row for any of them must not
// stop the dial loop. Mirrors the production wiring in
// SurrealLiveSource.Run: the onEvent closure is `translate → fanout`,
// and dispatch wraps it with recover. This regression asserts the
// recover-guard scope remains load-bearing: a translator panic on
// event N still allows event N+1 (and N+2) to be delivered.
func TestSubscriber_TranslatePanic_DoesNotStopSubsequentDelivery(t *testing.T) {
	t.Parallel()
	stub := &stubDB{}
	ch := make(chan surreallive.Notification, 3)
	stub.push(ch)

	sub := surreallive.New(stub, surreallive.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, nil)

	var delivered atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Mirrors SurrealLiveSource.Run's translate→fanout shape; the
		// "translator" panics on the second event but the first and
		// third must still be observed by the fanout-shaped
		// downstream.
		_ = sub.Subscribe(ctx, "documents", nil, func(ev surreallive.Event) {
			if ev.Action == surreallive.ActionUpdate {
				panic("translator panicked on UPDATE")
			}
			delivered.Add(1)
		})
	}()

	ch <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"workspace_id": "wksp_a", "id": "doc_1", "status": "new"},
	}
	ch <- surreallive.Notification{
		Action: surreallive.ActionUpdate,
		Result: map[string]any{"workspace_id": "wksp_a", "id": "doc_1", "status": "x"},
	}
	ch <- surreallive.Notification{
		Action: surreallive.ActionCreate,
		Result: map[string]any{"workspace_id": "wksp_a", "id": "doc_2", "status": "new"},
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if delivered.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := delivered.Load(); got < 2 {
		t.Fatalf("translate panic killed the loop: delivered=%d want>=2", got)
	}
}

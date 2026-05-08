package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/agent/worker"
)

// wsStub is a minimal /ws server that records inbound subscribe
// requests and lets the test push outbound events at will. The stub
// also exposes hooks to drop the connection or refuse the upgrade so
// reconnect/backoff cases can be exercised.
type wsStub struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu        sync.Mutex
	subs      []wsStubSub
	conns     []*websocket.Conn
	authHdrs  []string
	connHook  func(*websocket.Conn) // optional per-connection hook
	rejectAll atomic.Bool
}

type wsStubSub struct {
	Type    string `json:"type"`
	Topic   string `json:"topic"`
	SinceID string `json:"since_id"`
}

func newWSStub(t *testing.T) *wsStub {
	t.Helper()
	s := &wsStub{
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.authHdrs = append(s.authHdrs, r.Header.Get("Authorization"))
		hook := s.connHook
		s.mu.Unlock()
		if s.rejectAll.Load() {
			http.Error(w, "rejected", http.StatusUnauthorized)
			return
		}
		conn, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()

		// Read one subscribe message.
		var sub wsStubSub
		if err := conn.ReadJSON(&sub); err == nil {
			s.mu.Lock()
			s.subs = append(s.subs, sub)
			s.mu.Unlock()
		}

		if hook != nil {
			hook(conn)
		} else {
			// Default: hold the conn open until the test closes it.
			for {
				if _, _, err := conn.NextReader(); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *wsStub) wsURL() string {
	u, _ := url.Parse(s.srv.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	return u.String()
}

func (s *wsStub) sendEvent(t *testing.T, idx int, ev inboundEvent) {
	t.Helper()
	s.mu.Lock()
	require.Greater(t, len(s.conns), idx, "no connection yet")
	c := s.conns[idx]
	s.mu.Unlock()
	require.NoError(t, c.WriteJSON(ev))
}

func (s *wsStub) closeConn(t *testing.T, idx int) {
	t.Helper()
	s.mu.Lock()
	require.Greater(t, len(s.conns), idx)
	c := s.conns[idx]
	s.mu.Unlock()
	_ = c.Close()
}

// inboundEvent mirrors the wsclient's deserialisation shape — kept
// here so tests can author payloads without importing the unexported
// type.
type inboundEvent struct {
	ID          string         `json:"ID"`
	Topic       string         `json:"Topic"`
	Kind        string         `json:"Kind"`
	WorkspaceID string         `json:"WorkspaceID"`
	Data        map[string]any `json:"Data"`
}

func TestWSClient_SubscribeRoundTrip(t *testing.T) {
	stub := newWSStub(t)
	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wakeCh := c.Run(ctx)

	require.Eventually(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.subs) >= 1
	}, time.Second, 10*time.Millisecond, "subscribe never received")

	stub.mu.Lock()
	assert.Equal(t, "subscribe", stub.subs[0].Type)
	assert.Equal(t, "ws:wksp_a", stub.subs[0].Topic)
	stub.mu.Unlock()

	stub.sendEvent(t, 0, inboundEvent{
		ID: "1", Topic: "ws:wksp_a", Kind: "task.published", WorkspaceID: "wksp_a",
		Data: map[string]any{"task_id": "task_x", "project_id": "proj_y"},
	})

	select {
	case wake := <-wakeCh:
		assert.Equal(t, "task_x", wake.TaskID)
		assert.Equal(t, "proj_y", wake.ProjectID)
		assert.Equal(t, "wksp_a", wake.WorkspaceID)
	case <-time.After(time.Second):
		t.Fatal("WakeEvent never arrived")
	}
}

func TestWSClient_NonTaskPublishedDropped(t *testing.T) {
	stub := newWSStub(t)
	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wakeCh := c.Run(ctx)
	require.Eventually(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.subs) >= 1
	}, time.Second, 10*time.Millisecond)

	stub.sendEvent(t, 0, inboundEvent{
		ID: "1", Topic: "ws:wksp_a", Kind: "task.claimed", WorkspaceID: "wksp_a",
		Data: map[string]any{"task_id": "task_x"},
	})

	select {
	case wake := <-wakeCh:
		t.Fatalf("non-task.published event should be dropped, got %+v", wake)
	case <-time.After(150 * time.Millisecond):
		// expected
	}
}

func TestWSClient_ReconnectWithSinceCursor(t *testing.T) {
	stub := newWSStub(t)
	connectCount := atomic.Int32{}
	stub.mu.Lock()
	stub.connHook = func(conn *websocket.Conn) {
		idx := connectCount.Add(1)
		switch idx {
		case 1:
			// First connect: emit one event with id "1", then close.
			_ = conn.WriteJSON(inboundEvent{
				ID: "1", Topic: "ws:wksp_a", Kind: "task.published", WorkspaceID: "wksp_a",
				Data: map[string]any{"task_id": "task_first"},
			})
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
		case 2:
			// Second connect: emit one event with id "2".
			_ = conn.WriteJSON(inboundEvent{
				ID: "2", Topic: "ws:wksp_a", Kind: "task.published", WorkspaceID: "wksp_a",
				Data: map[string]any{"task_id": "task_second"},
			})
			// Hold open.
			for {
				if _, _, err := conn.NextReader(); err != nil {
					return
				}
			}
		}
	}
	stub.mu.Unlock()

	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wakeCh := c.Run(ctx)

	wait := func() worker.WakeEvent {
		select {
		case w := <-wakeCh:
			return w
		case <-time.After(2 * time.Second):
			t.Fatal("event timeout")
			return worker.WakeEvent{}
		}
	}
	first := wait()
	second := wait()
	assert.Equal(t, "task_first", first.TaskID)
	assert.Equal(t, "task_second", second.TaskID)

	require.Eventually(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.subs) >= 2
	}, 2*time.Second, 20*time.Millisecond, "second subscribe never seen")
	stub.mu.Lock()
	assert.Equal(t, "1", stub.subs[1].SinceID, "second subscribe must thread the last-seen id")
	stub.mu.Unlock()
}

func TestWSClient_BackoffBounds(t *testing.T) {
	stub := newWSStub(t)
	stub.rejectAll.Store(true)
	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = c.Run(ctx)

	require.Eventually(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.authHdrs) >= 3
	}, 2*time.Second, 20*time.Millisecond, "wsclient should retry at least 3 times within bounded backoff")
}

func TestWSClient_AuthHeaderForwarded(t *testing.T) {
	stub := newWSStub(t)
	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		AuthToken:             "tok-ws",
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = c.Run(ctx)
	require.Eventually(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.authHdrs) >= 1
	}, time.Second, 10*time.Millisecond)
	stub.mu.Lock()
	assert.Equal(t, "Bearer tok-ws", stub.authHdrs[0])
	stub.mu.Unlock()
}

func TestWSClient_QueryTokenAppended(t *testing.T) {
	// Validates query-param fallback: the wsclient appends ?token=<tok>
	// to the dial URL so wshandlers behind upgraders that strip
	// Authorization can still authenticate.
	var seenURL atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL.Store(r.URL.String())
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws"

	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                wsURL,
		AuthToken:             "tok-x",
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            20 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = c.Run(ctx)
	require.Eventually(t, func() bool {
		v := seenURL.Load()
		return v != nil && strings.Contains(v.(string), "token=tok-x")
	}, time.Second, 10*time.Millisecond, "query-token fallback not appended")
}

func TestWSClient_CtxCancelExits(t *testing.T) {
	stub := newWSStub(t)
	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	wakeCh := c.Run(ctx)
	require.Eventually(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.subs) >= 1
	}, time.Second, 10*time.Millisecond)

	cancel()
	select {
	case _, ok := <-wakeCh:
		if ok {
			// Drain extra events; channel should close shortly after.
		}
	case <-time.After(2 * time.Second):
	}
	// Wake channel must close.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-wakeCh:
			if !ok {
				return
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("wake channel did not close after ctx cancel")
}

func TestWSClient_FullChannelDropsEvent(t *testing.T) {
	stub := newWSStub(t)
	stub.mu.Lock()
	stub.connHook = func(conn *websocket.Conn) {
		// Read one subscribe (already consumed by the stub harness).
		// Then push wakeChanBuffer+5 events without giving the test a
		// chance to drain — the wsclient must drop, not deadlock.
		for i := 0; i < 100; i++ {
			err := conn.WriteJSON(inboundEvent{
				ID: idstr(int64(i + 1)), Topic: "ws:wksp_a", Kind: "task.published",
				WorkspaceID: "wksp_a", Data: map[string]any{"task_id": "task_x"},
			})
			if err != nil {
				return
			}
		}
		// Hold open.
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}
	stub.mu.Unlock()

	c := worker.NewWSClient(worker.WSConfig{
		HubURL:                stub.wsURL(),
		SubscribeWorkspaceIDs: []string{"wksp_a"},
		MinBackoff:            10 * time.Millisecond,
		MaxBackoff:            50 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	wakeCh := c.Run(ctx)

	// Drain at most one event then stop draining; the producer keeps
	// pushing, but the wsclient must not block — its select drops the
	// rest. We assert by timing: the test must complete inside ctx
	// (500ms) without the dial goroutine wedging.
	select {
	case <-wakeCh:
	case <-time.After(time.Second):
		t.Fatal("no events delivered at all")
	}
	<-ctx.Done()
	// Drain remaining; channel must close after ctx cancel.
	for {
		select {
		case _, ok := <-wakeCh:
			if !ok {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("wake channel did not close after ctx cancel — wsclient may have blocked on full channel")
		}
	}
}

// idstr renders a monotonic id so the wsclient's lastID cursor
// updates each loop without colliding.
func idstr(i int64) string {
	return jsonNumberLikeID(i)
}

// jsonNumberLikeID returns a 20-digit zero-padded string mirroring
// hub.nextID's wire shape so reconnect/cursor logic exercises the
// real comparison.
func jsonNumberLikeID(i int64) string {
	const w = 20
	s := []byte("00000000000000000000")
	digits := []byte{}
	if i == 0 {
		digits = []byte{'0'}
	}
	for n := i; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	copy(s[w-len(digits):], digits)
	return string(s)
}

// Ensure JSON encoder compiles in this file.
var _ = json.Marshal

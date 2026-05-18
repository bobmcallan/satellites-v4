// wshandler exercises the /ws upgrade + reader/writer goroutine
// pair against a stub EventSource. The hub-coupled tests from the
// pre-cutover era are gone with the hub; auth + subscribe + delivery
// + clean-disconnect + bearer/query-token paths survive.
package wshandler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
)

// --- test doubles ----------------------------------------------------------

type stubResolver struct {
	users map[string]auth.User
}

func (s *stubResolver) Resolve(_ context.Context, sessionID string) (auth.User, error) {
	u, ok := s.users[sessionID]
	if !ok {
		return auth.User{}, errString("bad session")
	}
	return u, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// stubBearer is a deterministic BearerValidator: tokens map to
// BearerInfo on hit, returns an error on miss.
type stubBearer struct {
	tokens map[string]auth.BearerInfo
}

func (s *stubBearer) Validate(_ context.Context, token string) (auth.BearerInfo, error) {
	if info, ok := s.tokens[token]; ok {
		return info, nil
	}
	return auth.BearerInfo{}, errString("invalid bearer")
}

// stubSource is the testable EventSource. push() injects a WireEvent
// into the channel of any subscriber registered against the matching
// topic.
type stubSource struct {
	mu      sync.Mutex
	chans   map[string]chan WireEvent  // subID → ch
	topics  map[string]map[string]bool // topic → subID → present
	members map[string]map[string]bool // wsID → userID → member
}

func newStubSource() *stubSource {
	return &stubSource{
		chans:   make(map[string]chan WireEvent),
		topics:  make(map[string]map[string]bool),
		members: make(map[string]map[string]bool),
	}
}

func (s *stubSource) addMember(wsID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members[wsID] == nil {
		s.members[wsID] = map[string]bool{}
	}
	s.members[wsID][userID] = true
}

func (s *stubSource) Subscribe(_ context.Context, topic, subID, userID, _ string) (<-chan WireEvent, error) {
	wsID, err := ParseTopicWorkspace(topic)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.members[wsID][userID] {
		return nil, ErrNotMember
	}
	ch := make(chan WireEvent, 16)
	s.chans[subID] = ch
	if s.topics[topic] == nil {
		s.topics[topic] = map[string]bool{}
	}
	s.topics[topic][subID] = true
	return ch, nil
}

func (s *stubSource) Unsubscribe(subID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.chans[subID]; ok {
		close(ch)
		delete(s.chans, subID)
	}
	for _, subs := range s.topics {
		delete(subs, subID)
	}
}

func (s *stubSource) push(topic string, ev WireEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for subID := range s.topics[topic] {
		select {
		case s.chans[subID] <- ev:
		default:
		}
	}
}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	server   *httptest.Server
	handler  *Handler
	source   *stubSource
	resolver *stubResolver
	bearer   *stubBearer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	source := newStubSource()
	resolver := &stubResolver{users: map[string]auth.User{}}
	bearer := &stubBearer{tokens: map[string]auth.BearerInfo{}}

	h := New(Deps{
		Source:          source,
		Sessions:        resolver,
		BearerValidator: bearer,
		Logger:          satarbor.New("info"),
		PingInterval:    time.Hour,
		WriteTimeout:    500 * time.Millisecond,
		ReadTimeout:     2 * time.Second,
	})

	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() { ts.Close() })

	return &fixture{
		server:   ts,
		handler:  h,
		source:   source,
		resolver: resolver,
		bearer:   bearer,
	}
}

func (f *fixture) seedUser(sessID, userID string) {
	f.resolver.users[sessID] = auth.User{ID: userID, Email: userID + "@example.com"}
}

func (f *fixture) dial(t *testing.T, sessionCookie string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, err := url.Parse(f.server.URL + "/ws")
	require.NoError(t, err)
	u.Scheme = "ws"
	hdr := http.Header{}
	if sessionCookie != "" {
		hdr.Set("Cookie", auth.CookieName+"="+sessionCookie)
	}
	return websocket.DefaultDialer.Dial(u.String(), hdr)
}

func (f *fixture) dialBearer(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, err := url.Parse(f.server.URL + "/ws")
	require.NoError(t, err)
	u.Scheme = "ws"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	return websocket.DefaultDialer.Dial(u.String(), hdr)
}

func (f *fixture) dialQueryToken(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, err := url.Parse(f.server.URL + "/ws?token=" + token)
	require.NoError(t, err)
	u.Scheme = "ws"
	return websocket.DefaultDialer.Dial(u.String(), nil)
}

// --- tests -----------------------------------------------------------------

func TestWS_NoCookie_Returns401(t *testing.T) {
	f := newFixture(t)
	_, resp, err := f.dial(t, "")
	require.Error(t, err, "dial must fail without a session cookie")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWS_SubscribeAsMember_ReceivesEvent(t *testing.T) {
	f := newFixture(t)
	f.seedUser("sess-alice", "alice")
	f.source.addMember("wksp_A", "alice")

	conn, _, err := f.dial(t, "sess-alice")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteJSON(subscribeMsg{Type: "subscribe", Topic: "ws:wksp_A"}))
	time.Sleep(50 * time.Millisecond)

	f.source.push("ws:wksp_A", WireEvent{
		Kind: "ledger.append", WorkspaceID: "wksp_A", Data: map[string]any{"x": "hello"},
	})

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	var got WireEvent
	require.NoError(t, conn.ReadJSON(&got))
	assert.Equal(t, "ledger.append", got.Kind)
	assert.Equal(t, "wksp_A", got.WorkspaceID)
}

func TestWS_SubscribeAsNonMember_Closed(t *testing.T) {
	f := newFixture(t)
	f.seedUser("sess-bob", "bob")
	// bob is NOT a member of wksp_A.

	conn, _, err := f.dial(t, "sess-bob")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteJSON(subscribeMsg{Type: "subscribe", Topic: "ws:wksp_A"}))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, payload, err := conn.ReadMessage()
	require.NoError(t, err, "error frame delivered before close")

	var em errorMsg
	require.NoError(t, json.Unmarshal(payload, &em))
	assert.Equal(t, "error", em.Type)
	assert.Equal(t, "not_member", em.Code)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err)
}

func TestWS_DisconnectCleansGoroutines(t *testing.T) {
	before := countSettledGoroutines(t)

	f := newFixture(t)
	f.seedUser("sess-alice", "alice")
	f.source.addMember("wksp_A", "alice")

	conn, _, err := f.dial(t, "sess-alice")
	require.NoError(t, err)
	require.NoError(t, conn.WriteJSON(subscribeMsg{Type: "subscribe", Topic: "ws:wksp_A"}))
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, conn.UnderlyingConn().Close())

	f.server.Close()

	after := countSettledGoroutines(t)
	assert.LessOrEqualf(t, after-before, 2,
		"goroutine leak suspected: before=%d after=%d", before, after)
}

func countSettledGoroutines(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var prev, curr int
	streak := 0
	for time.Now().Before(deadline) {
		curr = runtime.NumGoroutine()
		if curr == prev {
			streak++
			if streak >= 3 {
				return curr
			}
		} else {
			streak = 0
		}
		prev = curr
		time.Sleep(20 * time.Millisecond)
	}
	return curr
}

func TestServeWS_BearerHeader_OK(t *testing.T) {
	f := newFixture(t)
	f.bearer.tokens["valid-tok"] = auth.BearerInfo{UserID: "u_alice", Email: "alice@example.com"}
	f.source.addMember("wksp_A", "u_alice")

	conn, _, err := f.dialBearer(t, "valid-tok")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteJSON(subscribeMsg{Type: "subscribe", Topic: "ws:wksp_A"}))
	time.Sleep(50 * time.Millisecond)

	f.source.push("ws:wksp_A", WireEvent{Kind: "ledger.append", WorkspaceID: "wksp_A"})

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	var got WireEvent
	require.NoError(t, conn.ReadJSON(&got))
	assert.Equal(t, "ledger.append", got.Kind)
}

func TestServeWS_QueryToken_OK(t *testing.T) {
	f := newFixture(t)
	f.bearer.tokens["valid-q"] = auth.BearerInfo{UserID: "u_q", Email: "q@example.com"}
	f.source.addMember("wksp_A", "u_q")

	conn, _, err := f.dialQueryToken(t, "valid-q")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteJSON(subscribeMsg{Type: "subscribe", Topic: "ws:wksp_A"}))
	time.Sleep(50 * time.Millisecond)

	f.source.push("ws:wksp_A", WireEvent{
		Kind: "task.published", WorkspaceID: "wksp_A", Data: map[string]any{"task_id": "t1"},
	})

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	var got WireEvent
	require.NoError(t, conn.ReadJSON(&got))
	assert.Equal(t, "task.published", got.Kind)
}

func TestServeWS_NoAuth_401(t *testing.T) {
	f := newFixture(t)
	_, resp, err := f.dial(t, "")
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServeWS_InvalidBearer_401(t *testing.T) {
	f := newFixture(t)
	_, resp, err := f.dialBearer(t, "bogus")
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

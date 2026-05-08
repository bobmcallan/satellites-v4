// Package wshandler is the satellites-v4 /ws HTTP endpoint: upgrades the
// request to a websocket, resolves the caller to a user via the session
// cookie, accepts `{type:"subscribe", topic, since_id?}` messages, and
// streams workspace-scoped events from the AuthHub to the client.
//
// Tenancy enforcement lives at AuthHub (slice 10.2). This package is the
// transport adapter: it owns the upgrade handshake, the read/write
// goroutine pair, the JSON message protocol, and the goroutine-clean
// teardown on abrupt disconnect.
package wshandler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/hub"
)

// SessionResolver resolves a session cookie value to a User. In production
// this is two lookups (SessionStore.Get + UserStore.GetByID) wrapped in a
// single adapter; test setups inject a fake.
type SessionResolver interface {
	Resolve(ctx context.Context, sessionID string) (auth.User, error)
}

// BearerValidator validates an opaque bearer token (OAuth provider token,
// satellites-issued JWT, or sat_-prefixed registry token) and returns the
// resolved identity. Satisfied by *auth.BearerValidator; tests inject a
// stub.
type BearerValidator interface {
	Validate(ctx context.Context, token string) (auth.BearerInfo, error)
}

// Deps bundles the Handler's runtime dependencies.
type Deps struct {
	AuthHub  *hub.AuthHub
	Sessions SessionResolver
	Logger   arbor.ILogger
	// BearerValidator authenticates Bearer tokens carried on the WS
	// upgrade — Authorization header preferred, ?token=<key> query
	// param accepted as the upgrader-friendly fallback. Wired with
	// the same instance used by the /mcp middleware so token
	// revocation is uniform across the two surfaces. nil disables the
	// Bearer path; the handler falls back to session-cookie auth.
	BearerValidator BearerValidator
	// PingInterval paces server→client control pings on an otherwise-idle
	// connection. Zero disables pings (test mode).
	PingInterval time.Duration
	// WriteTimeout bounds each write to the client. Zero disables.
	WriteTimeout time.Duration
	// ReadTimeout bounds how long the read loop waits for a message or
	// pong. Zero disables.
	ReadTimeout time.Duration
}

// Handler owns the /ws route and the upgrader configuration.
type Handler struct {
	deps     Deps
	upgrader websocket.Upgrader
}

// New constructs a Handler from deps. Deps.Logger and Deps.AuthHub are
// required; Sessions must be non-nil outside tests.
func New(deps Deps) *Handler {
	if deps.PingInterval == 0 {
		deps.PingInterval = 25 * time.Second
	}
	if deps.WriteTimeout == 0 {
		deps.WriteTimeout = 10 * time.Second
	}
	if deps.ReadTimeout == 0 {
		deps.ReadTimeout = 60 * time.Second
	}
	return &Handler{
		deps: deps,
		upgrader: websocket.Upgrader{
			// Default checks rely on Origin; we accept any origin here and
			// rely on the session cookie (SameSite=Lax, HttpOnly) as the
			// CSRF boundary. A tighter policy can be added when CSP lands.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// Register attaches the handler at `GET /ws`.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ws", h.serveWS)
}

// subscribeMsg is the inbound subscribe request.
type subscribeMsg struct {
	Type    string `json:"type"`
	Topic   string `json:"topic"`
	SinceID string `json:"since_id,omitempty"`
}

// errorMsg is the outbound error frame sent before closing a misbehaving
// connection.
type errorMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (h *Handler) serveWS(w http.ResponseWriter, r *http.Request) {
	user, ok := h.resolveUser(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response if headers were writable.
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	subID := "ws-" + uuid.NewString()
	c := &connection{
		h:      h,
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
		subID:  subID,
		userID: user.ID,
		logger: h.deps.Logger,
		initCh: make(chan (<-chan hub.Event), 1),
	}
	c.run()
}

// resolveUser walks the supported auth schemes for /ws and returns the
// resolved User on first match. Order:
//
//  1. Session cookie (existing browser flow).
//  2. Authorization: Bearer <token> header (new — closes the auth gap
//     for headless WS subscribers like satellites-agent).
//  3. ?token=<token> query param (Bearer fallback for environments
//     that strip Authorization on the WS upgrade).
//
// Returns (zero, false) when no scheme produces a user; serveWS
// responds 401 in that case.
func (h *Handler) resolveUser(r *http.Request) (auth.User, bool) {
	if sid := auth.ReadCookie(r); sid != "" {
		if user, err := h.deps.Sessions.Resolve(r.Context(), sid); err == nil {
			return user, true
		}
	}
	if h.deps.BearerValidator == nil {
		return auth.User{}, false
	}
	tok := bearerFromHeader(r.Header.Get("Authorization"))
	if tok == "" {
		tok = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if tok == "" {
		return auth.User{}, false
	}
	info, err := h.deps.BearerValidator.Validate(r.Context(), tok)
	if err != nil {
		return auth.User{}, false
	}
	return auth.User{ID: info.UserID, Email: info.Email}, true
}

// bearerFromHeader extracts the token from an Authorization header.
// Returns "" when the header is empty or does not carry the Bearer
// scheme.
func bearerFromHeader(h string) string {
	const prefix = "Bearer "
	h = strings.TrimSpace(h)
	if len(h) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// connection owns the lifecycle of a single websocket. Ordered shutdown:
// any of (reader error, context cancel, writer error) cancels ctx; writer
// drains and closes the conn; reader exits; subscription is released on
// the shared deferred cleanup.
type connection struct {
	h      *Handler
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	subID  string
	userID string
	logger arbor.ILogger

	// initCh is the rendezvous between reader and writer: the reader
	// sends the Subscribe channel over it once (buffered 1) so the writer
	// can begin streaming. A single subscription per connection is
	// allowed; subsequent subscribe messages are ignored.
	initCh chan (<-chan hub.Event)
	// sentInit records whether the reader already delivered the event
	// channel to the writer — avoids double-send on the buffered initCh.
	sentInit bool
}

func (c *connection) run() {
	defer func() {
		c.cancel()
		c.h.deps.AuthHub.Unsubscribe(c.subID)
		_ = c.conn.Close()
	}()

	writerDone := make(chan struct{})
	go c.writer(writerDone)
	c.reader()
	<-writerDone
}

// reader drives the inbound JSON protocol. Exits on any read error (close
// frame, TCP reset, or deadline).
func (c *connection) reader() {
	deadline := c.h.deps.ReadTimeout
	if deadline > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(deadline))
	}
	c.conn.SetPongHandler(func(string) error {
		if deadline > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(deadline))
		}
		return nil
	})

	for {
		var msg subscribeMsg
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}
		if deadline > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(deadline))
		}
		if msg.Type != "subscribe" {
			c.writeError("bad_type", "only subscribe is supported")
			return
		}
		if c.sentInit {
			// One subscription per connection; ignore duplicates.
			continue
		}
		ch, err := c.h.deps.AuthHub.SubscribeSince(c.ctx, msg.Topic, c.subID, c.userID, msg.SinceID)
		if err != nil {
			c.writeError(errCode(err), err.Error())
			return
		}
		c.initCh <- ch
		c.sentInit = true
	}
}

// writer streams hub events to the client and paces keep-alive pings. It
// exits when the context is cancelled or an outbound write fails, freeing
// the reader goroutine through the conn close that serveWS schedules.
//
// evCh starts nil (blocks in select). The reader sends the Subscribe
// channel over initCh exactly once; the writer receives it and then
// disables the initCh case by setting that local to nil. A nil channel
// in select blocks forever — effectively removing the case.
func (c *connection) writer(done chan<- struct{}) {
	defer close(done)

	pingTicker := time.NewTicker(c.h.deps.PingInterval)
	defer pingTicker.Stop()

	var evCh <-chan hub.Event
	initCh := (<-chan (<-chan hub.Event))(c.initCh)

	for {
		select {
		case <-c.ctx.Done():
			return
		case ch, ok := <-initCh:
			if !ok {
				return
			}
			evCh = ch
			initCh = nil
		case ev, ok := <-evCh:
			if !ok {
				// Channel closed (eviction or explicit Unsubscribe).
				return
			}
			if err := c.writeEvent(ev); err != nil {
				return
			}
		case <-pingTicker.C:
			if err := c.writePing(); err != nil {
				return
			}
		}
	}
}

func (c *connection) writeEvent(ev hub.Event) error {
	if c.h.deps.WriteTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.h.deps.WriteTimeout))
	}
	return c.conn.WriteJSON(ev)
}

func (c *connection) writePing() error {
	if c.h.deps.WriteTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.h.deps.WriteTimeout))
	}
	return c.conn.WriteMessage(websocket.PingMessage, nil)
}

func (c *connection) writeError(code, msg string) {
	if c.h.deps.WriteTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.h.deps.WriteTimeout))
	}
	payload, _ := json.Marshal(errorMsg{Type: "error", Code: code, Message: msg})
	_ = c.conn.WriteMessage(websocket.TextMessage, payload)
}

// errCode maps AuthHub errors to stable string codes clients can dispatch
// on without parsing human messages.
func errCode(err error) string {
	switch {
	case errors.Is(err, hub.ErrNotMember):
		return "not_member"
	case errors.Is(err, hub.ErrInvalidTopic):
		return "invalid_topic"
	default:
		return "subscribe_failed"
	}
}

// SessionResolverFunc adapts a plain function to SessionResolver; useful in
// tests that want to inject a stub without defining a type.
type SessionResolverFunc func(ctx context.Context, sessionID string) (auth.User, error)

// Resolve implements SessionResolver.
func (f SessionResolverFunc) Resolve(ctx context.Context, sessionID string) (auth.User, error) {
	return f(ctx, sessionID)
}

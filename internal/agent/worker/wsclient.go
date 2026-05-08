// WSClient is the satellites-agent's gorilla/websocket subscriber to
// the substrate's hub. It dials the configured /ws endpoint, sends a
// subscribe message per workspace, filters incoming events to the
// "task.published" kind, and emits one WakeEvent per match onto a
// shared wake channel. Worker.Loop's select consumes that channel as
// the event-driven trigger to call Claim immediately, ahead of the
// idle-backoff fallback.
//
// Reconnect: each per-workspace goroutine reconnects with exponential
// backoff bounded by [WSConfig.MinBackoff, WSConfig.MaxBackoff]. The
// last-observed event id per workspace is preserved across
// disconnects and threaded back into the subsequent subscribe as
// since_id, so events that arrived during the disconnect window
// replay from the server's ring buffer.
//
// Backpressure: the wake channel is buffered to 64. When full, the
// wsclient drops the new event and logs worker_wake_dropped — the
// next Claim cycle picks up whatever is queued anyway, since wake
// events carry no ack semantics.

package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ternarybob/arbor"
)

// wakeChanBuffer matches the hub's per-subscriber buffer; lets the
// loop coast through bursts without forcing a drop.
const wakeChanBuffer = 64

// WSConfig parameterises a WSClient. SubscribeWorkspaceIDs MUST be
// non-empty; the client opens one connection per workspace.
type WSConfig struct {
	HubURL                string
	AuthToken             string
	SubscribeWorkspaceIDs []string
	SinceID               string
	MinBackoff            time.Duration
	MaxBackoff            time.Duration
	// Dialer overrides the gorilla websocket.Dialer used for tests.
	// Nil → websocket.DefaultDialer.
	Dialer *websocket.Dialer
}

// WSClient subscribes to per-workspace topics on the substrate's hub
// and emits WakeEvents to a single wake channel.
type WSClient struct {
	cfg    WSConfig
	logger arbor.ILogger

	mu         sync.Mutex
	wakeCh     chan WakeEvent
	cancelDial context.CancelFunc
	dialDoneCh chan struct{}
}

// NewWSClient constructs a WSClient. The returned value is inert
// until Run is called.
func NewWSClient(cfg WSConfig, logger arbor.ILogger) *WSClient {
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = cfg.MinBackoff
	}
	if cfg.Dialer == nil {
		cfg.Dialer = websocket.DefaultDialer
	}
	return &WSClient{cfg: cfg, logger: logger}
}

// Run starts the per-workspace dial loops and returns the read-only
// wake channel. The channel is closed only after Run's parent ctx is
// canceled and all dial goroutines have returned. Calling Run a
// second time on the same WSClient panics — it is single-shot.
func (c *WSClient) Run(ctx context.Context) <-chan WakeEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wakeCh != nil {
		panic("worker: WSClient.Run called twice")
	}
	c.wakeCh = make(chan WakeEvent, wakeChanBuffer)
	dialCtx, cancel := context.WithCancel(ctx)
	c.cancelDial = cancel
	c.dialDoneCh = make(chan struct{})

	var wg sync.WaitGroup
	for _, ws := range c.cfg.SubscribeWorkspaceIDs {
		wg.Add(1)
		go func(workspaceID string) {
			defer wg.Done()
			c.dialLoop(dialCtx, workspaceID, c.cfg.SinceID)
		}(ws)
	}
	go func() {
		wg.Wait()
		close(c.wakeCh)
		close(c.dialDoneCh)
	}()
	return c.wakeCh
}

// dialLoop owns one workspace's connection lifecycle. It dials,
// subscribes, drains incoming events, and reconnects on any failure
// until ctx is cancelled. The since_id cursor is updated on every
// emitted event so a subsequent subscribe replays only what was
// missed.
func (c *WSClient) dialLoop(ctx context.Context, workspaceID, sinceID string) {
	backoff := c.cfg.MinBackoff
	for ctx.Err() == nil {
		ok, lastID := c.runOneConn(ctx, workspaceID, sinceID)
		if lastID != "" {
			sinceID = lastID
		}
		if ctx.Err() != nil {
			return
		}
		if ok {
			// Successful drain (server-side close) — reset backoff.
			backoff = c.cfg.MinBackoff
		} else {
			if c.logger != nil {
				c.logger.Debug().Str("workspace_id", workspaceID).Dur("backoff", backoff).Msg("ws reconnect backoff")
			}
		}
		// Sleep with ctx-cancel.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
}

// subscribeMsg mirrors wshandler.subscribeMsg.
type wsSubscribeMsg struct {
	Type    string `json:"type"`
	Topic   string `json:"topic"`
	SinceID string `json:"since_id,omitempty"`
}

// inboundEvent is the subset of hub.Event the wsclient consumes.
// Data is decoded as a map so the task_id / project_id payload fields
// can be extracted into a WakeEvent.
type inboundEvent struct {
	ID          string         `json:"ID"`
	Topic       string         `json:"Topic"`
	Kind        string         `json:"Kind"`
	WorkspaceID string         `json:"WorkspaceID"`
	Data        map[string]any `json:"Data"`
}

// runOneConn dials the hub once, sends the subscribe, and pumps
// events until the connection closes. Returns (clean, lastID) where
// clean is true when the loop exited because the server closed the
// channel cleanly (vs an unrecoverable read/write error).
func (c *WSClient) runOneConn(ctx context.Context, workspaceID, sinceID string) (bool, string) {
	hdr := http.Header{}
	if c.cfg.AuthToken != "" {
		hdr.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	dialURL := c.cfg.HubURL
	if c.cfg.AuthToken != "" {
		// Query-param fallback for upgraders that strip Authorization.
		if strings.Contains(dialURL, "?") {
			dialURL += "&token=" + c.cfg.AuthToken
		} else {
			dialURL += "?token=" + c.cfg.AuthToken
		}
	}

	conn, _, err := c.cfg.Dialer.DialContext(ctx, dialURL, hdr)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn().Str("workspace_id", workspaceID).Str("error", err.Error()).Msg("ws dial failed")
		}
		return false, sinceID
	}
	defer conn.Close()

	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	sub := wsSubscribeMsg{Type: "subscribe", Topic: "ws:" + workspaceID, SinceID: sinceID}
	if err := conn.WriteJSON(sub); err != nil {
		if c.logger != nil {
			c.logger.Warn().Str("workspace_id", workspaceID).Str("error", err.Error()).Msg("ws subscribe write failed")
		}
		return false, sinceID
	}

	// Drain. Cancel-on-ctx wakes the read by setting a near-zero
	// deadline; the close in the deferred Close above frees the
	// goroutine if the deadline path is hit.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		case <-stop:
		}
	}()

	lastID := sinceID
	clean := false
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				clean = true
			}
			return clean, lastID
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		var ev inboundEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			if c.logger != nil {
				c.logger.Debug().Str("error", err.Error()).Msg("ws unmarshal failed")
			}
			continue
		}
		if ev.ID != "" {
			lastID = ev.ID
		}
		if ev.Kind != "task.published" {
			continue
		}
		taskID, _ := ev.Data["task_id"].(string)
		projectID, _ := ev.Data["project_id"].(string)
		wake := WakeEvent{
			TaskID:      taskID,
			ProjectID:   projectID,
			WorkspaceID: ev.WorkspaceID,
		}
		select {
		case c.wakeCh <- wake:
		default:
			if c.logger != nil {
				c.logger.Warn().Str("workspace_id", workspaceID).Str("task_id", taskID).Msg("worker_wake_dropped")
			}
		}
	}
}

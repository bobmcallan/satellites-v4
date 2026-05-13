package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/config"
)

func TestHealthzShape(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Port: 0, Env: "dev", LogLevel: "info", DevMode: true}
	s := New(cfg, satarbor.New("info"), time.Now().Add(-2*time.Second))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	for _, k := range []string{"version", "build", "commit", "started_at", "uptime_seconds"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing key %q in healthz payload", k)
		}
	}
	if uptime, ok := body["uptime_seconds"].(float64); !ok || uptime < 1 {
		t.Errorf("uptime_seconds = %v, want >=1", body["uptime_seconds"])
	}
}

// TestHealthzDBCheck verifies that an attached HealthCheck flips both the
// JSON payload and the HTTP status: a healthy ping keeps 200 + db_ok:true,
// a failing ping returns 503 + db_ok:false + db_error. Fly's /api/health
// probe relies on the status code to replace machines holding stale
// SurrealDB sockets — a 200-on-failure response masks the outage.
func TestHealthzDBCheck(t *testing.T) {
	t.Parallel()

	t.Run("healthy ping returns 200", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Port: 0, Env: "dev", LogLevel: "info", DevMode: true}
		s := New(cfg, satarbor.New("info"), time.Now())
		s.SetHealthCheck(func(context.Context) error { return nil })

		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("json decode: %v", err)
		}
		if got, ok := body["db_ok"].(bool); !ok || !got {
			t.Errorf("db_ok = %v, want true", body["db_ok"])
		}
		if _, present := body["db_error"]; present {
			t.Errorf("db_error must be absent on healthy ping; got %v", body["db_error"])
		}
	})

	t.Run("failing ping returns 503", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Port: 0, Env: "dev", LogLevel: "info", DevMode: true}
		s := New(cfg, satarbor.New("info"), time.Now())
		s.SetHealthCheck(func(context.Context) error {
			return errors.New("db: ping: write: broken pipe")
		})

		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("json decode: %v", err)
		}
		if got, ok := body["db_ok"].(bool); !ok || got {
			t.Errorf("db_ok = %v, want false", body["db_ok"])
		}
		if msg, _ := body["db_error"].(string); !strings.Contains(msg, "broken pipe") {
			t.Errorf("db_error = %q, want substring 'broken pipe'", msg)
		}
	})
}

// stubLLMPinger implements LLMPinger for tests. configured + pingErr
// drive the three observable outcomes (ok / unreachable / not_configured).
type stubLLMPinger struct {
	configured bool
	pingErr    error
}

func (s *stubLLMPinger) Configured() bool             { return s.configured }
func (s *stubLLMPinger) Ping(_ context.Context) error { return s.pingErr }

// TestHealthzGeminiField (sty_558c0431) — confirms the LLM probe lands
// in the `gemini` field of /healthz with the V3 enum and never flips
// the HTTP status (the LLM is not on Fly's machine-probe critical path).
func TestHealthzGeminiField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		pinger LLMPinger
		want   string
	}{
		{name: "no pinger attached", pinger: nil, want: ""},
		{name: "not configured", pinger: &stubLLMPinger{configured: false}, want: "not_configured"},
		{name: "configured + reachable", pinger: &stubLLMPinger{configured: true, pingErr: nil}, want: "ok"},
		{name: "configured + unreachable", pinger: &stubLLMPinger{configured: true, pingErr: errors.New("dial")}, want: "unreachable"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Port: 0, Env: "dev", LogLevel: "info", DevMode: true}
			s := New(cfg, satarbor.New("info"), time.Now())
			if tc.pinger != nil {
				s.SetLLMPinger(tc.pinger)
			}

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (LLM failure must not flip 503)", rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("json decode: %v", err)
			}
			got, _ := body["gemini"].(string)
			if got != tc.want {
				t.Errorf("gemini = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSecurityHeaders_AllPresent covers AC1+AC2 of story_d5652302.
// All non-HSTS headers ship on every endpoint regardless of env; HSTS
// is gated on prod (story_d5652302 — dev hits over plain HTTP).
func TestSecurityHeaders_AllPresent(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Port: 0, Env: "dev", LogLevel: "info"}
	s := New(cfg, satarbor.New("info"), time.Now())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)

	got := rec.Header()
	for _, want := range []struct{ key, contains string }{
		{"X-Frame-Options", "DENY"},
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
		{"Content-Security-Policy", "default-src 'self'"},
		{"Content-Security-Policy", "https://cdn.jsdelivr.net"},
		{"Content-Security-Policy", "https://fonts.googleapis.com"},
		{"Content-Security-Policy", "https://fonts.gstatic.com"},
		{"Content-Security-Policy", "'unsafe-inline'"},
		// Inline SVG caret arrows for <select> elements (story_cffd92d4).
		{"Content-Security-Policy", "img-src 'self' data:"},
	} {
		v := got.Get(want.key)
		if v == "" {
			t.Errorf("missing header %q", want.key)
			continue
		}
		if !strings.Contains(v, want.contains) {
			t.Errorf("header %q = %q, missing substring %q", want.key, v, want.contains)
		}
	}
	if got.Get("Strict-Transport-Security") != "" {
		t.Errorf("dev env emitted HSTS; should be prod-only")
	}
	// 'unsafe-eval' MUST be absent — story_739823eb completed the
	// @alpinejs/csp migration, removing the script-src grant the
	// standard Alpine build required.
	if csp := got.Get("Content-Security-Policy"); strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("CSP must not contain 'unsafe-eval'; got %q", csp)
	}
}

// TestSecurityHeaders_HSTSGatedOnProd verifies HSTS only ships in prod.
func TestSecurityHeaders_HSTSGatedOnProd(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Port: 0, Env: "prod", LogLevel: "info"}
	s := New(cfg, satarbor.New("info"), time.Now())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)

	hsts := rec.Header().Get("Strict-Transport-Security")
	if hsts == "" {
		t.Fatalf("prod env did not emit HSTS")
	}
	if !strings.Contains(hsts, "max-age=31536000") || !strings.Contains(hsts, "includeSubDomains") {
		t.Errorf("HSTS = %q, want max-age=31536000 + includeSubDomains", hsts)
	}
}

func TestRequestIDMiddlewareInjects(t *testing.T) {
	t.Parallel()
	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = satarbor.RequestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)
	if seen == "" {
		t.Fatal("expected request id to be injected into context")
	}
	if echoed := rec.Header().Get("X-Request-ID"); echoed != seen {
		t.Errorf("header echo = %q, context = %q", echoed, seen)
	}
}

func TestRequestIDMiddlewarePreservesInbound(t *testing.T) {
	t.Parallel()
	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = satarbor.RequestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req_supplied")
	h.ServeHTTP(rec, req)
	if seen != "req_supplied" {
		t.Errorf("seen = %q, want req_supplied", seen)
	}
}

// TestAccessLogPreservesFlusher is the regression for sty_8c17b89d iter-2.
// Iter-1 shipped the SSE endpoint /api/v1/task/log/stream and its
// integration test (TestTaskLogStream_EndToEnd) passed in CI, but pprod
// returned 500 ("streaming not supported") because the accessLog
// middleware wraps the ResponseWriter in *statusRecorder, which shadowed
// http.Flusher in exactly the same way it shadowed http.Hijacker before
// story_fb6ac2d8. Two callsite shapes must succeed after the fix:
//
//   - The legacy `w.(http.Flusher)` type-assertion — covered by the
//     explicit Flush() method on statusRecorder.
//   - The Go 1.20+ http.NewResponseController(w).Flush() — covered by
//     the Unwrap() method, which lets the controller walk the chain.
//
// This test exercises both against the production handler chain
// (SecurityHeaders → requestID → accessLog → mux) constructed via
// httpserver.New so the chain shape exactly matches cmd/satellites-server.
func TestAccessLogPreservesFlusher(t *testing.T) {
	t.Parallel()

	const probePath = "/__flusher_probe"

	// SSE-style handler that exercises both Flusher discovery paths.
	// The legacy `w.(http.Flusher)` assertion is checked first (before
	// the response is committed) so a regression there surfaces as 500.
	// The NewResponseController probe runs after headers are written —
	// a regression there surfaces as an in-body `ERR:` frame so the
	// assertion below catches it without losing the controller's error
	// text.
	probe := func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			http.Error(w, "type-assert w.(http.Flusher) failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			_, _ = w.Write([]byte("data: ERR: " + err.Error() + "\n\n"))
			return
		}
		_, _ = w.Write([]byte("id: 1\ndata: {\"ok\":true}\n\n"))
		_ = http.NewResponseController(w).Flush()
	}

	cfg := &config.Config{Port: 0, Env: "dev", LogLevel: "info", DevMode: true}
	s := New(cfg, satarbor.New("info"), time.Now())
	s.Mount(probePath, http.HandlerFunc(probe))

	srv := httptest.NewServer(s.http.Handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+probePath, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("probe status = %d body=%q — production middleware chain dropped Flusher",
			resp.StatusCode, string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "ERR:") {
		t.Errorf("body contains ResponseController.Flush error frame: %q", string(body))
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("body = %q, want SSE frame containing ok:true", string(body))
	}
}

// TestAccessLogPreservesHijacker is the regression for story_fb6ac2d8
// (WS indicator orange→red on /). The accessLog middleware wraps the
// ResponseWriter in *statusRecorder; before the fix the wrapper shadowed
// http.Hijacker, which caused gorilla/websocket's Upgrade to reject the
// connection with a 500 ("response does not implement http.Hijacker") and
// left the nav indicator stuck in reconnecting → disconnected.
func TestAccessLogPreservesHijacker(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	})
	mux := http.NewServeMux()
	mux.Handle("/ws", wsHandler)
	wrapped := requestID(accessLog(satarbor.New("info"), mux))

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	wsURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	wsURL.Scheme = "ws"
	wsURL.Path = "/ws"

	dialer := websocket.Dialer{
		NetDialContext:   (&net.Dialer{}).DialContext,
		HandshakeTimeout: 2 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := dialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("websocket dial through accessLog middleware failed: status=%d err=%v", got, err)
	}
	defer conn.Close()
}

package httpserver

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/ternarybob/arbor"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
)

// ContentSecurityPolicy is the served value of the CSP header. The portal
// emits inline <script> blocks for Alpine init, fetches Alpine itself
// from cdn.jsdelivr.net, and pulls Google Fonts for the wordmark, so the
// policy permits each source explicitly. Tightening to nonces is a
// follow-up — story_d5652302 names the CDN allow-listing as the
// pragmatic v4 baseline.
//
// 'unsafe-eval' is granted to script-src because Alpine.js v3's standard
// build evaluates inline directives (x-data, x-show, :class, …) via the
// Function() constructor (story_a7297367). Without it every Alpine page
// — nav dropdown, workspace switcher, ws-indicator, tasks board — is
// silently broken in production. Removing it requires migrating to the
// @alpinejs/csp build (Alpine.data factories instead of inline
// expressions), which is tracked as a follow-up so the unsafe-eval
// grant can be dropped later.
//
// Exported so test harnesses (tests/portalui) can apply the same policy
// the production server emits, ensuring chromedp tests run under the
// same CSP regime as pprod.
const ContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.jsdelivr.net; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"connect-src 'self' ws: wss:"

// SecurityHeaders injects the v4 security-header baseline on every
// response: CSP, X-Frame-Options, X-Content-Type-Options,
// Referrer-Policy, and Strict-Transport-Security (prod only). HSTS is
// gated on prod because dev/local hits 127.0.0.1 over plain HTTP and
// HSTS would lock the browser into HTTPS for the dev hostname. See
// story_d5652302.
//
// Exported so the portalui chromedp harness can wrap its in-process
// server in the same middleware as production (story_a7297367). Pass
// prod=false from tests; HSTS only emits in real prod.
func SecurityHeaders(prod bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if prod {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// requestID injects a UUID v4 into the request context when the inbound
// request does not carry an X-Request-ID header. The value is also echoed on
// the response so clients can correlate logs.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := satarbor.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog wraps next and emits one arbor Info line per request on complete,
// carrying method, path, status, duration_ms, and request_id.
func accessLog(logger arbor.ILogger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", sw.status).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Str("request_id", satarbor.RequestIDFrom(r.Context())).
			Msg("http access")
	})
}

// statusRecorder captures the response status for access logging. net/http's
// ResponseWriter doesn't expose it directly.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack delegates to the underlying ResponseWriter when it implements
// http.Hijacker so middleware composition does not break protocol-upgrade
// paths (e.g. the gorilla/websocket /ws upgrade). Without this passthrough
// gorilla/websocket fails the upgrade with "response does not implement
// http.Hijacker" and the client receives a 500, leaving the nav indicator
// stuck in reconnecting → disconnected.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpserver: underlying ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
}

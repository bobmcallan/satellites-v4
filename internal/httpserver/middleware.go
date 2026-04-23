package httpserver

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/ternarybob/arbor"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
)

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

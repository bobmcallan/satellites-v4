// Package httpserver is the satellites-v4 HTTP surface. It owns routing,
// middleware wiring, and lifecycle (start / graceful shutdown). Handlers are
// defined as package-level methods on *Server; new endpoints register in
// routes().
package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/config"
)

// Server wraps net/http.Server with the satellites-specific context: the
// validated runtime Config, the arbor logger used for access logs, and the
// process-start time used by /healthz to compute uptime.
type Server struct {
	cfg       *config.Config
	logger    arbor.ILogger
	http      *http.Server
	startedAt time.Time
	mux       *http.ServeMux

	healthCheck atomic.Pointer[HealthCheck]
}

// SetHealthCheck swaps in an optional liveness hook used by /healthz. Safe
// to call after New. Pass nil to detach.
func (s *Server) SetHealthCheck(h HealthCheck) {
	if h == nil {
		s.healthCheck.Store(nil)
		return
	}
	s.healthCheck.Store(&h)
}

// Mount adds a handler at the given pattern. Must be called before Start.
// Used to wire the MCP handler at /mcp without coupling httpserver to mcp-go.
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// RouteRegistrar is anything that can attach its own routes to a mux. The
// auth handlers satisfy this; later stories can plug in MCP + portal.
type RouteRegistrar interface {
	Register(mux *http.ServeMux)
}

// HealthCheck is the optional hook /healthz calls to expose liveness of a
// downstream dependency (e.g. SurrealDB). Returns nil when the dependency
// is healthy; a non-nil error is rendered as `db_ok:false` + `db_error:<msg>`.
type HealthCheck func(ctx context.Context) error

// New constructs a Server that listens on cfg.Port, uses logger for request
// and lifecycle logs, and stamps /healthz with the supplied startedAt instant.
// Additional routes are registered via the variadic registrars.
func New(cfg *config.Config, logger arbor.ILogger, startedAt time.Time, registrars ...RouteRegistrar) *Server {
	s := &Server{
		cfg:       cfg,
		logger:    logger,
		startedAt: startedAt,
		mux:       http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	for _, r := range registrars {
		r.Register(s.mux)
	}

	handler := requestID(accessLog(logger, s.mux))

	s.http = &http.Server{
		Addr:              net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Start runs the HTTP server until the context is cancelled; then it runs
// Shutdown with a 10-second drain bound. Returns the first non-nil error from
// either ListenAndServe (if not http.ErrServerClosed) or Shutdown.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info().
			Str("addr", s.http.Addr).
			Str("env", s.cfg.Env).
			Msg("http server listening")
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info().Msg("shutdown signal received — draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		if err := <-errCh; err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// healthz returns the process's liveness + identity metadata as JSON. Uptime
// is computed against s.startedAt — the caller's notion of "process start",
// not "server bind time". When a HealthCheck is attached, the payload also
// carries db_ok (+ db_error on failure).
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"version":        config.Version,
		"build":          config.Build,
		"commit":         config.GitCommit,
		"started_at":     s.startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	}
	if hc := s.healthCheck.Load(); hc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := (*hc)(ctx); err != nil {
			payload["db_ok"] = false
			payload["db_error"] = err.Error()
		} else {
			payload["db_ok"] = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

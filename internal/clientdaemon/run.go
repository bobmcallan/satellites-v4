package clientdaemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Run is the foreground main loop: bind the unix socket, serve the
// HTTP/JSON endpoints, drive the scheduler, block on ctx.Done, then
// drain. The caller (cmd/satellites-client/serve.go) wires SIGTERM
// to ctx cancellation.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.opts.SocketPath), 0o755); err != nil {
		return fmt.Errorf("clientdaemon: mkdir socket dir: %w", err)
	}
	_ = os.Remove(d.opts.SocketPath) // stale socket cleanup
	listener, err := net.Listen("unix", d.opts.SocketPath)
	if err != nil {
		return fmt.Errorf("clientdaemon: listen unix %q: %w", d.opts.SocketPath, err)
	}
	if err := os.Chmod(d.opts.SocketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("clientdaemon: chmod socket: %w", err)
	}
	d.info("daemon listening", "socket", d.opts.SocketPath, "parallelism", d.opts.Parallelism)

	state, err := LoadState(d.opts.StatePath)
	if err != nil {
		d.warn("load state on boot", err)
	}
	if err := d.ReconcileBoot(ctx, state); err != nil {
		d.warn("reconcile boot", err)
	}

	srv := &http.Server{Handler: d.mux()}
	srvErrCh := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErrCh <- err
		} else {
			srvErrCh <- nil
		}
	}()

	slots := make(chan struct{}, d.opts.Parallelism)
	schedCtx, schedCancel := context.WithCancel(ctx)
	go d.runScheduler(schedCtx, slots)

	defer func() {
		schedCancel()
		// Best-effort socket file cleanup on exit.
		_ = os.Remove(d.opts.SocketPath)
	}()

	select {
	case <-ctx.Done():
	case err := <-srvErrCh:
		if err != nil {
			return err
		}
	}

	// Drain.
	drainCtx, cancel := context.WithTimeout(context.Background(), d.opts.DrainTimeout+5*time.Second)
	defer cancel()
	drainErr := d.Drain(drainCtx, d.opts.DrainTimeout)
	if drainErr != nil {
		d.warn("drain", drainErr)
	}

	// Stop the HTTP server (idempotent if already shut down).
	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sCancel()
	_ = srv.Shutdown(shutdownCtx)

	return drainErr
}

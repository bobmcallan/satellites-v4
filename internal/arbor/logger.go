// Package arbor wraps the ternarybob/arbor structured logger with satellites-v4
// defaults: stdout-only console writer (Fly captures stdout), configurable
// level, and context helpers for request-id propagation.
package arbor

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/ternarybob/arbor"
	arbormodels "github.com/ternarybob/arbor/models"
)

type ctxKey int

const requestIDKey ctxKey = iota

var (
	defaultOnce sync.Once
	defaultLog  arbor.ILogger
)

// Default returns a process-wide arbor logger with level "info" and a console
// writer on stdout. Safe for use before config load (boot-time errors).
func Default() arbor.ILogger {
	defaultOnce.Do(func() {
		defaultLog = New("info")
	})
	return defaultLog
}

// New builds an arbor logger at the given level with a console writer on
// stdout. Level strings parseable by arbor include "trace", "debug", "info",
// "warn", "error". Unknown levels fall back to arbor's own default.
func New(level string) arbor.ILogger {
	return arbor.NewLogger().
		WithConsoleWriter(arbormodels.WriterConfiguration{
			Type:       arbormodels.LogWriterTypeConsole,
			Writer:     os.Stdout,
			TimeFormat: "2006-01-02T15:04:05Z07:00",
		}).
		WithLevelFromString(level)
}

// NewWithFile builds an arbor logger at the given level with both a
// console writer (matching New) AND a file writer rooted at logDir.
// arbor's FileWriter applies its own daily rotation + size-based
// rollover (defaults: 500KB rolling files, 20 backups) — the satellites
// caller passes the directory and the basename so each binary writes
// to its own log file (e.g. "satellites-agent.log" / "satellites-client.log").
//
// logDir is created on first write via phuslu/log's EnsureFolder
// option; an unwritable path surfaces as a write-time error rather
// than aborting boot. An empty logDir is a programming error — call
// New() instead. sty_92bfd9e6 / sty_b1345841.
func NewWithFile(level, logDir, basename string) arbor.ILogger {
	if logDir == "" {
		return New(level)
	}
	fileName := filepath.Join(logDir, basename)
	return arbor.NewLogger().
		WithConsoleWriter(arbormodels.WriterConfiguration{
			Type:       arbormodels.LogWriterTypeConsole,
			Writer:     os.Stdout,
			TimeFormat: "2006-01-02T15:04:05Z07:00",
		}).
		WithFileWriter(arbormodels.WriterConfiguration{
			Type:       arbormodels.LogWriterTypeFile,
			FileName:   fileName,
			TimeFormat: "2006-01-02T15:04:05Z07:00",
		}).
		WithLevelFromString(level)
}

// WithRequestID attaches a request ID to ctx; downstream code pulls it out via
// RequestIDFrom and attaches it as a structured field on every log line.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request ID stored on ctx, or "" if none.
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// Package arbor wraps the ternarybob/arbor structured logger with satellites-v4
// defaults: stdout-only console writer (Fly captures stdout), configurable
// level, and context helpers for request-id propagation.
package arbor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ternarybob/arbor"
	arbormodels "github.com/ternarybob/arbor/models"
)

// LogRetentionCount caps how many rolled-over log files are kept per
// per-binary basename stem. Each invocation writes a fresh timestamped
// file (e.g. satellites-client.<ts>.log); without retention the dir
// grows monotonically across operator sessions. sty_29d2dc1d.
const LogRetentionCount = 20

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
	logger := arbor.NewLogger().
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
	pruneOldLogs(logDir, basenameStem(basename), LogRetentionCount)
	return logger
}

// basenameStem strips the trailing ".log" suffix from a basename so
// the retention sweep can match the per-binary prefix. Inputs without
// the suffix are returned unchanged.
func basenameStem(basename string) string {
	return strings.TrimSuffix(basename, ".log")
}

// pruneOldLogs trims <logDir>/<stem>.*.log to the `keep` newest files
// by mtime. The current invocation's file is by construction one of
// the newest (arbor opened it before this runs). The bare
// "<stem>.log" pointer (when present) is skipped — that's the
// stable symlink/latest marker, not a rolled-over file.
//
// Best-effort: any os error short-circuits the function. The
// satellites caller should not propagate retention failures up to
// the boot path — losing a stale log row is acceptable; losing the
// CLI invocation is not.
func pruneOldLogs(logDir, stem string, keep int) {
	if logDir == "" || stem == "" || keep <= 0 {
		return
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	prefix := stem + "."
	var matches []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == stem+".log" {
			continue
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		matches = append(matches, e)
	}
	sort.Slice(matches, func(i, j int) bool {
		ii, _ := matches[i].Info()
		ji, _ := matches[j].Info()
		return ii.ModTime().After(ji.ModTime())
	})
	for idx, e := range matches {
		if idx < keep {
			continue
		}
		_ = os.Remove(filepath.Join(logDir, e.Name()))
	}
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

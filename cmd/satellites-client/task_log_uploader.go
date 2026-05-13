// task_log_uploader.go — implements the per-run chunk uploader for
// `satellites-client task run`. Wraps stdout / stderr, batches complete
// lines, and emits one task_log_append per batch via the typed cliremote
// surface. Flushes when 100 lines accumulate OR 250 ms elapse since the
// last flush, whichever fires first (sty_8c17b89d AC2).
//
// The uploader is fire-and-forget: local terminal output goes through
// the existing MultiWriter chain in internal/agent/worker/client_claude.go
// unchanged; upload errors are logged at the caller's logger and never
// abort the dispatched task.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/cliremote"
)

const (
	chunkFlushLineCount = 100
	chunkFlushInterval  = 250 * time.Millisecond
)

// nextSeqFunc returns the next monotonic seq for the run. The lifecycle
// + chunk uploaders share one counter so the SSE stream sees a
// strictly-monotonic seq across all kinds.
type nextSeqFunc func() int64

// taskLogUploader implements io.Writer. Each Write splits the buffer
// at '\n' boundaries; complete lines accumulate in `lines`. A flush
// drains `lines` into one task_log_append call.
type taskLogUploader struct {
	api     *cliremote.Client
	logger  arbor.ILogger
	taskID  string
	wsID    string
	projID  string
	kind    string
	nextSeq nextSeqFunc

	mu          sync.Mutex
	leftover    []byte
	lines       []string
	lastFlushAt time.Time

	bytesOut int64
	flushOut int

	stopCh chan struct{}
	doneCh chan struct{}
}

// newTaskLogUploader constructs an uploader. The caller MUST call Close
// to flush any buffered remainder and stop the background ticker.
func newTaskLogUploader(api *cliremote.Client, logger arbor.ILogger, taskID, wsID, projID, kind string, nextSeq nextSeqFunc) *taskLogUploader {
	u := &taskLogUploader{
		api:         api,
		logger:      logger,
		taskID:      taskID,
		wsID:        wsID,
		projID:      projID,
		kind:        kind,
		nextSeq:     nextSeq,
		lastFlushAt: time.Now(),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	go u.ticker()
	return u
}

// Write implements io.Writer. Accumulates bytes; splits on \n; flushes
// each completed 100-line chunk while extras remain buffered for the
// next flush trigger. Always returns len(p), nil so it composes safely
// under io.MultiWriter (an upload-side error must not short-circuit the
// local-stdout sibling).
func (u *taskLogUploader) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	u.mu.Lock()
	u.leftover = append(u.leftover, p...)
	for {
		idx := bytes.IndexByte(u.leftover, '\n')
		if idx < 0 {
			break
		}
		line := string(u.leftover[:idx])
		u.leftover = u.leftover[idx+1:]
		u.lines = append(u.lines, line)
	}
	// Carve out as many 100-line chunks as have accumulated. Anything
	// short of 100 stays buffered for the time-tick path. flushBatch
	// is called outside the lock so upload latency cannot stall the
	// local-stdout sibling tee.
	var batches [][]string
	for len(u.lines) >= chunkFlushLineCount {
		batch := append([]string(nil), u.lines[:chunkFlushLineCount]...)
		u.lines = u.lines[chunkFlushLineCount:]
		batches = append(batches, batch)
	}
	if len(batches) > 0 {
		u.lastFlushAt = time.Now()
	}
	u.mu.Unlock()
	for _, b := range batches {
		u.flushBatch(b)
	}
	return len(p), nil
}

// ticker fires every chunkFlushInterval; if any lines are buffered
// it flushes them. Stops when Close() signals stopCh.
func (u *taskLogUploader) ticker() {
	defer close(u.doneCh)
	t := time.NewTicker(chunkFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-u.stopCh:
			return
		case <-t.C:
			u.mu.Lock()
			batch := u.lines
			u.lines = nil
			u.lastFlushAt = time.Now()
			u.mu.Unlock()
			if len(batch) > 0 {
				u.flushBatch(batch)
			}
		}
	}
}

// Close stops the ticker, drains any buffered partial line into a
// final flush, and waits for the ticker goroutine to exit.
func (u *taskLogUploader) Close() {
	select {
	case <-u.stopCh:
		return
	default:
		close(u.stopCh)
	}
	<-u.doneCh
	// Drain anything left (lines + partial leftover treated as a line).
	u.mu.Lock()
	tail := u.lines
	u.lines = nil
	if len(u.leftover) > 0 {
		tail = append(tail, string(u.leftover))
		u.leftover = nil
	}
	u.mu.Unlock()
	if len(tail) > 0 {
		u.flushBatch(tail)
	}
}

// flushBatch issues one task_log_append carrying the chunk's lines.
// Errors log at warn and never block the caller — local-terminal
// output is unaffected (AC2).
func (u *taskLogUploader) flushBatch(lines []string) {
	payload, err := json.Marshal(map[string]any{"lines": lines})
	if err != nil {
		if u.logger != nil {
			u.logger.Warn().Str("error", err.Error()).Msg("task_log payload marshal failed")
		}
		return
	}
	seq := u.nextSeq()
	in := cliremote.TaskLogAppendInput{
		TaskID:      u.taskID,
		WorkspaceID: u.wsID,
		ProjectID:   u.projID,
		Seq:         seq,
		TS:          time.Now().UTC(),
		Kind:        u.kind,
		Payload:     payload,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := u.api.TaskLogAppend(ctx, in); err != nil {
		if u.logger != nil {
			u.logger.Warn().
				Str("task_id", u.taskID).
				Str("kind", u.kind).
				Int64("seq", seq).
				Str("error", err.Error()).
				Msg("task_log_append failed")
		}
		return
	}
	u.mu.Lock()
	u.bytesOut += int64(len(payload))
	u.flushOut++
	u.mu.Unlock()
}

// stats returns the byte + chunk totals the lifecycle finaliser uses
// for the ledger pointer row.
func (u *taskLogUploader) stats() (chunks int, bytesOut int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.flushOut, u.bytesOut
}

// task_log_follower.go — sty_7fc607f5.
//
// Consumer-only SSE follower for the `task_log` stream shipped by
// sty_8c17b89d (server endpoint at GET /api/v1/task/log/stream).
// Wired from `satellites-client task run --follow`: a goroutine that
// subscribes to the stream and prints lifecycle markers
// (start / heartbeat / stop) to stdout as they arrive. stdout / stderr
// chunk frames are intentionally skipped to avoid duplicate echo with
// the local subprocess pipe (task_run.go's io.MultiWriter already
// prints them).
//
// No new substrate primitive. No server-side change. The follower
// imports only stdlib + internal/cliremote types — layering stays
// clean per pr_mcp_cli_shared_path (the SSE wire shape is the
// sanctioned single deviation called out in
// internal/httpserver/api_task_log.go:1-15).

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/cliremote"
)

// followerConfig is the static configuration the follow goroutine
// needs. Constructed at the task_run.go call site so the goroutine
// itself is dependency-light + testable.
type followerConfig struct {
	serverURL  string
	authToken  string
	taskID     string
	out        io.Writer
	logger     arbor.ILogger
	httpClient *http.Client
}

// followBackoffInitial and followBackoffMax bound the reconnect
// backoff curve (1s → 2s → 4s → 8s → 16s → 30s).
const (
	followBackoffInitial = 1 * time.Second
	followBackoffMax     = 30 * time.Second
)

// followTaskLog opens a long-lived SSE connection to the server's
// task_log stream and prints lifecycle markers to cfg.out until the
// `stop` frame closes the stream OR ctx is cancelled.
//
// Reconnect behaviour: on a transient stream disconnect (network
// error, server-side timeout, mid-stream EOF without a stop frame)
// the loop reconnects, sending the last-printed seq via
// Last-Event-ID so the server resumes at seq+1
// (api_task_log.go:110-112). Exponential backoff caps at 30s.
// On a successful connection the backoff resets.
//
// Errors that prevent any forward progress (auth failure, bad
// task_id, ctx cancel) return; transient errors are logged at Debug
// and trigger a reconnect.
func followTaskLog(ctx context.Context, cfg followerConfig) error {
	if cfg.httpClient == nil {
		// No per-request timeout — SSE connections live for the
		// duration of the task run (potentially 20+ minutes).
		cfg.httpClient = &http.Client{}
	}
	lastSeq := int64(-1)
	backoff := followBackoffInitial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sawStop, perr := followOnce(ctx, cfg, &lastSeq)
		if sawStop {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if perr != nil && cfg.logger != nil {
			cfg.logger.Debug().
				Str("task_id", cfg.taskID).
				Str("error", perr.Error()).
				Int64("last_seq", lastSeq).
				Msg("task_log follower reconnect")
		}
		// Sleep with cancel-awareness, then double the backoff
		// (capped). A successful connection inside followOnce
		// resets backoff before the next loop iteration.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > followBackoffMax {
			backoff = followBackoffMax
		}
	}
}

// followOnce opens one HTTP connection and parses frames until the
// stop frame lands or the stream disconnects. The boolean return
// signals "stop frame observed" (caller exits) vs "transient
// disconnect" (caller reconnects).
//
// On HTTP success (200), backoff is implicitly reset by the caller
// only when followOnce returns sawStop=true OR no error — to keep
// the reset path next to the success path, the caller resets when
// the parse loop processed at least one frame. We surface that via
// updating *lastSeq.
func followOnce(ctx context.Context, cfg followerConfig, lastSeq *int64) (sawStop bool, err error) {
	url := strings.TrimRight(cfg.serverURL, "/") +
		"/api/v1/task/log/stream?task_id=" + cfg.taskID
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if rerr != nil {
		return false, rerr
	}
	req.Header.Set("Accept", "text/event-stream")
	if cfg.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.authToken)
	}
	if *lastSeq >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(*lastSeq, 10))
	}

	resp, doErr := cfg.httpClient.Do(req)
	if doErr != nil {
		return false, doErr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 4xx is non-retryable; 5xx and 408 are retryable. Return an
		// error either way; the caller decides on ctx-cancel checks.
		return false, fmt.Errorf("task_log stream: HTTP %d", resp.StatusCode)
	}

	return parseFrames(resp.Body, cfg.out, lastSeq)
}

// parseFrames reads SSE frames from r and prints one line per
// lifecycle frame to out. Returns sawStop=true once a `stop` frame
// has been processed. On reader error (EOF, network drop) returns
// the wrapped error; the caller reconnects when sawStop is false.
//
// SSE frame format: a sequence of `field: value\n` lines terminated
// by a blank line. We track `id:` and accumulate `data:` lines until
// the blank-line dispatcher fires.
func parseFrames(r io.Reader, out io.Writer, lastSeq *int64) (sawStop bool, err error) {
	sc := bufio.NewScanner(r)
	// SSE frames are usually small (a few hundred bytes); a 1MB
	// buffer comfortably handles outliers.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var (
		curID   int64
		hasID   bool
		curData strings.Builder
	)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			v := strings.TrimSpace(line[3:])
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				curID = n
				hasID = true
			}
		case strings.HasPrefix(line, "data:"):
			v := line[5:]
			if strings.HasPrefix(v, " ") {
				v = v[1:]
			}
			curData.WriteString(v)
		case line == "":
			if curData.Len() == 0 {
				continue
			}
			var entry cliremote.TaskLogListEntry
			if jerr := json.Unmarshal([]byte(curData.String()), &entry); jerr == nil {
				if !hasID {
					curID = entry.Seq
				}
				// Only advance lastSeq if this frame is strictly
				// ahead of what we've already printed (defensive —
				// replay of the same seq shouldn't print twice).
				if *lastSeq < 0 || curID > *lastSeq {
					if printed := printFollowerEvent(out, entry); printed {
						*lastSeq = curID
					}
					if entry.Kind == "stop" {
						sawStop = true
					}
				}
			}
			curData.Reset()
			hasID = false
			if sawStop {
				return true, nil
			}
		}
	}
	return sawStop, sc.Err()
}

// printFollowerEvent renders one SSE entry to out as a single
// `[<RFC3339Z>] <kind> <details>` line. Returns true when a line
// was actually written (so the caller can advance lastSeq) and
// false for the skipped kinds (stdout / stderr chunk frames).
func printFollowerEvent(out io.Writer, e cliremote.TaskLogListEntry) bool {
	switch e.Kind {
	case "start", "heartbeat", "stop":
	default:
		return false
	}
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	fmt.Fprintf(out, "[%s] %s %s\n",
		ts.UTC().Format(time.RFC3339), e.Kind, payloadSummary(e.Kind, e.Payload))
	return true
}

// payloadSummary returns the one-line details suffix per kind.
// Unknown / malformed payload yields an empty string — the line
// still prints with timestamp + kind so the operator sees the
// frame.
func payloadSummary(kind string, payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	switch kind {
	case "start":
		var p struct {
			WorkerPID int    `json:"worker_pid"`
			Origin    string `json:"origin"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return ""
		}
		parts := []string{}
		if p.WorkerPID != 0 {
			parts = append(parts, "worker_pid="+strconv.Itoa(p.WorkerPID))
		}
		if p.Origin != "" {
			parts = append(parts, "origin="+p.Origin)
		}
		return strings.Join(parts, " ")
	case "heartbeat":
		var p struct {
			ElapsedMS int64 `json:"elapsed_ms"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return ""
		}
		return "elapsed=" + (time.Duration(p.ElapsedMS) * time.Millisecond).String()
	case "stop":
		var p struct {
			Outcome    string `json:"outcome"`
			ExitCode   int    `json:"exit_code"`
			DurationMS int64  `json:"duration_ms"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return ""
		}
		parts := []string{}
		if p.Outcome != "" {
			parts = append(parts, "outcome="+p.Outcome)
		}
		parts = append(parts, "exit="+strconv.Itoa(p.ExitCode))
		if p.DurationMS != 0 {
			parts = append(parts, "duration="+(time.Duration(p.DurationMS)*time.Millisecond).String())
		}
		return strings.Join(parts, " ")
	}
	return ""
}

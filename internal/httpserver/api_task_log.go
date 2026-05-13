// api_task_log.go — HTTP routes for the task_log substrate primitive.
//
// Two thin-forwarder routes + one long-lived SSE handler:
//
//   - POST /api/v1/task/log/append : forwards to client.TaskLogAppend.
//   - POST /api/v1/task/log/list   : forwards to client.TaskLogList.
//   - GET  /api/v1/task/log/stream : long-lived SSE; replays via
//     client.TaskLogList, then live-tails via client.TaskLogSubscribe.
//     Sanctioned single deviation from pr_mcp_cli_shared_path for the
//     connection-lifetime shape; layering stays clean because the
//     handler imports only internal/client for substrate access
//     (the typed TaskLogSubscribe hides internal/tasklog behind a
//     channel of TaskLogListEntry).
//
// Sty_8c17b89d.

package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/client"
)

func (a *APIRegistrar) handleTaskLogAppend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID      string          `json:"task_id"`
		WorkspaceID string          `json:"workspace_id"`
		ProjectID   string          `json:"project_id"`
		Seq         int64           `json:"seq"`
		TS          string          `json:"ts"`
		Kind        string          `json:"kind"`
		Payload     json.RawMessage `json:"payload"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	in := client.TaskLogAppendInput{
		TaskID:      req.TaskID,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		Seq:         req.Seq,
		Kind:        req.Kind,
		Payload:     req.Payload,
		Memberships: cc.Memberships,
		Now:         time.Now().UTC(),
	}
	if req.TS != "" {
		if t, err := time.Parse(time.RFC3339Nano, req.TS); err == nil {
			in.TS = t
		}
	}
	out, err := a.client.TaskLogAppend(r.Context(), cc, in)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleTaskLogList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID  string `json:"task_id"`
		FromSeq int64  `json:"from_seq"`
		Limit   int    `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.TaskLogList(r.Context(), cc, client.TaskLogListInput{
		TaskID:      req.TaskID,
		FromSeq:     req.FromSeq,
		Limit:       req.Limit,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// handleTaskLogStream is the SSE endpoint. Connection lifecycle:
//
//  1. Validate task_id + caller memberships.
//  2. Honour Last-Event-ID header (defaults to startSeq=0).
//  3. Subscribe BEFORE replay so live rows arriving between the
//     replay query and the subscription set up are not lost.
//  4. Replay via client.TaskLogList(from=startSeq); emit each row.
//  5. Tail the subscribe channel; emit each row.
//  6. Close cleanly after the first kind:"stop" frame.
func (a *APIRegistrar) handleTaskLogStream(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if taskID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "task_id required")
		return
	}
	startSeq := int64(0)
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			startSeq = n + 1
		}
	} else if v := strings.TrimSpace(r.URL.Query().Get("from_seq")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			startSeq = n
		}
	}

	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIStatus(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Subscribe BEFORE replay so live rows arriving between the
	// replay query and the subscription set up are not lost. The
	// consumer reconciles by skipping live rows whose seq <=
	// lastReplayed.
	ch, cancel, err := a.client.TaskLogSubscribe(r.Context(), cc, client.TaskLogSubscribeInput{
		TaskID:      taskID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Replay block.
	replay, err := a.client.TaskLogList(r.Context(), cc, client.TaskLogListInput{
		TaskID:      taskID,
		FromSeq:     startSeq,
		Memberships: cc.Memberships,
	})
	if err != nil {
		// We've already committed to SSE framing; surface the error as
		// a single data frame and stop.
		fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
		return
	}
	lastSeq := startSeq - 1
	sawStop := false
	for _, e := range replay.Entries {
		if e.Seq < startSeq {
			continue
		}
		if werr := writeSSEFrame(w, e); werr != nil {
			return
		}
		flusher.Flush()
		lastSeq = e.Seq
		if e.Kind == "stop" {
			sawStop = true
		}
	}
	if sawStop {
		return
	}

	// Live tail.
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Seq <= lastSeq {
				continue
			}
			if werr := writeSSEFrame(w, e); werr != nil {
				return
			}
			flusher.Flush()
			lastSeq = e.Seq
			if e.Kind == "stop" {
				return
			}
		}
	}
}

// writeSSEFrame writes one `id: <seq>\ndata: <json>\n\n` frame.
func writeSSEFrame(w http.ResponseWriter, e client.TaskLogListEntry) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, body)
	return err
}

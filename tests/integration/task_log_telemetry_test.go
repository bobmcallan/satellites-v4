// task_log_telemetry_test.go — sty_090f6183 AC8.
//
// Boots a real satellites server via testcontainers, exercises the six
// new server-side semantic task_log kinds (claim / tool_call_start /
// tool_call_end / ledger_append / status_change / close) by driving
// the substrate through the typed /api/v1/* HTTP surface, and asserts:
//
//   - every new kind appears with the documented payload shape (AC2);
//   - ordering invariants AC3:
//     * KindClaim precedes any KindToolCall* row;
//     * every KindToolCallStart(tool_name=T) is followed by exactly
//       one KindToolCallEnd(tool_name=T) — no nested unmatched starts;
//     * KindLedgerAppend's ledger_id is retrievable via ledger_get at
//       the moment the SSE consumer reads the frame (read-your-write);
//     * KindStatusChange precedes KindClose when the same TaskUpdate
//       call triggers both.
//
// The producer is direct /api/v1/* HTTP calls — same shape as the
// existing tests/integration/task_log_stream_test.go harness. The
// `task run` dispatched-claude flow is covered in unit tests; here we
// focus on the substrate-side telemetry so the new kinds are covered
// without a real claude binary on the test host.

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestTaskLogTelemetry_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {"surrealdb"},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	docsHost := filepath.Join(repoRoot(t), "docs")
	const envBearer = "tasklog-telemetry-test"
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"SATELLITES_DB_DSN":       "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_DEV_USERNAME": "dev@local",
			"SATELLITES_DEV_PASSWORD": "letmein",
			"SATELLITES_DOCS_DIR":     "/app/docs",
			"SATELLITES_API_KEYS":     envBearer,
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	defer stop()

	// project_add + story_add are reachable only via /api/v1 +
	// satellites-client CLI (sty_4db0e025 C9 removed them from MCP).
	projectResp := callAPIv1(t, ctx, baseURL, envBearer, "project_add", map[string]any{
		"name":        "tasklog-telemetry",
		"description": "tasklog-telemetry integration",
	})
	projectID, _ := projectResp["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", projectResp)
	}
	storyResp := callAPIv1(t, ctx, baseURL, envBearer, "story_add", map[string]any{
		"project_id":          projectID,
		"title":               "tasklog-telemetry",
		"description":         "tasklog-telemetry",
		"acceptance_criteria": "see test",
	})
	storyID, _ := storyResp["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add returned no id: %+v", storyResp)
	}

	// developer_agent is system-scope; document_get's workspace
	// resolution drops it when no caller workspace matches, so list
	// agents and pick the one named developer_agent.
	agentArr := callAPIv1(t, ctx, baseURL, envBearer, "document_list", map[string]any{
		"type":  "agent",
		"limit": 100,
	})
	// developer_agent ships in the operator's workspace seed, not the
	// system-scope seed, so a freshly-minted test project does not see
	// it. Any system-scope agent doc works here because the test
	// omits `action` from task_add — no capability check fires.
	agentID := findAgentIDByName(agentArr, "claude_orchestrator")
	if agentID == "" {
		agentID = firstAgentID(agentArr)
	}
	if agentID == "" {
		t.Fatalf("no system-scope agent doc available via document_list; resp=%+v", agentArr)
	}

	taskResp := callAPIv1(t, ctx, baseURL, envBearer, "task_add", map[string]any{
		"agent_id": agentID,
		"prompt":   "tasklog-telemetry test",
		"story_id": storyID,
	})
	taskID, _ := taskResp["task_id"].(string)
	if taskID == "" {
		t.Fatalf("task_add returned no task_id: %+v", taskResp)
	}

	taskRow := callAPIv1(t, ctx, baseURL, envBearer, "task_get", map[string]any{"id": taskID})
	wsID, _ := taskRow["workspace_id"].(string)
	if wsID == "" {
		t.Fatalf("task_get missing workspace_id: %+v", taskRow)
	}

	// dispatchedAPI carries the X-Satellites-Dispatched-Task-ID header so
	// the server-side telemetry middleware emits tool_call_start +
	// tool_call_end rows. orchestratorAPI is the pre-dispatch claim
	// call — the operator-side CLI claims the task BEFORE spawning the
	// dispatched subprocess, so this call must NOT carry the header.
	dispatchedAPI := func(toolName string, args map[string]any) map[string]any {
		return callAPIv1WithHeader(t, ctx, baseURL, envBearer, toolName, args, taskID)
	}
	orchestratorAPI := func(toolName string, args map[string]any) map[string]any {
		return callAPIv1(t, ctx, baseURL, envBearer, toolName, args)
	}

	// 1. task_claim → KindClaim. The orchestrator claims the task
	// BEFORE dispatching the subprocess in production, so this hit
	// runs without the dispatched-task header — it produces KindClaim
	// but NOT tool_call_start/end. That matches AC3 invariant
	// "KindClaim precedes any KindToolCall*". Running it before we
	// open the SSE stream also guarantees the task_log row range is
	// non-empty so SurrealStore.Subscribe resolves the workspace.
	_ = orchestratorAPI("task_claim", map[string]any{
		"worker_id":    "telemetry-test-worker",
		"workspace_id": wsID,
	})

	// Open the SSE stream now that the substrate has at least one row
	// for the task. The collector replays from seq=0 + live-tails.
	streamCtx, streamCancel := context.WithTimeout(ctx, 60*time.Second)
	defer streamCancel()
	frameCh := make(chan telemetryFrame, 256)
	subscribed := make(chan struct{})
	go collectTelemetryFrames(t, streamCtx, baseURL, envBearer, taskID, frameCh, subscribed)
	select {
	case <-subscribed:
	case <-time.After(15 * time.Second):
		t.Fatalf("SSE subscriber failed to attach within 15s")
	}

	// 2. ledger_append → KindLedgerAppend (tagged with task_id so
	// the publisher's gate fires).
	ledgerResp := dispatchedAPI("ledger_append", map[string]any{
		"project_id": projectID,
		"story_id":   storyID,
		"type":       "evidence",
		"tags":       []any{"task_id:" + taskID, "kind:test-evidence"},
		"content":    "telemetry test evidence",
	})
	appendedLedgerID, _ := ledgerResp["id"].(string)
	if appendedLedgerID == "" {
		t.Fatalf("ledger_append returned no id: %+v", ledgerResp)
	}

	// 3. story_update(status="blocked") → KindStatusChange. The
	// storystatus reconciler auto-flips the story to in_progress
	// when the first open task is observed, so by the time we land
	// here current.Status is already in_progress. Flipping to blocked
	// keeps the test deterministic: blocked has no template hook
	// (feature only gates in_progress + done), and in_progress →
	// blocked is a valid ValidTransition matrix edge.
	_ = dispatchedAPI("story_update", map[string]any{
		"id":     storyID,
		"status": "blocked",
	})

	// 4. task_update(status="closed") → KindStatusChange then KindClose.
	_ = dispatchedAPI("task_update", map[string]any{
		"id":                  taskID,
		"status":              "closed",
		"outcome":             "success",
		"evidence_ledger_ids": []any{appendedLedgerID},
	})

	// Collect frames until KindClose lands (the server-side terminal
	// semantic event) or the deadline fires.
	frames := drainTelemetryFrames(t, streamCtx, frameCh, 30*time.Second)
	if len(frames) == 0 {
		t.Fatalf("no SSE frames received")
	}

	// Group frames by kind for assertions.
	byKind := map[string][]telemetryFrame{}
	for _, f := range frames {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}

	// AC1 / AC2 — every new kind present with the documented payload.
	requiredKinds := []string{"claim", "tool_call_start", "tool_call_end", "ledger_append", "status_change", "close"}
	for _, k := range requiredKinds {
		if len(byKind[k]) == 0 {
			t.Errorf("expected ≥1 %q frame, got 0 (frames=%v)", k, summariseKinds(frames))
		}
	}

	if claim := byKind["claim"]; len(claim) > 0 {
		var p map[string]any
		if err := json.Unmarshal(claim[0].Payload, &p); err != nil {
			t.Errorf("claim payload decode: %v", err)
		} else {
			if p["task_id"] != taskID {
				t.Errorf("claim payload task_id = %v, want %q", p["task_id"], taskID)
			}
			if p["claimed_by"] == nil || p["claimed_by"] == "" {
				t.Errorf("claim payload missing claimed_by: %+v", p)
			}
			if p["claimed_at"] == nil || p["claimed_at"] == "" {
				t.Errorf("claim payload missing claimed_at: %+v", p)
			}
		}
	}

	if la := byKind["ledger_append"]; len(la) > 0 {
		var p map[string]any
		if err := json.Unmarshal(la[0].Payload, &p); err != nil {
			t.Errorf("ledger_append payload decode: %v", err)
		} else {
			if p["ledger_id"] == nil || p["ledger_id"] == "" {
				t.Errorf("ledger_append payload missing ledger_id: %+v", p)
			}
			if p["type"] == nil {
				t.Errorf("ledger_append payload missing type: %+v", p)
			}
			if p["tags_summary"] == nil {
				t.Errorf("ledger_append payload missing tags_summary: %+v", p)
			}
			// Read-your-write: the referenced ledger row must be
			// retrievable at the moment the consumer sees the frame.
			if id, ok := p["ledger_id"].(string); ok && id != "" {
				row := callAPIv1(t, ctx, baseURL, envBearer, "ledger_get", map[string]any{"id": id})
				if got, _ := row["id"].(string); got != id {
					t.Errorf("ledger_get(%q) returned id=%q (row=%+v)", id, got, row)
				}
			}
		}
	}

	if sc := byKind["status_change"]; len(sc) > 0 {
		var p map[string]any
		if err := json.Unmarshal(sc[0].Payload, &p); err != nil {
			t.Errorf("status_change payload decode: %v", err)
		} else {
			if p["from"] == nil || p["to"] == nil {
				t.Errorf("status_change payload missing from/to: %+v", p)
			}
		}
	}

	if cl := byKind["close"]; len(cl) > 0 {
		var p map[string]any
		if err := json.Unmarshal(cl[0].Payload, &p); err != nil {
			t.Errorf("close payload decode: %v", err)
		} else {
			if p["outcome"] != "success" {
				t.Errorf("close payload outcome = %v, want success", p["outcome"])
			}
			if p["evidence_ledger_ids"] == nil {
				t.Errorf("close payload missing evidence_ledger_ids: %+v", p)
			}
		}
	}

	// AC3 ordering invariants.
	firstOf := func(kind string) int {
		for i, f := range frames {
			if f.Kind == kind {
				return i
			}
		}
		return -1
	}
	lastOf := func(kind string) int {
		idx := -1
		for i, f := range frames {
			if f.Kind == kind {
				idx = i
			}
		}
		return idx
	}

	claimIdx := firstOf("claim")
	firstToolStart := firstOf("tool_call_start")
	if claimIdx >= 0 && firstToolStart >= 0 && claimIdx >= firstToolStart {
		t.Errorf("ordering: claim (idx=%d) must precede first tool_call_start (idx=%d)", claimIdx, firstToolStart)
	}

	// Pair tool_call_start with tool_call_end of same tool_name with
	// no nested unmatched start of the same tool.
	open := map[string]int{}
	for _, f := range frames {
		if f.Kind != "tool_call_start" && f.Kind != "tool_call_end" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			continue
		}
		name, _ := p["tool_name"].(string)
		if f.Kind == "tool_call_start" {
			open[name]++
			if open[name] > 1 {
				t.Errorf("nested tool_call_start for %q without paired end", name)
			}
		} else {
			if open[name] <= 0 {
				t.Errorf("tool_call_end for %q with no matching tool_call_start", name)
			} else {
				open[name]--
			}
		}
	}
	for name, n := range open {
		if n != 0 {
			t.Errorf("unbalanced tool_call_start for %q: %d unmatched", name, n)
		}
	}

	statusIdx := lastOf("status_change")
	closeIdx := lastOf("close")
	if statusIdx >= 0 && closeIdx >= 0 && statusIdx >= closeIdx {
		t.Errorf("ordering: status_change (last idx=%d) must precede close (last idx=%d)", statusIdx, closeIdx)
	}
}

// telemetryFrame is the decoded SSE row.
type telemetryFrame struct {
	Seq     int64
	Kind    string
	Payload json.RawMessage
}

// callAPIv1WithHeader is callAPIv1 + the dispatched-task-id header so
// the server's tool-call telemetry middleware fires.
func callAPIv1WithHeader(t *testing.T, ctx context.Context, baseURL, bearer, toolName string, args map[string]any, dispatchedTaskID string) map[string]any {
	t.Helper()
	path := apiPathForToolName(toolName)
	body, _ := json.Marshal(args)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if dispatchedTaskID != "" {
		req.Header.Set("X-Satellites-Dispatched-Task-ID", dispatchedTaskID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s api: %v", toolName, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s api status=%d body=%s", toolName, resp.StatusCode, string(raw))
	}
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s api decode: %v; raw=%s", toolName, err, string(raw))
	}
	if m, ok := out.(map[string]any); ok {
		return m
	}
	return map[string]any{"_array": out}
}

// collectTelemetryFrames opens an SSE stream and pushes every decoded
// frame onto frameCh. Signals `subscribed` when the connection
// handshake has succeeded so the caller can start the producer. Closes
// frameCh when the connection ends.
//
// When SATELLITES_TELEMETRY_DOGFOOD_FILE is set, the literal SSE
// response bytes are also written to that path verbatim — the
// orchestrator uses this to capture the kind:dogfood-evidence row's
// payload for AC6 without parsing.
func collectTelemetryFrames(t *testing.T, ctx context.Context, baseURL, bearer, taskID string, frameCh chan<- telemetryFrame, subscribed chan<- struct{}) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/task/log/stream?task_id="+taskID, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("stream open: %v", err)
		close(frameCh)
		close(subscribed)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("stream status=%d body=%s", resp.StatusCode, string(body))
		close(frameCh)
		close(subscribed)
		return
	}
	close(subscribed)

	var dogfoodSink io.Writer
	if path := strings.TrimSpace(getenv("SATELLITES_TELEMETRY_DOGFOOD_FILE")); path != "" {
		if f, err := createDogfoodFile(path); err == nil {
			defer f.Close()
			dogfoodSink = f
			t.Logf("telemetry dogfood: writing raw SSE to %s", path)
		} else {
			t.Logf("telemetry dogfood: cannot open %s: %v", path, err)
		}
	}

	var body io.Reader = resp.Body
	if dogfoodSink != nil {
		body = io.TeeReader(resp.Body, dogfoodSink)
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var curSeq int64
	var curData bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			fmt.Sscanf(line[4:], "%d", &curSeq)
		case strings.HasPrefix(line, "data: "):
			curData.WriteString(line[6:])
		case line == "":
			if curData.Len() == 0 {
				continue
			}
			var entry struct {
				Seq     int64           `json:"seq"`
				Kind    string          `json:"kind"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(curData.Bytes(), &entry); err == nil {
				select {
				case frameCh <- telemetryFrame{Seq: entry.Seq, Kind: entry.Kind, Payload: entry.Payload}:
				case <-ctx.Done():
					close(frameCh)
					return
				}
			}
			curData.Reset()
			_ = curSeq
		}
	}
	close(frameCh)
}

// drainTelemetryFrames buffers frames from ch until KindClose lands
// (the server-side terminal semantic event) + a short quiet window
// elapses, or the deadline fires. Returns frames in arrival order.
func drainTelemetryFrames(t *testing.T, ctx context.Context, ch <-chan telemetryFrame, deadline time.Duration) []telemetryFrame {
	t.Helper()
	frames := []telemetryFrame{}
	deadlineT := time.After(deadline)
	sawClose := false
	closeAtIdx := -1
	for {
		// After we see close, keep collecting briefly so any trailing
		// rows (none expected from server-side path, but the dispatch
		// shell may emit `stop`) settle.
		var quiet <-chan time.Time
		if sawClose {
			quiet = time.After(2 * time.Second)
		}
		select {
		case f, ok := <-ch:
			if !ok {
				return frames
			}
			frames = append(frames, f)
			if f.Kind == "close" && !sawClose {
				sawClose = true
				closeAtIdx = len(frames) - 1
			}
		case <-quiet:
			_ = closeAtIdx
			return frames
		case <-deadlineT:
			t.Logf("drainTelemetryFrames: deadline reached, frames=%d kinds=%v", len(frames), summariseKinds(frames))
			return frames
		case <-ctx.Done():
			return frames
		}
	}
}

// getenv is os.Getenv but trims whitespace so callers do not have to.
func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }

// createDogfoodFile opens path for write+truncate so the test always
// starts from a clean file.
func createDogfoodFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

// summariseKinds returns one string per arrival frame for log lines.
func summariseKinds(frames []telemetryFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Kind
	}
	return out
}

// firstAgentID returns the first id from the document_list response
// when the by-name lookup fails. Used as a fallback in environments
// where the seeded agent set differs.
func firstAgentID(resp map[string]any) string {
	if arr, ok := resp["_array"].([]any); ok {
		for _, raw := range arr {
			if row, ok2 := raw.(map[string]any); ok2 {
				if id, _ := row["id"].(string); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

// findAgentIDByName scans the document_list response (which may wrap
// rows under "_array", "items", or "entries") and returns the id of
// the entry whose name matches target. Returns "" when absent.
func findAgentIDByName(resp map[string]any, target string) string {
	candidates := [][]any{}
	for _, key := range []string{"_array", "items", "entries", "documents"} {
		if v, ok := resp[key]; ok {
			if arr, ok2 := v.([]any); ok2 {
				candidates = append(candidates, arr)
			}
		}
	}
	for _, v := range resp {
		if arr, ok := v.([]any); ok {
			candidates = append(candidates, arr)
		}
	}
	for _, arr := range candidates {
		for _, raw := range arr {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := row["name"].(string); name == target {
				if id, _ := row["id"].(string); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

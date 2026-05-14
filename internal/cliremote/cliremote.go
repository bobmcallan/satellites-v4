// Package cliremote is the HTTP client the satellites-client CLI uses
// to call satellites-server's typed-verb HTTP API at /api/v1/<noun>/<verb>.
//
// Per docs/cli-primary-design.md §3 + sty_73207fc8 — error envelopes
// map to internal/cliexit codes by HTTP status:
//
//	HTTP 400                → cliexit.Usage  (validation errors)
//	HTTP 401 / 403          → cliexit.Auth
//	HTTP 404                → cliexit.NotFound
//	HTTP 5xx / parse failure → cliexit.Server
//
// Wire shape:
//
//	POST <server>/api/v1/<noun>/<verb>
//	Content-Type: application/json
//	Authorization: Bearer <bearer>
//
//	{ ...args... }
//
// Success (HTTP 200): the response body is the verb's JSON output
// directly, decoded into dst when non-nil.
//
// Error (HTTP non-2xx): the response body is `{"error": "..."}` per
// internal/httpserver/api_errors.go.
//
// The toolName argument retains its `<noun>_<verb>` shape for
// backward-compatible CLI handler call sites; the package translates
// it to the URL path internally via ToolNameToPath (split on the
// first `_`, replace remaining `_` with `-`).
package cliremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// DispatchedTaskIDEnv names the operator-supplied env-var the dispatched
// claude subprocess inherits (set by internal/agent/worker.client_claude).
// When present, Call stamps the value on every outbound /api/v1/* request
// via the X-Satellites-Dispatched-Task-ID header — the satellites-server's
// tool-call telemetry middleware reads the header to emit
// KindToolCallStart / KindToolCallEnd. sty_090f6183.
const DispatchedTaskIDEnv = "SATELLITES_DISPATCHED_TASK_ID"

// DispatchedTaskIDHeader is the HTTP header name the server reads.
const DispatchedTaskIDHeader = "X-Satellites-Dispatched-Task-ID"

// Client is the per-invocation HTTP remote.
type Client struct {
	server string
	bearer string
	http   *http.Client
	logger arbor.ILogger
}

// New constructs a Client bound to the given server base URL + bearer.
// The server should be the base URL (e.g. https://satellites-pprod.fly.dev),
// not the /mcp or /api/v1 path; the package appends /api/v1/... per call.
// httpClient is optional; nil falls back to a default with a 30s timeout.
func New(server, bearer string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		server: server,
		bearer: bearer,
		http:   httpClient,
	}
}

// WithLogger attaches an arbor logger to the client; subsequent Call
// invocations will emit a single Debug row per call carrying
// {verb, method, path, status, duration_ms, error}. Returns the same
// client for fluent wiring at construction. nil-safe — a nil receiver
// is a no-op.
func (c *Client) WithLogger(logger arbor.ILogger) *Client {
	if c == nil {
		return nil
	}
	c.logger = logger
	return c
}

// Call invokes the named verb against /api/v1/<noun>/<verb> with the
// supplied args as the JSON request body. On HTTP 200 the response
// body is decoded into dst (if non-nil). On non-2xx responses the
// returned error carries a typed cliexit.Code.
//
// When a logger is wired (see WithLogger), each invocation emits one
// Debug row at end-of-call with {verb, method, path, status,
// duration_ms, error}. Request body / args are NOT logged — verbs
// like ledger_append and task_add carry operator-pastable text that
// often includes sensitive content.
func (c *Client) Call(ctx context.Context, toolName string, args any, dst any) (rerr error) {
	if c == nil {
		return cliexit.Newf(cliexit.Server, "cliremote: client not configured")
	}
	start := time.Now()
	path := "/api/v1" + ToolNameToPath(toolName)
	status := 0
	defer func() {
		if c.logger == nil {
			return
		}
		c.logger.Debug().
			Str("verb", toolName).
			Str("method", "POST").
			Str("path", path).
			Int("status", status).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Bool("error", rerr != nil).
			Msg("cli call")
	}()
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("marshal args: %w", err))
	}
	url := strings.TrimRight(c.server, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliexit.Wrap(cliexit.Server, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	// sty_090f6183: forward the dispatched-task id when running inside
	// a claude subprocess spawned by the satellites worker.
	if tid := strings.TrimSpace(os.Getenv(DispatchedTaskIDEnv)); tid != "" {
		req.Header.Set(DispatchedTaskIDHeader, tid)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("http %s: %w", url, err))
	}
	defer resp.Body.Close()
	status = resp.StatusCode

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("read response: %w", err))
	}

	if resp.StatusCode == http.StatusOK {
		if dst == nil {
			return nil
		}
		if err := json.Unmarshal(respBody, dst); err != nil {
			return cliexit.Wrap(cliexit.Server, fmt.Errorf("parse response: %w (body: %s)", err, truncate(respBody, 200)))
		}
		return nil
	}

	msg := readAPIErrorMessage(respBody, resp.Status)
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return cliexit.Newf(cliexit.Usage, "%s: %s", toolName, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return cliexit.Newf(cliexit.Auth, "%s: %s", toolName, msg)
	case http.StatusNotFound:
		return cliexit.Newf(cliexit.NotFound, "%s: %s", toolName, msg)
	default:
		if resp.StatusCode >= 500 {
			return cliexit.Newf(cliexit.Server, "%s: %s: %s", toolName, resp.Status, truncate(respBody, 200))
		}
		return cliexit.Newf(cliexit.Server, "%s: %s", toolName, msg)
	}
}

// SystemVersionArtifact mirrors the per-binary entry in the manifest
// payload returned by /api/v1/system/version. Re-stated locally so
// CLI callers (the boot drift check) can decode the typed shape
// without importing internal/client. sty_64e69db8.
type SystemVersionArtifact struct {
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Filename    string `json:"filename"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
}

// SystemVersionOutput mirrors the JSON wire-shape the server emits at
// POST /api/v1/system/version. Decode-only struct — callers don't
// construct one.
type SystemVersionOutput struct {
	Version   string                  `json:"version"`
	Build     string                  `json:"build"`
	Commit    string                  `json:"commit"`
	Artifacts []SystemVersionArtifact `json:"artifacts"`
	FetchedAt time.Time               `json:"fetched_at"`
}

// SystemVersion is the typed convenience wrapper around Call(...) for
// the boot drift check. Returns the decoded payload or a typed
// cliexit error.
func (c *Client) SystemVersion(ctx context.Context) (SystemVersionOutput, error) {
	var out SystemVersionOutput
	if err := c.Call(ctx, "system_version", map[string]any{}, &out); err != nil {
		return SystemVersionOutput{}, err
	}
	return out, nil
}

// TaskLogAppendInput is the typed wire payload for task_log_append.
// Mirrors the server-side input shape used by `cmd/satellites-client
// task run`'s lifecycle + chunk uploaders. Payload is a json.RawMessage
// so the caller can pre-marshal a per-kind body. sty_8c17b89d.
type TaskLogAppendInput struct {
	TaskID      string          `json:"task_id"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	ProjectID   string          `json:"project_id,omitempty"`
	Seq         int64           `json:"seq"`
	TS          time.Time       `json:"ts,omitempty"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// TaskLogAppendOutput mirrors the typed return of task_log_append.
type TaskLogAppendOutput struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskLogAppend is the typed wrapper around Call(...) for the
// task_log_append verb. Marshals `in` directly so the wire body matches
// the JSON tag set.
func (c *Client) TaskLogAppend(ctx context.Context, in TaskLogAppendInput) (TaskLogAppendOutput, error) {
	args := map[string]any{
		"task_id": in.TaskID,
		"seq":     in.Seq,
		"kind":    in.Kind,
	}
	if in.WorkspaceID != "" {
		args["workspace_id"] = in.WorkspaceID
	}
	if in.ProjectID != "" {
		args["project_id"] = in.ProjectID
	}
	if !in.TS.IsZero() {
		args["ts"] = in.TS.UTC().Format(time.RFC3339Nano)
	}
	if len(in.Payload) > 0 {
		args["payload"] = in.Payload
	}
	var out TaskLogAppendOutput
	if err := c.Call(ctx, "task_log_append", args, &out); err != nil {
		return TaskLogAppendOutput{}, err
	}
	return out, nil
}

// TaskLogListInput selects rows for task_log_list. sty_8c17b89d.
type TaskLogListInput struct {
	TaskID  string `json:"task_id"`
	FromSeq int64  `json:"from_seq,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// TaskLogListEntry mirrors one row in the task_log_list response.
type TaskLogListEntry struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	WorkspaceID string          `json:"workspace_id"`
	ProjectID   string          `json:"project_id,omitempty"`
	Seq         int64           `json:"seq"`
	TS          time.Time       `json:"ts"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// TaskLogListOutput mirrors the wire payload of task_log_list.
type TaskLogListOutput struct {
	Entries []TaskLogListEntry `json:"entries"`
}

// TaskLogList is the typed wrapper around Call(...) for the
// task_log_list verb.
func (c *Client) TaskLogList(ctx context.Context, in TaskLogListInput) (TaskLogListOutput, error) {
	args := map[string]any{
		"task_id": in.TaskID,
	}
	if in.FromSeq != 0 {
		args["from_seq"] = in.FromSeq
	}
	if in.Limit > 0 {
		args["limit"] = in.Limit
	}
	var out TaskLogListOutput
	if err := c.Call(ctx, "task_log_list", args, &out); err != nil {
		return TaskLogListOutput{}, err
	}
	return out, nil
}

// compoundNouns names tool prefixes whose noun-form spans two
// underscore-separated tokens. Each entry maps the compound noun's
// canonical MCP form (`task_log`) to its URL-path projection
// (`task/log`). Used by ToolNameToPath so `task_log_append` resolves
// to `/task/log/append` rather than `/task/log-append`. sty_8c17b89d.
var compoundNouns = map[string]string{
	"task_log": "task/log",
}

// ToolNameToPath translates a tool name (`<noun>_<verb>` form, where
// the verb itself may carry further `_` separators) into the URL path
// the HTTP API exposes (`/<noun>/<verb>`, with `_` inside the verb
// becoming `-`). Compound nouns registered in compoundNouns retain
// their internal `_`→`/` mapping. Exported for the table-driven test.
func ToolNameToPath(name string) string {
	for prefix, urlPath := range compoundNouns {
		head := prefix + "_"
		if strings.HasPrefix(name, head) {
			verb := strings.ReplaceAll(name[len(head):], "_", "-")
			return "/" + urlPath + "/" + verb
		}
	}
	i := strings.IndexByte(name, '_')
	if i < 0 {
		return "/" + name
	}
	noun := name[:i]
	verb := strings.ReplaceAll(name[i+1:], "_", "-")
	return "/" + noun + "/" + verb
}

// readAPIErrorMessage parses the `{"error": "..."}` envelope written
// by internal/httpserver/api_errors.go. Falls back to the supplied
// status text when the body is missing, not JSON, or has an empty
// `error` field.
func readAPIErrorMessage(body []byte, fallback string) string {
	if len(body) == 0 {
		return fallback
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		return env.Error
	}
	return fallback
}

// truncate returns at most n bytes of the supplied byte slice as a
// string. Convenience for error messages — keeps the output bounded.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

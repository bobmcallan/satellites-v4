// Package cliremote is the HTTP/JSON-RPC client the satellites-client
// CLI uses to call satellites-server's MCP endpoint. Speaks the same
// `tools/call` shape every dispatched-claude session uses.
//
// Per docs/cli-primary-design.md §3 — error envelopes map to
// internal/cliexit codes:
//
//	HTTP 401/403            → cliexit.Auth
//	error containing "not_found" / HTTP 404 → cliexit.NotFound
//	HTTP 5xx / parse failure → cliexit.Server
//
// JSON-RPC envelope:
//
//	{
//	  "jsonrpc": "2.0",
//	  "id": <int>,
//	  "method": "tools/call",
//	  "params": {"name": <toolName>, "arguments": <args>}
//	}
//
// Response shape (mcp-go):
//
//	{
//	  "jsonrpc": "2.0",
//	  "id": <int>,
//	  "result": {
//	    "content": [{"type": "text", "text": "<json-encoded-payload>"}],
//	    "isError": false
//	  }
//	}
//
// On `isError=true` the text is a structured error message (often
// JSON itself); the Client maps it to a typed cliexit error.
package cliremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// Client is the per-invocation HTTP/JSON-RPC remote.
type Client struct {
	server string
	bearer string
	http   *http.Client
	id     atomic.Int64
}

// New constructs a Client bound to the given server URL + bearer.
// httpClient is optional; nil falls back to a default with a 30s
// timeout — enough for the slowest verb (story_export_walk on a long
// chain).
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

// Call invokes the named MCP tool with the given args. On success
// the text payload is unmarshalled into dst (if non-nil). On error
// the returned error carries a typed cliexit.Code.
func (c *Client) Call(ctx context.Context, toolName string, args any, dst any) error {
	if c == nil {
		return cliexit.Newf(cliexit.Server, "cliremote: client not configured")
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.id.Add(1),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("marshal envelope: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server, bytes.NewReader(body))
	if err != nil {
		return cliexit.Wrap(cliexit.Server, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("http %s: %w", c.server, err))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return cliexit.Newf(cliexit.Auth, "%s: %s", toolName, resp.Status)
	case http.StatusNotFound:
		return cliexit.Newf(cliexit.NotFound, "%s: %s", toolName, resp.Status)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode >= 500 {
		return cliexit.Newf(cliexit.Server, "%s: %s: %s", toolName, resp.Status, truncate(respBody, 200))
	}

	var rpc struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpc); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("parse jsonrpc: %w (body: %s)", err, truncate(respBody, 200)))
	}
	if rpc.Error != nil {
		return cliexit.Newf(cliexit.Server, "%s: %s (code=%d)", toolName, rpc.Error.Message, rpc.Error.Code)
	}
	if rpc.Result == nil || len(rpc.Result.Content) == 0 {
		return cliexit.Newf(cliexit.Server, "%s: empty result", toolName)
	}
	text := rpc.Result.Content[0].Text
	if rpc.Result.IsError {
		// MCP error envelopes pack a body that often contains a
		// JSON object describing the failure; parse to extract
		// not_found / auth markers for typed exit code routing.
		return mapToolError(toolName, text)
	}
	if dst != nil {
		if err := json.Unmarshal([]byte(text), dst); err != nil {
			return cliexit.Wrap(cliexit.Server, fmt.Errorf("parse result text: %w", err))
		}
	}
	return nil
}

// mapToolError inspects the error text for known markers and routes
// to the matching cliexit code. Falls back to Server.
func mapToolError(toolName, text string) error {
	lc := strings.ToLower(text)
	switch {
	case strings.Contains(lc, "not_found"), strings.Contains(lc, "not found"):
		return cliexit.Newf(cliexit.NotFound, "%s: %s", toolName, text)
	case strings.Contains(lc, "session_not_registered"), strings.Contains(lc, "unauthorized"), strings.Contains(lc, "forbidden"):
		return cliexit.Newf(cliexit.Auth, "%s: %s", toolName, text)
	default:
		return cliexit.Newf(cliexit.Server, "%s: %s", toolName, text)
	}
}

// truncate returns at most n bytes of the supplied byte slice as a
// string. Convenience for error messages — keeps the output bounded.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

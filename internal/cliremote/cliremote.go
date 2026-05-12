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
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// Client is the per-invocation HTTP remote.
type Client struct {
	server string
	bearer string
	http   *http.Client
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

// Call invokes the named verb against /api/v1/<noun>/<verb> with the
// supplied args as the JSON request body. On HTTP 200 the response
// body is decoded into dst (if non-nil). On non-2xx responses the
// returned error carries a typed cliexit.Code.
func (c *Client) Call(ctx context.Context, toolName string, args any, dst any) error {
	if c == nil {
		return cliexit.Newf(cliexit.Server, "cliremote: client not configured")
	}
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("marshal args: %w", err))
	}
	url := strings.TrimRight(c.server, "/") + "/api/v1" + ToolNameToPath(toolName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliexit.Wrap(cliexit.Server, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("http %s: %w", url, err))
	}
	defer resp.Body.Close()

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

// ToolNameToPath translates a tool name (`<noun>_<verb>` form, where
// the verb itself may carry further `_` separators) into the URL path
// the HTTP API exposes (`/<noun>/<verb>`, with `_` inside the verb
// becoming `-`). Exported for the table-driven test.
func ToolNameToPath(name string) string {
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

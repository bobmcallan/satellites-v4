// Package client — portal_get_page verb (sty_02ad5eb9).
//
// Restores the V3 satellites_portal_get_page surface that was dropped
// in the V3→current substrate migration. Read-only verb: validates the
// requested portal path, mints a 30-second loopback session for the
// caller, issues a non-redirect-following GET against the local
// satellites HTTP server, and returns the raw SSR HTML body + status
// to the caller so an MCP-side LLM/operator/auditor can introspect
// what the portal actually renders (not just substrate state).
//
// The typed method is the single business-logic site per
// pr_mcp_cli_shared_path; the MCP, HTTP, and CLI handlers are thin
// transport adapters that re-shape the wire envelope.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PortalGetPageInput is the typed input to PortalGetPage.
type PortalGetPageInput struct {
	// Path is the portal path to fetch (must start with "/"). Validated
	// against the blocked-prefix set before any loopback call is made.
	Path string
}

// PortalGetPageOutput is the typed mirror of the JSON-RPC wire shape.
// JSON tags pin the field names so MCP + HTTP + CLI callers see the
// same envelope.
type PortalGetPageOutput struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// portalSessionTTL is the lifetime of the loopback cookie the verb
// mints (AC3). 30 seconds is enough for the GET + body read but short
// enough that a stolen cookie can't be replayed past the call.
const portalSessionTTL = 30 * time.Second

// portalLoopbackTimeout caps the loopback GET at 10 seconds (AC4). A
// slow SSR render is a substrate bug, not something this verb should
// hang on.
const portalLoopbackTimeout = 10 * time.Second

// portalBlockedPrefixes is the set of path prefixes PortalGetPage
// rejects (AC2). Each entry has a distinct error string the
// integration test can assert against.
var portalBlockedPrefixes = []string{
	"/api/",
	"/oauth/",
	"/mcp",
	"/static/",
	"/.well-known/",
}

// PortalGetPage fetches the SSR-rendered HTML at in.Path by issuing a
// loopback HTTP GET against the local satellites server. Auth, path
// validation, session mint, and redirect-not-followed semantics live
// here so MCP + HTTP + CLI callers share one substrate path
// (pr_mcp_cli_shared_path).
//
// Errors map to documented strings the integration test asserts
// against:
//   - zero caller.UserID                 → "authentication required"
//   - missing PortalLoopbackURL          → "portal_get_page unavailable: loopback not configured"
//   - missing PortalSessionMint          → "portal_get_page unavailable: session minter not configured"
//   - empty path / no leading slash      → "path must start with '/'"
//   - path contains ".."                 → "path must not contain '..'"
//   - blocked prefix                     → "prefix <p> blocked"
//   - loopback request failure           → "portal_get_page request failed: <err>"
func (c *Client) PortalGetPage(ctx context.Context, caller Caller, in PortalGetPageInput) (PortalGetPageOutput, error) {
	if caller.UserID == "" {
		return PortalGetPageOutput{}, errors.New("authentication required")
	}
	if c.deps.PortalLoopbackURL == nil {
		return PortalGetPageOutput{}, errors.New("portal_get_page unavailable: loopback not configured")
	}
	if c.deps.PortalSessionMint == nil {
		return PortalGetPageOutput{}, errors.New("portal_get_page unavailable: session minter not configured")
	}
	if err := validatePortalPath(in.Path); err != nil {
		return PortalGetPageOutput{}, err
	}
	sessionID, err := c.deps.PortalSessionMint(caller.UserID, portalSessionTTL)
	if err != nil {
		return PortalGetPageOutput{}, fmt.Errorf("portal_get_page session mint failed: %w", err)
	}
	base := strings.TrimRight(c.deps.PortalLoopbackURL(), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+in.Path, nil)
	if err != nil {
		return PortalGetPageOutput{}, fmt.Errorf("portal_get_page request build failed: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: "satellites_session", Value: sessionID})

	client := c.deps.PortalHTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: portalLoopbackTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return PortalGetPageOutput{}, fmt.Errorf("portal_get_page request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PortalGetPageOutput{}, fmt.Errorf("portal_get_page body read failed: %w", err)
	}
	return PortalGetPageOutput{Status: resp.StatusCode, Body: string(body)}, nil
}

// validatePortalPath enforces AC2. URL-decoding runs first so a
// percent-encoded "%2e%2e" cannot bypass the literal ".." check. The
// prefix scan runs after decoding so percent-encoded blocked-prefix
// attempts ("/%61pi/...") are rejected too.
func validatePortalPath(raw string) error {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return errors.New("path must start with '/'")
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return fmt.Errorf("path url-decode failed: %w", err)
	}
	if strings.Contains(decoded, "..") {
		return errors.New("path must not contain '..'")
	}
	for _, p := range portalBlockedPrefixes {
		if strings.HasPrefix(decoded, p) {
			return fmt.Errorf("prefix %s blocked", p)
		}
	}
	return nil
}

// ResolvePortalLoopbackHost returns the loopback host that the verb's
// loopback URL accessor should embed: "" and "0.0.0.0" both fall back
// to 127.0.0.1; any other value passes through unchanged (AC4). The
// wire layer (cmd/satellites-server/main.go) calls this when wiring
// Deps.PortalLoopbackURL from cfg.Port.
func ResolvePortalLoopbackHost(host string) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0":
		return "127.0.0.1"
	default:
		return host
	}
}

// Package client — handshake instructions resolver (sty_4db0e025 slice A11).
//
// The MCP server's handshake instructions block is sourced from the
// seeded agent-process artifact when available, falling back to the
// caller-provided literal when the resolver returns empty. Originally
// lived as resolveHandshakeInstructions in internal/mcpserver/mcp.go;
// slice A11 lifted it onto the typed client surface so the transport
// file no longer imports internal/agentprocess directly.
package client

import (
	"context"

	"github.com/bobmcallan/satellites/internal/agentprocess"
)

// ResolveHandshakeInstructions returns the agent-process artifact body
// for the system tier (no project / no memberships), or the supplied
// fallback when the resolver returns empty. The Client's Documents
// store may be nil during early-boot tests; the helper returns the
// fallback in that case so the server stays bootable.
func (c *Client) ResolveHandshakeInstructions(ctx context.Context, fallback string) string {
	if body := agentprocess.Resolve(ctx, c.deps.Documents, "", nil); body != "" {
		return body
	}
	return fallback
}

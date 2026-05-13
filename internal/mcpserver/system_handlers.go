// Package mcpserver — `system_version` MCP verb.
//
// Thin wire-layer adapter for the typed *client.Client.SystemVersion
// method. The fetch / cache / manifest-parse logic lives on the typed
// surface per `pr_mcp_cli_shared_path` (sty_3dc39a5c); this file owns
// only argument decoding + response shaping. sty_64e69db8.
package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleSystemVersion is the system_version tool adapter. Resolves
// caller → typed call → marshal wire-shape payload. No substrate
// imports.
func (s *Server) handleSystemVersion(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().SystemVersion(ctx, toClientCaller(caller), client.SystemVersionInput{})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"version":    out.Version,
		"build":      out.Build,
		"commit":     out.Commit,
		"artifacts":  out.Artifacts,
		"fetched_at": out.FetchedAt.Format(time.RFC3339),
	})
	return mcpgo.NewToolResultText(string(body)), nil
}

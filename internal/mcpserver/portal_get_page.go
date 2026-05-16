// portal_get_page wire adapter (sty_02ad5eb9).
//
// Thin MCP wire layer: tool registration + a handler that reads the
// JSON-RPC `path` arg, hands it to *client.Client.PortalGetPage, and
// re-packs the `{status, body}` envelope. The substrate logic (auth,
// path validation, loopback request, redirect-not-followed) lives in
// internal/client per pr_mcp_cli_shared_path; this file does not
// import any substrate package directly.
package mcpserver

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// registerPortalGetPage wires the portal_get_page MCP tool. Called
// from Server.New once the loopback + session-mint deps are wired.
func (s *Server) registerPortalGetPage() {
	tool := mcpgo.NewTool("portal_get_page",
		mcpgo.WithDescription("Fetch a rendered portal page and return its HTML content. Use this to inspect the portal UI, debug layout issues, or verify page content. Returns the full HTML body + the HTTP status of the requested page. Read-only loopback GET; redirects are NOT followed (the 3xx status is returned as-is). Allowed paths must start with '/' and must not target the API/OAuth/MCP/static/well-known surfaces."),
		mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Portal page path to fetch (e.g. /home, /projects, /projects/{id}).")),
	)
	s.mcp.AddTool(tool, s.handlePortalGetPage)
}

// handlePortalGetPage is the wire adapter: read the JSON arg, hand it
// to the typed surface, marshal the `{status, body}` envelope. Errors
// surface via mcpgo.NewToolResultError so the caller sees the typed
// method's error string verbatim.
func (s *Server) handlePortalGetPage(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	path, _ := req.RequireString("path")
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().PortalGetPage(ctx, toClientCaller(caller), client.PortalGetPageInput{Path: path})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

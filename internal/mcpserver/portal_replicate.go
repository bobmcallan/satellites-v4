// portal_replicate wire adapter (sty_f3f7bf9b slice 12; converged
// under sty_4db0e025 slice A8).
//
// This file is the thin MCP wire-layer adapter: tool registration +
// a ≤25-line handler that pulls JSON args off the request, hands the
// raw strings to *client.Client.PortalReplicate, and re-packs the
// pre-extraction `{summary, results}` envelope. Boot-time wiring
// (SetReplicateVocabulary, SetReplicateRunner, LoadReplicateVocabularyFromDoc,
// requireReplicatePrereqs) lives on the Server in mcp.go — the
// internal/portalreplicate types are needed there to spell the
// accessor signatures, and parking them on the already-allowlisted
// mcp.go keeps this file out of the legacy substrate-import list.
package mcpserver

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// registerPortalReplicate wires the portal_replicate MCP tool. Called
// from Server.New when the dep prerequisites (stories + ledger) are
// non-nil.
func (s *Server) registerPortalReplicate() {
	tool := mcpgo.NewTool("portal_replicate",
		mcpgo.WithDescription("Drive a headless browser through a sequence of actions against a target URL and ledger the captured DOM / console / screenshot evidence onto a story. Sty_088f6d5c. Action types: navigate, wait_visible, click, dom_snapshot, console_capture, screenshot, dom_visible — plus any natural-language alias declared by the replicate_vocabulary document."),
		mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story to attach evidence to (sty_<8hex>).")),
		mcpgo.WithString("target_url", mcpgo.Required(), mcpgo.Description("Absolute base URL to navigate against. The first navigate action without a Value loads this URL.")),
		mcpgo.WithString("actions", mcpgo.Required(), mcpgo.Description("JSON array of {type, selector?, value?, timeout_ms?, label?}. Type may be a canonical action or a vocabulary alias.")),
		mcpgo.WithString("cookies", mcpgo.Description("Optional JSON array of {name, value, domain?, path?, secure?, http_only?}. Domain defaults to target_url host.")),
	)
	s.mcp.AddTool(tool, s.handlePortalReplicate)
}

// handlePortalReplicate is the wire adapter: read JSON args off the
// request, hand them as raw strings to the typed surface, marshal the
// `{summary, results}` envelope. Substrate logic — including the JSON
// unmarshal into portalreplicate.Action/Cookie — lives in
// *client.Client.PortalReplicate.
func (s *Server) handlePortalReplicate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, _ := req.RequireString("story_id")
	targetURL, _ := req.RequireString("target_url")
	actionsJSON := req.GetString("actions", "")
	if actionsJSON == "" {
		return mcpgo.NewToolResultError("actions is required (JSON array)"), nil
	}
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().PortalReplicate(ctx, toClientCaller(caller), client.PortalReplicateInput{
		StoryID:     storyID,
		TargetURL:   targetURL,
		ActionsJSON: actionsJSON,
		CookiesJSON: req.GetString("cookies", ""),
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

package mcpserver

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleStoryExportWalk implements story_export_walk: render a story's
// task chain as paste-ready markdown for PR descriptions, delivery
// reports, and stakeholder hand-offs. Thin forwarder to
// client.StoryExportWalk (the renderer moved to internal/client in
// sty_ef248ab2 so /api/v1 can share it). Sty_a248f4df.
func (s *Server) handleStoryExportWalk(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().StoryExportWalk(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.StoryExportWalkInput{
		StoryID:     storyID,
		Format:      req.GetString("format", "markdown"),
		Memberships: memberships,
	})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

package mcpserver

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleTaskWalk implements the task_walk MCP verb. Thin forwarder to
// client.TaskWalk per cli-primary order:07a-layer-2 (sty_df1cb227).
func (s *Server) handleTaskWalk(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().TaskWalk(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.TaskWalkInput{StoryID: storyID, Memberships: memberships})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

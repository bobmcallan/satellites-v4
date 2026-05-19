package mcpserver

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleTaskRenderPrompt implements the task_render_prompt MCP verb.
// Thin forwarder to client.RenderTaskPrompt — the typed business
// surface owns the assembly logic; the wire handler only parses input
// and returns the rendered markdown as a single `prompt` field. The
// satellites-client CLI strips the envelope and writes the markdown
// to stdout raw (AC1). Sty_72e36256.
func (s *Server) handleTaskRenderPrompt(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	action, err := req.RequireString("action")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	workBody := ""
	if v, ok := req.GetArguments()["work"].(string); ok {
		workBody = v
	}

	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	ctx = client.WithOriginVerb(ctx, "task_render_prompt")

	prompt, err := s.cli().RenderTaskPrompt(ctx, client.Caller{
		UserID:      caller.UserID,
		Email:       caller.Email,
		Memberships: memberships,
	}, client.RenderTaskPromptInput{
		TaskID:   taskID,
		Action:   action,
		StoryID:  storyID,
		WorkBody: workBody,
	})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(map[string]any{"prompt": prompt})
	return mcpgo.NewToolResultText(string(body)), nil
}

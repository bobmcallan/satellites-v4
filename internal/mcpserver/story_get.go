// Package mcpserver — story_get MCP verb.
//
// story_get is a single-roundtrip composer that returns the orientation
// bundle an agent needs when picking up work on a story: the story row
// (body / status / fields / tags), the owning project, recent ledger
// evidence, the resolved agent_process instruction markdown, and the
// category template (when one applies).
//
// sty_48e38e83 merged the prior thin row-only `story_get` with the
// orientation-bundle `story_context` verb. The CRUD-shaped name wins;
// the orientation-bundle behaviour is what callers want.
//
// sty_4db0e025 slice A9 converged this handler onto the typed
// *client.Client surface: the orientation-bundle assembly now lives in
// client.StoryGet (internal/client/story.go), and this file is a thin
// wire adapter — it unmarshals the JSON-RPC args, calls one typed
// method, and shapes the response. Per pr_mcp_cli_shared_path,
// transport handlers do not import substrate domain packages directly.
package mcpserver

import (
	"context"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleStoryGet implements `story_get`. Workspace-scoped via
// memberships; cross-workspace stories return story_not_found. Thin
// forwarder to client.StoryGet.
func (s *Server) handleStoryGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	ctx = client.WithOriginVerb(ctx, "story_get")
	out, err := s.cli().StoryGet(ctx, client.Caller{
		UserID:      caller.UserID,
		Email:       caller.Email,
		Memberships: memberships,
	}, client.StoryGetInput{
		ID:          id,
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_get").
		Str("story_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return jsonResult(out)
}

// Package mcpserver — task_add MCP verb (sty_a427368d).
//
// task_add mints one task at status=published for the given agent. When
// story_id is omitted the substrate auto-mints a thin ad-hoc story so
// every task is anchored to a story (project intent: "no work outside
// a story"). task_add mints exactly one task — review pairing, when a
// contract requires it, is authored by the reviewer's contract prose
// via a follow-up task_add(prior_task_id=…) call.
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleTaskAdd implements `task_add`. Thin forwarder to client.TaskAdd
// per cli-primary order:07a-layer-2 (sty_df1cb227). The resolver
// callbacks (CallerActiveProjectID / ResolveStoryProjectID /
// ResolveProjectWorkspaceID) carry the wire-layer's session + URL
// context into the typed method without coupling the client package to
// HTTP request shapes.
func (s *Server) handleTaskAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)

	in := client.TaskAddInput{
		AgentID:     strings.TrimSpace(req.GetString("agent_id", "")),
		Prompt:      req.GetString("prompt", ""),
		StoryID:     strings.TrimSpace(req.GetString("story_id", "")),
		Kind:        strings.TrimSpace(req.GetString("kind", "")),
		Action:      strings.TrimSpace(req.GetString("action", "")),
		Priority:    strings.TrimSpace(req.GetString("priority", "")),
		Memberships: memberships,
		Now:         s.nowUTC(),
		Resolve: client.TaskAddResolveDeps{
			CallerActiveProjectID: func(ctx context.Context, c client.Caller) string {
				return s.callerActiveProjectID(ctx, auth.CallerIdentity{UserID: c.UserID, Email: c.Email})
			},
			ResolveStoryProjectID:     s.resolveStoryProjectID,
			ResolveProjectWorkspaceID: s.resolveProjectWorkspaceID,
			DefaultProjectID:          s.defaultProjectID,
		},
	}
	out, err := s.cli().TaskAdd(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"task_id":      out.TaskID,
		"story_id":     out.StoryID,
		"story_minted": out.StoryMinted,
		"status":       out.Status,
		"agent_id":     out.AgentID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "task_add").
		Str("story_id", out.StoryID).
		Str("task_id", out.TaskID).
		Bool("story_minted", out.StoryMinted).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// callerActiveProjectID returns the project id stamped on the caller's
// session row (set by project_set / session_register), or "" when the
// caller has no resolvable session.
func (s *Server) callerActiveProjectID(ctx context.Context, caller auth.CallerIdentity) string {
	if s.sessions == nil {
		return ""
	}
	sessionID := resolveSessionID(ctx, "")
	if sessionID == "" {
		return ""
	}
	sess, err := s.sessions.Get(ctx, caller.UserID, sessionID)
	if err != nil {
		return ""
	}
	return sess.ActiveProjectID
}

// resolveStoryProjectID returns the project_id of the story with id
// storyID, or "" when the story is missing or the caller has no
// membership in its workspace. Sty_21d4d830 — used by the
// scope=workspace agent path to prefer the story's project as the
// task's tenancy when a story_id is supplied.
func (s *Server) resolveStoryProjectID(ctx context.Context, storyID string, memberships []string) string {
	if s.stories == nil || storyID == "" {
		return ""
	}
	st, err := s.stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		return ""
	}
	return st.ProjectID
}


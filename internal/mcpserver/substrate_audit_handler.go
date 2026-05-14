// Package mcpserver — `substrate_audit` MCP verb (sty_2f0db922).
//
// Thin wire-layer adapter for the typed *client.Client.SubstrateAudit
// method. Decodes optional caller args (project_id, workspace_id),
// invokes the typed surface, and marshals the wire payload. Per
// pr_mcp_cli_shared_path the typed method owns the agent-resolution
// + task-mint logic; this file holds no payload-assembly code of its
// own.
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleSubstrateAudit is the substrate_audit tool adapter. Mints a
// kind=work action=contract:substrate_audit task naming the system-
// scope substrate_auditor agent. Returns {task_id, story_id,
// agent_id, scope}.
func (s *Server) handleSubstrateAudit(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	in := client.SubstrateAuditInput{
		ProjectID:   strings.TrimSpace(stringArg(args, "project_id")),
		WorkspaceID: strings.TrimSpace(stringArg(args, "workspace_id")),
		Memberships: memberships,
		Resolve: client.TaskAddResolveDeps{
			CallerActiveProjectID: func(ctx context.Context, c client.Caller) string {
				return s.callerActiveProjectID(ctx, auth.CallerIdentity{UserID: c.UserID, Email: c.Email})
			},
			ResolveStoryProjectID:     s.resolveStoryProjectID,
			ResolveProjectWorkspaceID: s.resolveProjectWorkspaceID,
			DefaultProjectID:          s.deps.DefaultProjectID,
		},
	}
	out, err := s.cli().SubstrateAudit(ctx, client.Caller{
		UserID:      caller.UserID,
		Email:       caller.Email,
		Memberships: memberships,
	}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"task_id":  out.TaskID,
		"story_id": out.StoryID,
		"agent_id": out.AgentID,
		"scope":    out.Scope,
	})
	return mcpgo.NewToolResultText(string(body)), nil
}

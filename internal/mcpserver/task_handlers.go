package mcpserver

import (
	"context"
	"errors"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleTaskGet implements task_get with workspace scoping. Thin
// forwarder to client.TaskGet.
func (s *Server) handleTaskGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	t, err := s.cli().TaskGet(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.TaskGetInput{ID: id, Memberships: memberships})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(t)
}

// handleTaskList implements task_list. Thin forwarder to
// client.TaskList — filter parsing lives at the wire boundary, the
// scoped store query lives on the typed surface.
func (s *Server) handleTaskList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	in := client.TaskListInput{
		Origin:      getString(args, "origin"),
		Status:      getString(args, "status"),
		Priority:    getString(args, "priority"),
		ClaimedBy:   getString(args, "claimed_by"),
		StoryID:     getString(args, "story_id"),
		Kind:        getString(args, "kind"),
		Memberships: memberships,
	}
	if v, ok := args["include_archived"].(bool); ok {
		in.IncludeArchived = v
	}
	if v, ok := args["limit"].(float64); ok {
		in.Limit = int(v)
	}
	rows, err := s.cli().TaskList(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(rows)
}

// handleTaskClaim implements task_claim: atomic pick + kind:task-claimed
// ledger row. Returns null result when queue is empty (not an error).
// Thin forwarder to client.TaskClaim.
func (s *Server) handleTaskClaim(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	in := client.TaskClaimInput{
		WorkerID:    getString(args, "worker_id"),
		Memberships: memberships,
		Now:         time.Now().UTC(),
	}
	if scoped := getString(args, "workspace_id"); scoped != "" {
		in.WorkspaceIDs = []string{scoped}
	}
	t, err := s.cli().TaskClaim(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if errors.Is(err, client.ErrNoTaskAvailable) {
		return jsonResult(nil)
	}
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(t)
}

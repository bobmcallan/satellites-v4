package mcpserver

import (
	"context"
	"errors"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/task"
)

// handleTaskPlan implements task_plan: write a task at status=planned —
// the agent's drafting state. Subscribers do not see planned rows.
// Thin forwarder per cli-primary order:07a-layer-2 (sty_df1cb227).
func (s *Server) handleTaskPlan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	expectedStr := getString(args, "expected_duration")
	var expected time.Duration
	if expectedStr != "" {
		if d, err := time.ParseDuration(expectedStr); err == nil {
			expected = d
		}
	}
	in := client.TaskPlanInput{
		Origin:           getString(args, "origin"),
		WorkspaceID:      getString(args, "workspace_id"),
		ProjectID:        getString(args, "project_id"),
		Kind:             getString(args, "kind"),
		AgentID:          getString(args, "agent_id"),
		ParentTaskID:     getString(args, "parent_task_id"),
		PriorTaskID:      getString(args, "prior_task_id"),
		Priority:         getString(args, "priority"),
		Trigger:          []byte(getString(args, "trigger")),
		ExpectedDuration: expected,
		Memberships:      memberships,
	}
	out, err := s.cli().TaskPlan(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{
		"task_id":        out.TaskID,
		"ledger_root_id": out.LedgerRootID,
		"workspace_id":   out.WorkspaceID,
		"status":         out.Status,
		"priority":       out.Priority,
		"origin":         out.Origin,
	})
}

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

// handleTaskList implements task_list. Out of Slice-A scope — kept
// here unmodified (Slice B/C territory). Behaviour-parity proof.
func (s *Server) handleTaskList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.tasks == nil {
		return mcpgo.NewToolResultError("task_list unavailable"), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	opts := task.ListOptions{
		Origin:    getString(args, "origin"),
		Status:    getString(args, "status"),
		Priority:  getString(args, "priority"),
		ClaimedBy: getString(args, "claimed_by"),
		StoryID:   getString(args, "story_id"),
		Kind:      getString(args, "kind"),
	}
	if v, ok := args["include_archived"].(bool); ok {
		opts.IncludeArchived = v
	}
	if v, ok := args["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	rows, err := s.tasks.List(ctx, opts, memberships)
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
	workerID := getString(args, "worker_id")
	in := client.TaskClaimInput{
		WorkerID:    workerID,
		Memberships: memberships,
		Now:         time.Now().UTC(),
	}
	if scoped := getString(args, "workspace_id"); scoped != "" {
		in.WorkspaceIDs = []string{scoped}
	}
	t, err := s.cli().TaskClaim(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if errors.Is(err, task.ErrNoTaskAvailable) {
		return jsonResult(nil)
	}
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(t)
}

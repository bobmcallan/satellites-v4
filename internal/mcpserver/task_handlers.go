package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/task"
)

// handleTaskPlan implements task_plan: write a task at status=planned —
// the agent's drafting state. Subscribers do not see planned rows.
// sty_c1200f75. task_plan covers the bare-draft case where a task is
// staged for later publication; task_add is the single-task creation
// path that lands at status=published.
func (s *Server) handleTaskPlan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return s.createTask(ctx, req, task.StatusPlanned, "task_plan")
}

// createTask is the shared body of task_plan and other internal
// task-creation flows that need a thin MCP wrapper. status names the
// row's initial state.
func (s *Server) createTask(ctx context.Context, req mcpgo.CallToolRequest, status, verbName string) (*mcpgo.CallToolResult, error) {
	if s.tasks == nil {
		return mcpgo.NewToolResultError(verbName + " unavailable: task store not configured"), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	origin := getString(args, "origin")
	if origin == "" {
		return mcpgo.NewToolResultError(verbName + " requires origin"), nil
	}
	workspaceID := getString(args, "workspace_id")
	if workspaceID == "" {
		if len(memberships) == 0 {
			return mcpgo.NewToolResultError(verbName + ": no caller workspace memberships"), nil
		}
		workspaceID = memberships[0]
	}
	priority := getString(args, "priority")
	if priority == "" {
		priority = task.PriorityMedium
	}
	projectID := getString(args, "project_id")
	kind := getString(args, "kind")
	agentID := getString(args, "agent_id")
	parentTaskID := getString(args, "parent_task_id")
	priorTaskID := getString(args, "prior_task_id")
	triggerRaw := []byte(getString(args, "trigger"))
	expectedStr := getString(args, "expected_duration")
	var expected time.Duration
	if expectedStr != "" {
		if d, err := time.ParseDuration(expectedStr); err == nil {
			expected = d
		}
	}
	// sty_c6d76a5b: when parent_task_id resolves to a known task and the
	// caller didn't override, inherit the parent's project_id / agent_id
	// so successor tasks form a coherent thread without the caller
	// restating each field.
	if parentTaskID != "" && s.tasks != nil {
		if parent, perr := s.tasks.GetByID(ctx, parentTaskID, memberships); perr == nil {
			if projectID == "" {
				projectID = parent.ProjectID
			}
			if agentID == "" {
				agentID = parent.AgentID
			}
		}
	}
	now := time.Now().UTC()
	seed := task.Task{
		WorkspaceID:      workspaceID,
		ProjectID:        projectID,
		Kind:             kind,
		AgentID:          agentID,
		ParentTaskID:     parentTaskID,
		PriorTaskID:      priorTaskID,
		Status:           status,
		Origin:           origin,
		Trigger:          triggerRaw,
		Priority:         priority,
		ExpectedDuration: expected,
	}
	t, err := s.tasks.Enqueue(ctx, seed, now)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("%s: %s", verbName, err)), nil
	}
	ledgerID := ""
	if s.ledger != nil {
		ledgerKind := "kind:task-published"
		if t.Status == task.StatusPlanned {
			ledgerKind = "kind:task-planned"
		} else if t.Status == task.StatusEnqueued {
			ledgerKind = "kind:task-enqueued"
		}
		row, lerr := s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: t.WorkspaceID,
			ProjectID:   t.ProjectID,
			Type:        ledger.TypeDecision,
			Tags: []string{
				ledgerKind,
				"task_id:" + t.ID,
				"origin:" + t.Origin,
				"priority:" + t.Priority,
			},
			Content:    fmt.Sprintf("task created: id=%s status=%s origin=%s priority=%s", t.ID, t.Status, t.Origin, t.Priority),
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
			CreatedBy:  caller.UserID,
		}, now)
		if lerr == nil {
			ledgerID = row.ID
			t.LedgerRootID = row.ID
		}
	}
	return jsonResult(map[string]any{
		"task_id":        t.ID,
		"ledger_root_id": ledgerID,
		"workspace_id":   t.WorkspaceID,
		"status":         t.Status,
		"priority":       t.Priority,
		"origin":         t.Origin,
	})
}

// handleTaskGet implements task_get with workspace scoping.
func (s *Server) handleTaskGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.tasks == nil {
		return mcpgo.NewToolResultError("task_get unavailable"), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	t, err := s.tasks.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(t)
}

// handleTaskList implements task_list.
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
func (s *Server) handleTaskClaim(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.tasks == nil {
		return mcpgo.NewToolResultError("task_claim unavailable"), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	workerID := getString(args, "worker_id")
	if workerID == "" {
		workerID = caller.UserID
	}
	if workerID == "" {
		return mcpgo.NewToolResultError("task_claim requires worker_id"), nil
	}
	workspaceIDs := memberships
	if scoped := getString(args, "workspace_id"); scoped != "" {
		workspaceIDs = []string{scoped}
	}
	now := time.Now().UTC()
	t, err := s.tasks.Claim(ctx, workerID, workspaceIDs, now)
	if errors.Is(err, task.ErrNoTaskAvailable) {
		return jsonResult(nil)
	}
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if s.ledger != nil {
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: t.WorkspaceID,
			ProjectID:   t.ProjectID,
			Type:        ledger.TypeDecision,
			Tags: []string{
				"kind:task-claimed",
				"task_id:" + t.ID,
				"worker_id:" + workerID,
			},
			Content:    fmt.Sprintf("task claimed: id=%s worker=%s", t.ID, workerID),
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
			CreatedBy:  caller.UserID,
		}, now)
	}
	return jsonResult(t)
}

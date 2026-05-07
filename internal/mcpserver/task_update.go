// Package mcpserver — task_update MCP verb (sty_a427368d).
//
// task_update mutates a task's lifecycle state. The first supported
// transition is status=closed: closes the target task and optionally
// records evidence ledger ids. Closure mutates exactly the target row;
// any successor task (review, retry) is authored by the reviewer's
// contract prose via task_add.
//
// task_update replaces the close path on the retired task_submit
// (kind=close) verb. Future updates (priority change, agent
// reassignment) join here as the substrate grows.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/task"
)

// handleTaskUpdate implements `task_update`.
func (s *Server) handleTaskUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)

	if s.tasks == nil {
		return mcpgo.NewToolResultError("task_update unavailable: task store not configured"), nil
	}

	taskID, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	status := strings.TrimSpace(req.GetString("status", ""))
	if status == "" {
		return mcpgo.NewToolResultError("task_update: status is required"), nil
	}

	memberships := s.resolveCallerMemberships(ctx, caller)
	current, err := s.tasks.GetByID(ctx, taskID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_not_found: %s", taskID)), nil
	}

	switch status {
	case task.StatusClosed:
		return s.taskUpdateClose(ctx, req, current, caller, memberships, start)
	default:
		return mcpgo.NewToolResultError(fmt.Sprintf("task_update: unsupported status target %q", status)), nil
	}
}

// taskUpdateClose closes a task and optionally tags evidence rows.
// Closure mutates exactly the target row; no other task is published
// or rewritten as a side effect.
func (s *Server) taskUpdateClose(ctx context.Context, req mcpgo.CallToolRequest, current task.Task, caller CallerIdentity, memberships []string, start time.Time) (*mcpgo.CallToolResult, error) {
	outcome := req.GetString("outcome", task.OutcomeSuccess)
	if outcome != task.OutcomeSuccess && outcome != task.OutcomeFailure {
		return mcpgo.NewToolResultError(fmt.Sprintf("invalid_outcome: %q (expected %q or %q)", outcome, task.OutcomeSuccess, task.OutcomeFailure)), nil
	}
	if current.Status == task.StatusClosed || current.Status == task.StatusArchived {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_already_terminal: %s status=%s", current.ID, current.Status)), nil
	}

	now := s.nowUTC()
	closed, err := s.tasks.Close(ctx, current.ID, outcome, now, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("task close: %v", err)), nil
	}

	rawEvidence := req.GetString("evidence_ledger_ids", "")
	evidenceIDs := parseStringArray(rawEvidence)

	body, _ := json.Marshal(map[string]any{
		"task_id":             closed.ID,
		"status":              closed.Status,
		"outcome":             closed.Outcome,
		"evidence_ledger_ids": evidenceIDs,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "task_update").
		Str("status", task.StatusClosed).
		Str("task_id", current.ID).
		Str("outcome", outcome).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	_ = caller
	return mcpgo.NewToolResultText(string(body)), nil
}

// parseStringArray decodes a JSON array argument as a string slice.
// Empty / invalid input returns nil.
func parseStringArray(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

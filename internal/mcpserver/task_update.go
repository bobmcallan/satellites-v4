// Package mcpserver — task_update MCP verb (sty_a427368d).
//
// task_update mutates a task's lifecycle state. The first supported
// transition is status=closed: closes the target task and optionally
// records evidence ledger ids. Closure mutates exactly the target row;
// any successor task (review, retry) is authored by the reviewer's
// contract prose via task_add.
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/task"
)

// handleTaskUpdate implements `task_update`. Thin forwarder to
// client.TaskUpdate per cli-primary order:07a-layer-2 (sty_df1cb227).
func (s *Server) handleTaskUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	taskID, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	in := client.TaskUpdateInput{
		ID:                taskID,
		Status:            strings.TrimSpace(req.GetString("status", "")),
		Outcome:           req.GetString("outcome", task.OutcomeSuccess),
		EvidenceLedgerIDs: parseStringArray(req.GetString("evidence_ledger_ids", "")),
		Memberships:       memberships,
		Now:               s.nowUTC(),
	}
	out, err := s.cli().TaskUpdate(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"task_id":             out.TaskID,
		"status":              out.Status,
		"outcome":             out.Outcome,
		"evidence_ledger_ids": out.EvidenceLedgerIDs,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "task_update").
		Str("status", out.Status).
		Str("task_id", out.TaskID).
		Str("outcome", out.Outcome).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
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

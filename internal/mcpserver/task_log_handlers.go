package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleTaskLogAppend implements task_log_append: append one task_log
// row. Thin forwarder to client.TaskLogAppend.
func (s *Server) handleTaskLogAppend(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	in := client.TaskLogAppendInput{
		TaskID:      getString(args, "task_id"),
		WorkspaceID: getString(args, "workspace_id"),
		ProjectID:   getString(args, "project_id"),
		Kind:        getString(args, "kind"),
		Memberships: memberships,
		Now:         time.Now().UTC(),
	}
	if v, ok := args["seq"].(float64); ok {
		in.Seq = int64(v)
	}
	if raw := getString(args, "ts"); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			in.TS = t
		}
	}
	if raw := getString(args, "payload"); raw != "" {
		in.Payload = json.RawMessage(raw)
	}
	out, err := s.cli().TaskLogAppend(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(out)
}

// handleTaskLogList implements task_log_list. Read-only — verifies the
// caller's memberships via the typed surface.
func (s *Server) handleTaskLogList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	args := req.GetArguments()
	in := client.TaskLogListInput{
		TaskID:      getString(args, "task_id"),
		Memberships: memberships,
	}
	if v, ok := args["from_seq"].(float64); ok {
		in.FromSeq = int64(v)
	}
	if v, ok := args["limit"].(float64); ok {
		in.Limit = int(v)
	}
	out, err := s.cli().TaskLogList(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(out)
}

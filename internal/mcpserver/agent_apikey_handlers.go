// Package mcpserver — agent_apikey_* adapters.
//
// Slice 7 of sty_f3f7bf9b lifted the business logic into
// internal/client/agent_apikey.go; this file now houses the thin
// wire-layer adapters that parse the MCP request, call the typed
// surface, and marshal the wire envelope. Validation rejections ride
// on *client.AgentAPIKeyError so the adapter can stringify the JSON
// envelope verbatim.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// parseAPIKeyExpiresAt extracts the optional expires_at RFC3339 arg
// and returns either the parsed UTC pointer or a wire-shape
// invalid_expires_at error envelope. Kept beside the create adapter
// so the parse-error fields (raw, parse_err) stay close to the
// request text.
func parseAPIKeyExpiresAt(req mcpgo.CallToolRequest) (*time.Time, *mcpgo.CallToolResult) {
	raw := req.GetString("expires_at", "")
	if raw == "" {
		return nil, nil
	}
	t, perr := time.Parse(time.RFC3339, raw)
	if perr != nil {
		body, _ := json.Marshal(map[string]any{"error": "invalid_expires_at", "message": "expires_at must be RFC3339", "raw": raw, "parse_err": perr.Error()})
		return nil, mcpgo.NewToolResultError(string(body))
	}
	tt := t.UTC()
	return &tt, nil
}

// handleAgentAPIKeyCreate parses the request, calls the typed
// AgentAPIKeyCreate surface, and marshals the wire response.
func (s *Server) handleAgentAPIKeyCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	expiresAt, errRes := parseAPIKeyExpiresAt(req)
	if errRes != nil {
		return errRes, nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	in := client.AgentAPIKeyCreateInput{Name: name, ProjectID: req.GetString("project_id", ""), ExpiresAt: expiresAt, ActorSource: caller.Source, Now: s.nowUTC()}
	out, err := s.cli().AgentAPIKeyCreate(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships, GlobalAdmin: caller.GlobalAdmin}, in)
	if err != nil {
		var envErr *client.AgentAPIKeyError
		if errors.As(err, &envErr) {
			return mcpgo.NewToolResultError(envErr.Body()), nil
		}
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "agent_apikey_create").Str("id", out.ID).Str("owner_user_id", out.OwnerUserID).Str("project_id", out.ProjectID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleAgentAPIKeyList delegates to AgentAPIKeyList and marshals the
// {items,count,project_id} envelope.
func (s *Server) handleAgentAPIKeyList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	in := client.AgentAPIKeyListInput{ProjectID: req.GetString("project_id", ""), IncludeArchived: req.GetBool("include_archived", false)}
	out, err := s.cli().AgentAPIKeyList(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, GlobalAdmin: caller.GlobalAdmin}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{"items": out.Items, "count": out.Count, "project_id": out.ProjectID})
	s.logger.Info().Str("method", "tools/call").Str("tool", "agent_apikey_list").Str("owner", out.Owner).Str("project_id", out.ProjectID).Int("count", out.Count).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleAgentAPIKeyDelete delegates to AgentAPIKeyDelete and marshals
// the {id,status} envelope.
func (s *Server) handleAgentAPIKeyDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	out, err := s.cli().AgentAPIKeyDelete(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, GlobalAdmin: caller.GlobalAdmin}, client.AgentAPIKeyDeleteInput{ID: id, ActorSource: caller.Source, Now: s.nowUTC()})
	if err != nil {
		var envErr *client.AgentAPIKeyError
		if errors.As(err, &envErr) {
			return mcpgo.NewToolResultError(envErr.Body()), nil
		}
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{"id": out.ID, "status": out.Status})
	s.logger.Info().Str("method", "tools/call").Str("tool", "agent_apikey_delete").Str("id", out.ID).Str("actor", caller.UserID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

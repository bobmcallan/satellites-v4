package mcpserver

// kv_handlers.go — MCP wire adapters for the four kv_* verbs
// (kv_get / kv_set / kv_delete / kv_list / kv_get_resolved).
//
// Sty_4db0e025 slice A4: the scope resolution, auth gate, projection,
// and ledger.Append all live on *client.Client via the KVGet /
// KVList / KVGetResolved / KVSet / KVDelete typed methods. This file
// owns wire-shape concerns only — JSON-RPC arg parsing, response
// envelope shaping, structured logging — and delegates the substrate
// work through the typed surface. No internal/ledger or
// internal/workspace import remains.

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleKVGet is the satellites_kv_get verb. Reads the latest non-
// tombstone row for (scope, ids, key) and returns {key, value, scope}.
func (s *Server) handleKVGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scope, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	row, found, err := s.cli().KVGet(ctx, toClientCallerWith(caller, memberships), client.KVGetInput{
		Scope:       scope,
		Key:         key,
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		UserID:      req.GetString("user_id", caller.UserID),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if !found {
		body, _ := json.Marshal(map[string]any{"error": "not_found", "scope": scope, "key": key})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(map[string]any{
		"scope":      row.Scope,
		"key":        row.Key,
		"value":      row.Value,
		"updated_at": row.UpdatedAt,
		"updated_by": row.UpdatedBy,
		"entry_id":   row.EntryID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_get").
		Str("scope", scope).
		Str("key", key).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVSet is the satellites_kv_set verb. Appends a new KV row via
// the typed client surface (auth gate + entry-build + ledger.Append
// all live on *client.Client).
func (s *Server) handleKVSet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scope, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	value, err := req.RequireString("value")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().KVSet(ctx, toClientCallerWith(caller, memberships), client.KVSetInput{
		Scope:       scope,
		Key:         key,
		Value:       value,
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		UserID:      req.GetString("user_id", caller.UserID),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"scope":    out.Scope,
		"key":      out.Key,
		"value":    out.Value,
		"entry_id": out.EntryID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_set").
		Str("scope", string(out.Scope)).
		Str("key", out.Key).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVDelete is the satellites_kv_delete verb. Appends a tombstone
// row via the typed client surface.
func (s *Server) handleKVDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scope, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().KVDelete(ctx, toClientCallerWith(caller, memberships), client.KVDeleteInput{
		Scope:       scope,
		Key:         key,
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		UserID:      req.GetString("user_id", caller.UserID),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"scope":    out.Scope,
		"key":      out.Key,
		"entry_id": out.EntryID,
		"deleted":  true,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_delete").
		Str("scope", string(out.Scope)).
		Str("key", out.Key).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVGetResolved is the satellites_kv_get_resolved verb
// (story_405b7221). Walks system → user → project → workspace,
// returning the first matching value.
func (s *Server) handleKVGetResolved(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	row, found, err := s.cli().KVGetResolved(ctx, toClientCallerWith(caller, memberships), client.KVGetResolvedInput{
		Key:         key,
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		UserID:      req.GetString("user_id", caller.UserID),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if !found {
		body, _ := json.Marshal(map[string]any{"error": "not_found", "key": key})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(map[string]any{
		"key":            row.Key,
		"value":          row.Value,
		"resolved_scope": row.ResolvedScope,
		"updated_at":     row.UpdatedAt,
		"updated_by":     row.UpdatedBy,
		"entry_id":       row.EntryID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_get_resolved").
		Str("key", key).
		Str("resolved_scope", string(row.ResolvedScope)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVList is the satellites_kv_list verb. Returns the full
// projection for a (scope, ids) tuple as a sorted array.
func (s *Server) handleKVList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scope, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedScope, rows, err := s.cli().KVList(ctx, toClientCallerWith(caller, memberships), client.KVListInput{
		Scope:       scope,
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		UserID:      req.GetString("user_id", caller.UserID),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"scope":      row.Scope,
			"key":        row.Key,
			"value":      row.Value,
			"updated_at": row.UpdatedAt,
			"updated_by": row.UpdatedBy,
			"entry_id":   row.EntryID,
		})
	}
	body, _ := json.Marshal(map[string]any{
		"scope": resolvedScope,
		"items": out,
		"count": len(out),
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_list").
		Str("scope", string(resolvedScope)).
		Int("count", len(out)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

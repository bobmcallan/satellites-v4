package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"sort"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// validKVScope reports whether s is one of the four supported scope
// values. Returns the strongly-typed KVScope on success.
func validKVScope(s string) (ledger.KVScope, bool) {
	switch ledger.KVScope(s) {
	case ledger.KVScopeSystem, ledger.KVScopeWorkspace, ledger.KVScopeProject, ledger.KVScopeUser:
		return ledger.KVScope(s), true
	default:
		return "", false
	}
}

// kvCheckWriteAuth enforces the per-scope role gate for kv_set and
// kv_delete (story_eb17cb16). Reads remain unrestricted within
// workspace boundaries.
//
//   - system: caller.GlobalAdmin == true. The seed loader writes via
//     the internal Append path, not MCP; this gate covers MCP callers.
//   - workspace: caller is RoleAdmin of opts.WorkspaceID.
//   - project: caller is project.OwnerUserID OR workspace admin of
//     project.WorkspaceID.
//   - user: caller.UserID == opts.UserID. v1 default is self-only;
//     cross-user writes are not permitted (cross-tier admin override
//     deferred to a future story).
//
// Returns nil on permit and a structured "forbidden: scope=X requires
// role=Y" error on reject. Errors are user-safe.
func (s *Server) kvCheckWriteAuth(ctx context.Context, scope ledger.KVScope, opts ledger.KVProjectionOptions, caller auth.CallerIdentity) error {
	switch scope {
	case ledger.KVScopeSystem:
		if !caller.GlobalAdmin {
			return fmt.Errorf("forbidden: scope=system requires role=global_admin")
		}
		return nil
	case ledger.KVScopeWorkspace:
		if caller.GlobalAdmin {
			return nil
		}
		if s.workspaces == nil {
			return fmt.Errorf("forbidden: workspace store unavailable")
		}
		role, err := s.workspaces.GetRole(ctx, opts.WorkspaceID, caller.UserID)
		if err != nil || role != workspace.RoleAdmin {
			return fmt.Errorf("forbidden: scope=workspace requires role=workspace_admin")
		}
		return nil
	case ledger.KVScopeProject:
		if caller.GlobalAdmin {
			return nil
		}
		if s.projects == nil {
			return fmt.Errorf("forbidden: project store unavailable")
		}
		p, err := s.projects.GetByID(ctx, opts.ProjectID, nil)
		if err == nil && p.OwnerUserID == caller.UserID {
			return nil
		}
		// Workspace admin of the project's workspace also passes.
		if s.workspaces != nil && opts.WorkspaceID != "" {
			role, rerr := s.workspaces.GetRole(ctx, opts.WorkspaceID, caller.UserID)
			if rerr == nil && role == workspace.RoleAdmin {
				return nil
			}
		}
		return fmt.Errorf("forbidden: scope=project requires role=project_owner_or_workspace_admin")
	case ledger.KVScopeUser:
		// v1 default: only self may write to their own user-scope KV.
		// Cross-user writes (workspace/global admins overriding) deferred.
		if caller.UserID == "" || caller.UserID != opts.UserID {
			return fmt.Errorf("forbidden: scope=user is self-only (v1)")
		}
		return nil
	default:
		return fmt.Errorf("forbidden: unknown scope %q", scope)
	}
}

// resolveKVScopeArgs parses the scope-specific identifier arguments from
// a verb request and returns the projection options. Caller-supplied
// project_id is resolved through the existing helper so aliases (slug,
// name, "default") work; workspace_id for project rows is derived from
// the resolved project; user_id defaults to the caller for user scope.
//
// Returns the populated options + a memberships slice that includes the
// system sentinel ("") for system-scope reads. Errors are user-safe.
func (s *Server) resolveKVScopeArgs(ctx context.Context, scope ledger.KVScope, req mcpgo.CallToolRequest, caller auth.CallerIdentity) (ledger.KVProjectionOptions, []string, error) {
	memberships := s.resolveCallerMemberships(ctx, caller)
	opts := ledger.KVProjectionOptions{Scope: scope}
	switch scope {
	case ledger.KVScopeSystem:
		// System scope rows live with workspace_id="" — extend memberships
		// with the empty-string sentinel so the ledger membership filter
		// admits them.
		memberships = append([]string{""}, memberships...)
	case ledger.KVScopeWorkspace:
		wsID := req.GetString("workspace_id", "")
		if wsID == "" {
			return opts, nil, fmt.Errorf("workspace_id is required for scope=workspace")
		}
		opts.WorkspaceID = wsID
	case ledger.KVScopeProject:
		projectID := req.GetString("project_id", "")
		if projectID == "" {
			return opts, nil, fmt.Errorf("project_id is required for scope=project")
		}
		resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
		if err != nil {
			return opts, nil, err
		}
		opts.ProjectID = resolvedID
		opts.WorkspaceID = s.resolveProjectWorkspaceID(ctx, resolvedID)
	case ledger.KVScopeUser:
		wsID := req.GetString("workspace_id", "")
		if wsID == "" {
			return opts, nil, fmt.Errorf("workspace_id is required for scope=user")
		}
		userID := req.GetString("user_id", caller.UserID)
		if userID == "" {
			return opts, nil, fmt.Errorf("user_id is required for scope=user")
		}
		opts.WorkspaceID = wsID
		opts.UserID = userID
	}
	return opts, memberships, nil
}

// kvWriteEntry constructs the LedgerEntry for a KV write or delete.
// Tags include `scope:<scope>` plus `key:<name>`; user-scope rows add
// `user:<id>`; deletes add `kind:tombstone`.
func kvWriteEntry(opts ledger.KVProjectionOptions, key, value, actor string, tombstone bool) ledger.LedgerEntry {
	tags := []string{
		string(kvScopeTag(opts.Scope)),
		"key:" + key,
	}
	if opts.Scope == ledger.KVScopeUser && opts.UserID != "" {
		tags = append(tags, "user:"+opts.UserID)
	}
	if tombstone {
		tags = append(tags, ledger.KVTombstoneTag)
	}
	entry := ledger.LedgerEntry{
		WorkspaceID: opts.WorkspaceID,
		Type:        ledger.TypeKV,
		Tags:        tags,
		Content:     value,
		CreatedBy:   actor,
	}
	if opts.Scope == ledger.KVScopeProject {
		entry.ProjectID = opts.ProjectID
	}
	return entry
}

// kvScopeTag returns the `scope:<scope>` tag string for opts.Scope.
func kvScopeTag(scope ledger.KVScope) string {
	return "scope:" + string(scope)
}

// handleKVGet is the satellites_kv_get verb. Reads the latest non-
// tombstone row for (scope, ids, key) and returns {key, value, scope}.
func (s *Server) handleKVGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scopeStr, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	scope, ok := validKVScope(scopeStr)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("invalid scope %q (want system|workspace|project|user)", scopeStr)), nil
	}
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	opts, memberships, err := s.resolveKVScopeArgs(ctx, scope, req, caller)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	rows, err := ledger.KVProjectionScoped(ctx, s.ledger, opts, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	row, present := rows[key]
	if !present {
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
		Str("scope", string(scope)).
		Str("key", key).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVSet is the satellites_kv_set verb. Appends a new KV row.
// Per-scope role gates land in story_eb17cb16 (#4); this story checks
// only that the caller is authenticated and meets the minimal
// scope-shape requirements (system scope requires GlobalAdmin).
func (s *Server) handleKVSet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scopeStr, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	scope, ok := validKVScope(scopeStr)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("invalid scope %q (want system|workspace|project|user)", scopeStr)), nil
	}
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	value, err := req.RequireString("value")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	opts, _, err := s.resolveKVScopeArgs(ctx, scope, req, caller)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.kvCheckWriteAuth(ctx, scope, opts, caller); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	entry := kvWriteEntry(opts, key, value, caller.UserID, false)
	row, err := s.ledger.Append(ctx, entry, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"scope":    scope,
		"key":      key,
		"value":    value,
		"entry_id": row.ID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_set").
		Str("scope", string(scope)).
		Str("key", key).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVDelete is the satellites_kv_delete verb. Appends a tombstone
// row. The append-only ledger has no Delete primitive; the projection
// suppresses keys whose latest row carries the tombstone tag.
func (s *Server) handleKVDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scopeStr, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	scope, ok := validKVScope(scopeStr)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("invalid scope %q (want system|workspace|project|user)", scopeStr)), nil
	}
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	opts, _, err := s.resolveKVScopeArgs(ctx, scope, req, caller)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.kvCheckWriteAuth(ctx, scope, opts, caller); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	entry := kvWriteEntry(opts, key, "", caller.UserID, true)
	row, err := s.ledger.Append(ctx, entry, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"scope":    scope,
		"key":      key,
		"entry_id": row.ID,
		"deleted":  true,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_delete").
		Str("scope", string(scope)).
		Str("key", key).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVGetResolved is the satellites_kv_get_resolved verb (story_405b7221).
// Walks system → user → project → workspace, returning the first
// matching value. Missing identifiers skip the corresponding tier.
//
// Reads remain unrestricted within workspace boundaries (memberships
// filter handles tenancy). The caller's user_id defaults to caller.UserID.
func (s *Server) handleKVGetResolved(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	// Include the system sentinel so system-tier rows are visible to
	// resolved reads from any caller.
	memberships = append([]string{""}, memberships...)
	opts := ledger.KVResolveOptions{
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		UserID:      req.GetString("user_id", caller.UserID),
	}
	if opts.ProjectID != "" && opts.WorkspaceID == "" {
		// Resolve workspace from the project so the workspace-tier
		// fallback can still run.
		opts.WorkspaceID = s.resolveProjectWorkspaceID(ctx, opts.ProjectID)
	}
	row, found, err := ledger.KVResolveScoped(ctx, s.ledger, key, opts, memberships)
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
		"resolved_scope": row.Scope,
		"updated_at":     row.UpdatedAt,
		"updated_by":     row.UpdatedBy,
		"entry_id":       row.EntryID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_get_resolved").
		Str("key", key).
		Str("resolved_scope", string(row.Scope)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleKVList is the satellites_kv_list verb. Returns the full
// projection for a (scope, ids) tuple as a sorted array.
func (s *Server) handleKVList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	scopeStr, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	scope, ok := validKVScope(scopeStr)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("invalid scope %q (want system|workspace|project|user)", scopeStr)), nil
	}
	opts, memberships, err := s.resolveKVScopeArgs(ctx, scope, req, caller)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	rows, err := ledger.KVProjectionScoped(ctx, s.ledger, opts, memberships)
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
	sort.Slice(out, func(i, j int) bool {
		return out[i]["key"].(string) < out[j]["key"].(string)
	})
	body, _ := json.Marshal(map[string]any{
		"scope": scope,
		"items": out,
		"count": len(out),
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "kv_list").
		Str("scope", string(scope)).
		Int("count", len(out)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

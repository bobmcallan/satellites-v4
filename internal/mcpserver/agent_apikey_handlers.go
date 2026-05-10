// Package mcpserver — agent_apikey_* handlers (story_3191fbfc).
//
// Three verbs let an authenticated operator mint, list, and
// soft-delete substrate-managed agent api-keys. The keys are
// consumed on the auth side by the AuthMiddleware fall-through
// (see auth.go) — agents that hold a `sat_<…>` cleartext present
// it as `Authorization: Bearer …` and the middleware resolves the
// owning user via auth.APIKeyStore.LookupByToken.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// apiKeyMetadata is the wire shape returned by agent_apikey_list and
// the metadata block of agent_apikey_create. The cleartext key is
// emitted ONCE on create — under a separate `key` field — and never
// echoed on list or get. KeyHash and KeySalt are server-side only and
// are deliberately absent from this struct.
type apiKeyMetadata struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	ProjectID   string     `json:"project_id,omitempty"`
	OwnerUserID string     `json:"owner_user_id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toAPIKeyMetadata(k auth.APIKey) apiKeyMetadata {
	return apiKeyMetadata{
		ID:          k.ID,
		WorkspaceID: k.WorkspaceID,
		ProjectID:   k.ProjectID,
		OwnerUserID: k.OwnerUserID,
		Name:        k.Name,
		Prefix:      k.Prefix,
		Status:      k.Status,
		ExpiresAt:   k.ExpiresAt,
		LastUsedAt:  k.LastUsedAt,
		CreatedAt:   k.CreatedAt,
	}
}

// handleAgentAPIKeyCreate mints a new agent api-key, persists the
// hash + salt + metadata, writes a kind:agent-apikey-created ledger
// row, and returns the cleartext key ONCE. The cleartext is never
// retrievable after this call.
func (s *Server) handleAgentAPIKeyCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	if s.apiKeys == nil {
		return mcpgo.NewToolResultError("agent_apikey: store not configured"), nil
	}

	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if name == "" {
		return mcpgo.NewToolResultError("name is required"), nil
	}
	projectID := req.GetString("project_id", "")
	expiresArg := req.GetString("expires_at", "")
	var expiresAt *time.Time
	if expiresArg != "" {
		t, perr := time.Parse(time.RFC3339, expiresArg)
		if perr != nil {
			errBody, _ := json.Marshal(map[string]any{
				"error":     "invalid_expires_at",
				"message":   "expires_at must be RFC3339",
				"raw":       expiresArg,
				"parse_err": perr.Error(),
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
		tt := t.UTC()
		expiresAt = &tt
	}

	memberships := s.resolveCallerMemberships(ctx, caller)
	workspaceID := ""
	if projectID != "" {
		workspaceID = s.resolveProjectWorkspaceID(ctx, projectID)
		if workspaceID == "" {
			errBody, _ := json.Marshal(map[string]any{
				"error":      "project_not_found",
				"project_id": projectID,
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
		// Cross-tenant write guard: the project's workspace must be in
		// the caller's membership slice (or the caller is a global
		// admin).
		if !caller.GlobalAdmin && !ledgerWorkspaceInMemberships(workspaceID, memberships) {
			errBody, _ := json.Marshal(map[string]any{
				"error":      "forbidden",
				"reason":     "caller is not a member of the project's workspace",
				"project_id": projectID,
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
	} else {
		workspaceID = s.resolveCallerWorkspaceID(ctx, caller)
	}

	cleartext, salt, err := auth.GenerateAPIKey()
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("generate api key: %v", err)), nil
	}
	now := s.nowUTC()
	row := auth.APIKey{
		ID:          auth.NewAPIKeyID(),
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		OwnerUserID: caller.UserID,
		Name:        name,
		Prefix:      auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:     auth.HashAPIKey(salt, cleartext),
		KeySalt:     salt,
		Status:      auth.APIKeyStatusActive,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
	}
	if err := s.apiKeys.Create(ctx, row); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store create: %v", err)), nil
	}

	// kind:agent-apikey-created ledger row. Cleartext is NOT logged
	// — only the prefix + id + actor — so the ledger never carries a
	// pre-image of the secret.
	auditPayload, _ := json.Marshal(map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"prefix":        row.Prefix,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"actor":         caller.UserID,
		"actor_source":  caller.Source,
	})
	if s.ledger != nil {
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-apikey-created", "apikey:" + row.ID},
			Content:     "agent api-key minted",
			Structured:  auditPayload,
			CreatedBy:   caller.UserID,
		}, now)
	}

	resp := map[string]any{
		"id":            row.ID,
		"key":           cleartext, // raw key — last call that returns this
		"prefix":        row.Prefix,
		"name":          row.Name,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"status":        row.Status,
		"expires_at":    row.ExpiresAt,
		"created_at":    row.CreatedAt,
	}
	body, _ := json.Marshal(resp)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "agent_apikey_create").
		Str("id", row.ID).
		Str("owner_user_id", row.OwnerUserID).
		Str("project_id", row.ProjectID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleAgentAPIKeyList returns the caller's owned api-keys. Global
// admins see every key; the project_id arg further narrows the list.
// The raw key, key_hash, and key_salt are absent from every row.
func (s *Server) handleAgentAPIKeyList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	if s.apiKeys == nil {
		return mcpgo.NewToolResultError("agent_apikey: store not configured"), nil
	}
	projectID := req.GetString("project_id", "")
	includeArchived := req.GetBool("include_archived", false)

	owner := caller.UserID
	if caller.GlobalAdmin {
		owner = "" // admin sees all owners
	}
	rows, err := s.apiKeys.List(ctx, owner, projectID, includeArchived)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store list: %v", err)), nil
	}

	out := make([]apiKeyMetadata, 0, len(rows))
	for _, k := range rows {
		out = append(out, toAPIKeyMetadata(k))
	}
	body, _ := json.Marshal(map[string]any{
		"items":      out,
		"count":      len(out),
		"project_id": projectID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "agent_apikey_list").
		Str("owner", owner).
		Str("project_id", projectID).
		Int("count", len(out)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleAgentAPIKeyDelete soft-deletes the row and writes a
// kind:agent-apikey-archived ledger row. Cross-owner deletes are
// forbidden unless the caller is a global admin.
func (s *Server) handleAgentAPIKeyDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	if s.apiKeys == nil {
		return mcpgo.NewToolResultError("agent_apikey: store not configured"), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	row, err := s.apiKeys.Get(ctx, id)
	if err != nil {
		if errors.Is(err, auth.ErrAPIKeyNotFound) {
			errBody, _ := json.Marshal(map[string]any{
				"error": "agent_apikey_not_found",
				"id":    id,
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
		return mcpgo.NewToolResultError(fmt.Sprintf("store get: %v", err)), nil
	}
	if row.OwnerUserID != caller.UserID && !caller.GlobalAdmin {
		errBody, _ := json.Marshal(map[string]any{
			"error":  "forbidden",
			"reason": "caller does not own the api-key",
			"id":     id,
		})
		return mcpgo.NewToolResultError(string(errBody)), nil
	}
	if row.Status == auth.APIKeyStatusArchived {
		// Idempotent: re-deleting is a no-op but still returns the
		// archived shape so callers needn't differentiate.
		body, _ := json.Marshal(map[string]any{
			"id":     row.ID,
			"status": row.Status,
		})
		return mcpgo.NewToolResultText(string(body)), nil
	}

	if err := s.apiKeys.Delete(ctx, id); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store delete: %v", err)), nil
	}

	now := s.nowUTC()
	auditPayload, _ := json.Marshal(map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"prefix":        row.Prefix,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"actor":         caller.UserID,
		"actor_source":  caller.Source,
	})
	if s.ledger != nil {
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: row.WorkspaceID,
			ProjectID:   row.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-apikey-archived", "apikey:" + row.ID, "actor:" + caller.UserID},
			Content:     "agent api-key archived",
			Structured:  auditPayload,
			CreatedBy:   caller.UserID,
		}, now)
	}

	body, _ := json.Marshal(map[string]any{
		"id":     row.ID,
		"status": auth.APIKeyStatusArchived,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "agent_apikey_delete").
		Str("id", row.ID).
		Str("actor", caller.UserID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

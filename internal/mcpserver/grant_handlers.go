package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/rolegrant"
)

// handleAgentRoleClaim implements the agent_role_claim MCP verb.
//
// On success: writes a role_grant row (status=active) + a kind:role-grant,
// event:claimed ledger row; returns {grant_id, effective_verbs, role_id,
// agent_id, workspace_id, grantee_kind, grantee_id, issued_at}.
func (s *Server) handleAgentRoleClaim(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.grants == nil {
		return mcpgo.NewToolResultError("agent_role_claim unavailable: role-grant store not configured"), nil
	}
	if s.docs == nil {
		return mcpgo.NewToolResultError("agent_role_claim unavailable: document store not configured"), nil
	}
	args := req.GetArguments()
	workspaceID := getString(args, "workspace_id")
	roleID := getString(args, "role_id")
	agentID := getString(args, "agent_id")
	granteeKind := getString(args, "grantee_kind")
	granteeID := getString(args, "grantee_id")
	projectID := getString(args, "project_id")

	if workspaceID == "" || roleID == "" || agentID == "" || granteeKind == "" || granteeID == "" {
		return mcpgo.NewToolResultError("agent_role_claim requires workspace_id, role_id, agent_id, grantee_kind, grantee_id"), nil
	}

	roleDoc, err := s.docs.GetByID(ctx, roleID, nil)
	if err != nil || roleDoc.Type != document.TypeRole || roleDoc.Status != document.StatusActive {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: role_id %q does not resolve to an active type=role document", roleID)), nil
	}
	agentDoc, err := s.docs.GetByID(ctx, agentID, nil)
	if err != nil || agentDoc.Type != document.TypeAgent || agentDoc.Status != document.StatusActive {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: agent_id %q does not resolve to an active type=agent document", agentID)), nil
	}

	agentPayload, err := decodeAgentPayload(agentDoc.Structured)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: agent %q structured payload malformed: %s", agentID, err)), nil
	}
	if !contains(agentPayload.PermittedRoles, roleID) {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: role %q not in agent %q permitted_roles", roleID, agentID)), nil
	}

	rolePayload, err := decodeRolePayload(roleDoc.Structured)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: role %q structured payload malformed: %s", roleID, err)), nil
	}
	if !ceilingCovers(agentPayload.ToolCeiling, rolePayload.AllowedMCPVerbs) {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: agent %q tool_ceiling does not cover role %q allowed_mcp_verbs", agentID, roleID)), nil
	}

	effective := intersectPatterns(agentPayload.ToolCeiling, rolePayload.AllowedMCPVerbs)

	now := time.Now().UTC()
	var projectPtr *string
	if projectID != "" {
		pid := projectID
		projectPtr = &pid
	}
	grant, err := s.grants.Create(ctx, rolegrant.RoleGrant{
		WorkspaceID: workspaceID,
		ProjectID:   projectPtr,
		RoleID:      roleID,
		AgentID:     agentID,
		GranteeKind: granteeKind,
		GranteeID:   granteeID,
		Status:      rolegrant.StatusActive,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_claim: %s", err)), nil
	}

	// Audit ledger row. Missing ledger store is not fatal — the grant
	// itself is the load-bearing artefact.
	if s.ledger != nil {
		caller, _ := UserFrom(ctx)
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Type:        ledger.TypeDecision,
			Content:     fmt.Sprintf("role-grant claimed: grant=%s role=%s agent=%s grantee=%s:%s", grant.ID, roleID, agentID, granteeKind, granteeID),
			Tags:        []string{"kind:role-grant", "event:claimed", "grant_id:" + grant.ID, "role_id:" + roleID, "agent_id:" + agentID, "grantee_id:" + granteeID},
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			Status:      ledger.StatusActive,
			CreatedBy:   caller.UserID,
		}, now)
	}

	payload := map[string]any{
		"grant_id":        grant.ID,
		"workspace_id":    grant.WorkspaceID,
		"role_id":         grant.RoleID,
		"agent_id":        grant.AgentID,
		"grantee_kind":    grant.GranteeKind,
		"grantee_id":      grant.GranteeID,
		"status":          grant.Status,
		"issued_at":       grant.IssuedAt,
		"effective_verbs": effective,
	}
	return jsonResult(payload)
}

// handleAgentRoleRelease implements the agent_role_release MCP verb.
//
// Idempotent: second call on a released grant returns the grant with a
// release-redundant ledger row; status/released_at are preserved.
func (s *Server) handleAgentRoleRelease(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.grants == nil {
		return mcpgo.NewToolResultError("agent_role_release unavailable: role-grant store not configured"), nil
	}
	args := req.GetArguments()
	grantID := getString(args, "grant_id")
	reason := getString(args, "reason")
	if grantID == "" {
		return mcpgo.NewToolResultError("agent_role_release requires grant_id"), nil
	}
	now := time.Now().UTC()
	grant, err := s.grants.Release(ctx, grantID, reason, now, nil)
	redundant := errors.Is(err, rolegrant.ErrAlreadyReleased)
	if err != nil && !redundant {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_release: %s", err)), nil
	}
	if s.ledger != nil {
		event := "released"
		if redundant {
			event = "release-redundant"
		}
		caller, _ := UserFrom(ctx)
		projID := ""
		if grant.ProjectID != nil {
			projID = *grant.ProjectID
		}
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: grant.WorkspaceID,
			ProjectID:   projID,
			Type:        ledger.TypeDecision,
			Content:     fmt.Sprintf("role-grant %s: grant=%s reason=%q", event, grant.ID, reason),
			Tags:        []string{"kind:role-grant", "event:" + event, "grant_id:" + grant.ID},
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			Status:      ledger.StatusActive,
			CreatedBy:   caller.UserID,
		}, now)
	}
	payload := map[string]any{
		"grant_id":    grant.ID,
		"status":      grant.Status,
		"released_at": grant.ReleasedAt,
		"redundant":   redundant,
	}
	return jsonResult(payload)
}

// handleAgentRoleList implements the agent_role_list MCP verb.
func (s *Server) handleAgentRoleList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.grants == nil {
		return mcpgo.NewToolResultError("agent_role_list unavailable: role-grant store not configured"), nil
	}
	args := req.GetArguments()
	opts := rolegrant.ListOptions{
		RoleID:      getString(args, "role_id"),
		AgentID:     getString(args, "agent_id"),
		GranteeKind: getString(args, "grantee_kind"),
		GranteeID:   getString(args, "grantee_id"),
		Status:      getString(args, "status"),
	}
	if v, ok := args["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	rows, err := s.grants.List(ctx, opts, nil)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_role_list: %s", err)), nil
	}
	return jsonResult(rows)
}

// agentPayload captures the subset of agent.structured consumed by the
// grant handlers. Unknown fields are ignored.
type agentPayload struct {
	PermittedRoles []string `json:"permitted_roles"`
	ToolCeiling    []string `json:"tool_ceiling"`
}

// rolePayload captures the subset of role.structured consumed by the
// grant handlers.
type rolePayload struct {
	AllowedMCPVerbs []string `json:"allowed_mcp_verbs"`
	RequiredHooks   []string `json:"required_hooks"`
}

func decodeAgentPayload(raw []byte) (agentPayload, error) {
	if len(raw) == 0 {
		return agentPayload{}, errors.New("empty structured payload")
	}
	var p agentPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return agentPayload{}, err
	}
	return p, nil
}

func decodeRolePayload(raw []byte) (rolePayload, error) {
	if len(raw) == 0 {
		return rolePayload{}, errors.New("empty structured payload")
	}
	var p rolePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return rolePayload{}, err
	}
	return p, nil
}

// contains returns true when s appears in the haystack.
func contains(haystack []string, s string) bool {
	for _, h := range haystack {
		if h == s {
			return true
		}
	}
	return false
}

// ceilingCovers returns true when every verb in want is matched by at
// least one pattern in ceiling. Patterns support a single trailing "*"
// wildcard (e.g. "document_*" matches "document_get").
func ceilingCovers(ceiling, want []string) bool {
	for _, w := range want {
		if !verbMatchesAny(w, ceiling) {
			return false
		}
	}
	return true
}

// intersectPatterns returns the subset of want that matches at least one
// pattern in ceiling. Used to compute effective_verbs.
func intersectPatterns(ceiling, want []string) []string {
	out := make([]string, 0, len(want))
	for _, w := range want {
		if verbMatchesAny(w, ceiling) {
			out = append(out, w)
		}
	}
	return out
}

// verbMatchesAny tests verb against each pattern in patterns. A pattern
// is either an exact match or a prefix with trailing "*".
func verbMatchesAny(verb string, patterns []string) bool {
	for _, p := range patterns {
		if verbMatches(verb, p) {
			return true
		}
	}
	return false
}

// verbMatches tests whether verb matches a single pattern.
func verbMatches(verb, pattern string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(verb, strings.TrimSuffix(pattern, "*"))
	}
	return verb == pattern
}

// getString reads a string arg without panicking on absence / wrong type.
func getString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// jsonResult wraps an arbitrary payload as a structured MCP tool result.
func jsonResult(payload any) (*mcpgo.CallToolResult, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("jsonResult: marshal: %w", err)
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

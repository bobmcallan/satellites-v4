package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/bobmcallan/satellites/internal/rolegrant"
)

// bootstrapVerbs is the static allowlist of tools that bypass the grant
// middleware even when GrantsEnforced is on. These are either (a)
// identity / metadata queries needed before a caller can claim a grant,
// or (b) the grant-lifecycle verbs themselves — a caller needs them to
// obtain a grant in the first place.
var bootstrapVerbs = map[string]struct{}{
	"satellites_info":     {},
	"session_whoami":      {},
	"agent_role_claim":    {},
	"agent_role_release":  {},
	"agent_role_list":     {},
}

// grantMiddleware returns a mcp-go ToolHandlerMiddleware that enforces
// the role-grant gate on every tool call.
//
// Enforcement is gated on two conditions:
//  1. Config.GrantsEnforced is true AND
//  2. RoleGrantStore is wired (s.grants != nil).
//
// When either is absent, the middleware is a pass-through so existing
// deployments and tests keep working. Story_7d9c4b1b (6.4) will flip
// GrantsEnforced=true alongside SessionStart orchestrator grants.
func (s *Server) grantMiddleware() mcpserver.ToolHandlerMiddleware {
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if !s.grantsEnforced() {
				return next(ctx, req)
			}
			tool := req.Params.Name
			if _, ok := bootstrapVerbs[tool]; ok {
				return next(ctx, req)
			}
			caller, ok := UserFrom(ctx)
			if !ok || caller.UserID == "" {
				return grantDenied(tool, "no authenticated caller"), nil
			}
			grants, err := s.grants.List(ctx, rolegrant.ListOptions{
				GranteeID: caller.UserID,
				Status:    rolegrant.StatusActive,
			}, nil)
			if err != nil {
				return grantDenied(tool, fmt.Sprintf("grant lookup failed: %s", err)), nil
			}
			if len(grants) == 0 {
				return grantDenied(tool, "caller holds no active grants"), nil
			}
			if !anyGrantCovers(grants, tool, s) {
				return grantDenied(tool, "no grant covers this verb"), nil
			}
			return next(ctx, req)
		}
	}
}

// grantsEnforced returns true only when configuration AND wiring both
// call for enforcement.
func (s *Server) grantsEnforced() bool {
	if s.grants == nil {
		return false
	}
	if s.cfg == nil {
		return false
	}
	return s.cfg.GrantsEnforced
}

// anyGrantCovers resolves each grant's role document and checks whether
// the tool name matches the role's allowed_mcp_verbs (intersected with
// the agent's tool_ceiling). Matches use the same glob rules as the
// claim path (exact or trailing-"*" prefix).
//
// If any lookup fails (role archived, payload malformed), that single
// grant is skipped; other grants can still cover the call. A complete
// lookup failure across all grants denies the call.
func anyGrantCovers(grants []rolegrant.RoleGrant, tool string, s *Server) bool {
	for _, g := range grants {
		role, err := s.docs.GetByID(context.Background(), g.RoleID, nil)
		if err != nil {
			continue
		}
		agent, err := s.docs.GetByID(context.Background(), g.AgentID, nil)
		if err != nil {
			continue
		}
		rp, err := decodeRolePayload(role.Structured)
		if err != nil {
			continue
		}
		ap, err := decodeAgentPayload(agent.Structured)
		if err != nil {
			continue
		}
		effective := intersectPatterns(ap.ToolCeiling, rp.AllowedMCPVerbs)
		if verbMatchesAny(tool, effective) {
			return true
		}
	}
	return false
}

// grantDenied builds a structured tool-error response naming the verb
// and the reason. The text is intentionally specific so integration tests
// can assert on it.
func grantDenied(tool, reason string) *mcpgo.CallToolResult {
	payload := map[string]any{
		"error":  "grant_required",
		"tool":   tool,
		"reason": reason,
	}
	b, _ := json.Marshal(payload)
	return mcpgo.NewToolResultError(fmt.Sprintf("grant_required: %s (tool=%s): %s", reason, tool, string(b)))
}

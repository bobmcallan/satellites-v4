package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/session"
)

// resolveSessionStaleness returns the configured claim-staleness window.
// Env SATELLITES_SESSION_STALENESS (seconds) overrides the default.
func resolveSessionStaleness() time.Duration {
	if raw := os.Getenv("SATELLITES_SESSION_STALENESS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return session.StalenessDefault
}

// handleStoryContractClaim is the keystone claim verb: it runs the
// process-order gate, verifies the session, writes action-claim and
// optional plan ledger rows, and transitions the CI to claimed.
// Same-session re-claim is an amend: prior plan + action_claim rows
// are dereferenced and rewritten.
func (s *Server) handleStoryContractClaim(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	ciID, err := req.RequireString("contract_instance_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	sessionID, err := req.RequireString("session_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	permissionsClaim := req.GetStringSlice("permissions_claim", nil)
	skillsUsed := req.GetStringSlice("skills_used", nil)
	planMarkdown := req.GetString("plan_markdown", "")

	memberships := s.resolveCallerMemberships(ctx, caller)
	ci, err := s.contracts.GetByID(ctx, ciID, memberships)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": "ci_not_found", "contract_instance_id": ciID})
		return mcpgo.NewToolResultError(string(body)), nil
	}

	// Same-session amend path: CI already claimed, claimer = this
	// session → dereference prior action_claim + plan rows and write
	// fresh ones.
	amend := ci.Status == contract.StatusClaimed && ci.ClaimedBySessionID == sessionID
	if ci.Status == contract.StatusClaimed && !amend {
		body, _ := json.Marshal(map[string]any{
			"error":                "wrong_session",
			"contract_instance_id": ciID,
			"claimed_by":           ci.ClaimedBySessionID,
		})
		return mcpgo.NewToolResultError(string(body)), nil
	}

	if !amend {
		if err := contract.CheckCIReady(ci); err != nil {
			return mcpgo.NewToolResultError(marshalGateRejection(err)), nil
		}
		peers, err := s.contracts.List(ctx, ci.StoryID, memberships)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if err := contract.PredecessorGate(peers, ci); err != nil {
			return mcpgo.NewToolResultError(marshalGateRejection(err)), nil
		}
	}

	// Session registry: must be registered + not stale.
	if err := s.verifyCallerSession(ctx, caller.UserID, sessionID, time.Now().UTC()); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	// Role-grant gate (story_85675c33). If the contract's structured
	// payload names a required_role AND we have the grant/doc machinery
	// wired, verify the caller's active orchestrator grant's role id
	// matches. No required_role OR no wiring = fall through to the
	// legacy session-only path.
	grantID, gateErr := s.resolveRequiredRoleGrant(ctx, ci, caller.UserID, sessionID)
	if gateErr != nil {
		return mcpgo.NewToolResultError(gateErr.Error()), nil
	}

	// If amending, dereference prior action_claim + plan rows before
	// writing fresh ones.
	if amend {
		if ci.PlanLedgerID != "" {
			_, _ = s.ledger.Dereference(ctx, ci.PlanLedgerID, "amended", caller.UserID, time.Now().UTC(), memberships)
		}
		// The action_claim row shares the CI scope but isn't tracked on
		// the CI directly — find the latest active action_claim row for
		// this CI and dereference it.
		if priorAC := s.findLatestActionClaim(ctx, ci, memberships); priorAC != "" {
			_, _ = s.ledger.Dereference(ctx, priorAC, "amended", caller.UserID, time.Now().UTC(), memberships)
		}
	}

	now := time.Now().UTC()
	acStructured, _ := json.Marshal(map[string]any{
		"permissions_claim": permissionsClaim,
		"skills_used":       skillsUsed,
	})
	acRow, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: ci.WorkspaceID,
		ProjectID:   ci.ProjectID,
		StoryID:     ledger.StringPtr(ci.StoryID),
		ContractID:  ledger.StringPtr(ci.ID),
		Type:        ledger.TypeActionClaim,
		Tags:        []string{"kind:action-claim", "phase:" + ci.ContractName},
		Content:     "action claim",
		Structured:  acStructured,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	var planRowID string
	if planMarkdown != "" {
		planRow, err := s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: ci.WorkspaceID,
			ProjectID:   ci.ProjectID,
			StoryID:     ledger.StringPtr(ci.StoryID),
			ContractID:  ledger.StringPtr(ci.ID),
			Type:        ledger.TypePlan,
			Tags:        []string{"kind:plan", "phase:" + ci.ContractName},
			Content:     planMarkdown,
			CreatedBy:   caller.UserID,
		}, now)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		planRowID = planRow.ID
	}

	if !amend {
		if _, err := s.contracts.Claim(ctx, ci.ID, sessionID, now, memberships); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
	}
	if grantID != "" {
		if _, err := s.contracts.SetClaimedViaGrant(ctx, ci.ID, grantID, now, memberships); err != nil {
			// Non-fatal — the session_id path still authoritative.
			if s.logger != nil {
				s.logger.Warn().Str("ci_id", ci.ID).Str("grant_id", grantID).Str("error", err.Error()).Msg("claim: SetClaimedViaGrant failed; session_id path preserved")
			}
		}
	}
	if planRowID != "" {
		planRef := planRowID
		if _, err := s.contracts.UpdateLedgerRefs(ctx, ci.ID, &planRef, nil, caller.UserID, now, memberships); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
	}

	body, _ := json.Marshal(map[string]any{
		"contract_instance_id":   ci.ID,
		"story_id":               ci.StoryID,
		"status":                 contract.StatusClaimed,
		"amended":                amend,
		"action_claim_ledger_id": acRow.ID,
		"plan_ledger_id":         planRowID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_contract_claim").
		Str("ci_id", ci.ID).
		Bool("amended", amend).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleSessionWhoami returns the caller's registered session row, or
// a structured not-registered error. Used by tests + agents to verify
// the SessionStart hook populated the registry. When an
// OrchestratorGrantID is stamped on the row, the response also includes
// effective_verbs derived from the seeded role's allowed_mcp_verbs
// intersected with the agent's tool_ceiling.
func (s *Server) handleSessionWhoami(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := UserFrom(ctx)
	sessionID := req.GetString("session_id", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id is required"), nil
	}
	sess, err := s.sessions.Get(ctx, caller.UserID, sessionID)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": "session_not_registered"})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	payload := map[string]any{
		"user_id":       sess.UserID,
		"session_id":    sess.SessionID,
		"source":        sess.Source,
		"registered_at": sess.Registered,
		"last_seen_at":  sess.LastSeenAt,
	}
	if sess.OrchestratorGrantID != "" {
		payload["orchestrator_grant_id"] = sess.OrchestratorGrantID
		if verbs := s.resolveGrantEffectiveVerbs(ctx, sess.OrchestratorGrantID); len(verbs) > 0 {
			payload["effective_verbs"] = verbs
		}
	}
	body, _ := json.Marshal(payload)
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleSessionRegister lets the SessionStart hook and API-key flows
// populate the registry. In production this is driven by the harness;
// exposing it as a verb keeps tests honest and gives callers a way to
// re-register after an unexpected restart.
//
// When the RoleGrantStore + document store are both wired AND the
// system-scope orchestrator docs (role_orchestrator + agent_claude_orchestrator)
// resolve, Register also mints a role-grant on behalf of the session
// and stamps the returned grant_id on the session row. Failure in the
// grant-issuance path is non-fatal: the session row is still created,
// but the orchestrator_grant_id field stays empty. Rationale: the hook
// is a hot path and we prefer a session without a grant over a failed
// registration. Story_7d9c4b1b.
func (s *Server) handleSessionRegister(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := UserFrom(ctx)
	sessionID, err := req.RequireString("session_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	source := req.GetString("source", session.SourceSessionStart)
	now := time.Now().UTC()
	sess, err := s.sessions.Register(ctx, caller.UserID, sessionID, source, now)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if updated, ok := s.issueOrchestratorGrant(ctx, sess, caller.UserID, now); ok {
		sess = updated
	}
	body, _ := json.Marshal(sess)
	return mcpgo.NewToolResultText(string(body)), nil
}

// verifyCallerSession returns a structured error body string on
// failure. On success it touches the registry and returns nil.
func (s *Server) verifyCallerSession(ctx context.Context, userID, sessionID string, now time.Time) error {
	sess, err := s.sessions.Get(ctx, userID, sessionID)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": "session_not_registered"})
		return errors.New(string(body))
	}
	if session.IsStale(sess, now, resolveSessionStaleness()) {
		body, _ := json.Marshal(map[string]any{"error": "session_stale", "last_seen_at": sess.LastSeenAt.Format(time.RFC3339)})
		return errors.New(string(body))
	}
	if _, err := s.sessions.Touch(ctx, userID, sessionID, now); err != nil {
		return fmt.Errorf("session: touch: %w", err)
	}
	return nil
}

// findLatestActionClaim searches the ledger for the latest active
// kind:action-claim row scoped to the CI. Returns empty string when
// none exists.
func (s *Server) findLatestActionClaim(ctx context.Context, ci contract.ContractInstance, memberships []string) string {
	rows, err := s.ledger.List(ctx, ci.ProjectID, ledger.ListOptions{
		Type: ledger.TypeActionClaim,
		Tags: []string{"kind:action-claim"},
	}, memberships)
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.ContractID != nil && *r.ContractID == ci.ID && r.Status == ledger.StatusActive {
			return r.ID
		}
	}
	return ""
}

// marshalGateRejection is the shared renderer for *contract.GateRejection.
func marshalGateRejection(err error) string {
	var gr *contract.GateRejection
	if errors.As(err, &gr) {
		b, _ := json.Marshal(map[string]any{
			"error":    gr.Kind,
			"ci_id":    gr.CIID,
			"blocking": gr.Blocking,
			"current":  gr.Current,
			"message":  gr.Error(),
		})
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{"error": "claim_rejected", "message": err.Error()})
	return string(b)
}

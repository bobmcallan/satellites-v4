package permhook

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/session"
)

// Source identifies which path through the resolution chain produced
// the resolved permission_patterns.
const (
	SourceActiveCI       = "active_ci"
	SourceSessionDefault = "session_default"
	SourceNone           = "none"
)

// Decision identifies whether a tool call should be allowed.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// Resolver answers /hooks/enforce queries: given a session_id and a
// tool name, return whether the tool is allowed by the resolved
// permission_patterns. story_c08856b2.
//
// Resolution chain (first match wins):
//  1. The most recent active kind:action-claim ledger row scoped to
//     this session (via the user-id of the session's owner). The row's
//     Structured payload carries the allocated agent's permission_patterns
//     (story_b39b393f / story_cc55e093).
//  2. The most recent active kind:session-default-install ledger row
//     for this session_id (story_488b8223).
//  3. None — deny with reason="no_resolved_permissions".
type Resolver struct {
	Sessions session.Store
	Ledger   ledger.Store
	Docs     document.Store
}

// Result captures a resolution outcome.
type Result struct {
	Decision string   `json:"decision"`
	Reason   string   `json:"reason"`
	Source   string   `json:"source"`
	Patterns []string `json:"patterns,omitempty"`
	AgentID  string   `json:"agent_id,omitempty"`
}

// Resolve answers the enforce question. Story_c08856b2 AC1+AC2.
func (r *Resolver) Resolve(ctx context.Context, sessionID, tool string) Result {
	if r == nil || r.Sessions == nil || r.Ledger == nil {
		return deny("resolver_unwired", SourceNone)
	}
	if sessionID == "" {
		return deny("session_id_required", SourceNone)
	}
	sess, err := r.lookupSession(ctx, sessionID)
	if err != nil {
		return deny("session_not_registered", SourceNone)
	}

	// Path 1: active CI's action_claim row.
	patterns, agentID, ok := r.activeCIPatterns(ctx, sess)
	if ok {
		return decide(patterns, agentID, tool, SourceActiveCI)
	}

	// Path 2: session-default-install row.
	patterns, agentID, ok = r.sessionDefaultPatterns(ctx, sess)
	if ok {
		return decide(patterns, agentID, tool, SourceSessionDefault)
	}

	return deny("no_resolved_permissions", SourceNone)
}

// lookupSession finds a session row by session_id without knowing the
// owning user up front. The session.Store interface only exposes
// Get(userID, sessionID); this helper walks the registry via ListAll
// when available (the Surreal store + memory store both implement it
// per orchestrator_grant.go usage).
func (r *Resolver) lookupSession(ctx context.Context, sessionID string) (session.Session, error) {
	type allLister interface {
		ListAll(ctx context.Context) ([]session.Session, error)
	}
	if al, ok := r.Sessions.(allLister); ok {
		rows, err := al.ListAll(ctx)
		if err != nil {
			return session.Session{}, err
		}
		for _, row := range rows {
			if row.SessionID == sessionID {
				return row, nil
			}
		}
	}
	return session.Session{}, errors.New("session: not found")
}

// activeCIPatterns returns the allocated agent's permission_patterns
// for the most recent action-claim row scoped to the session's user.
// Returns (nil, "", false) when no claim is active.
func (r *Resolver) activeCIPatterns(ctx context.Context, sess session.Session) ([]string, string, bool) {
	rows, err := r.Ledger.List(ctx, "", ledger.ListOptions{
		Type: ledger.TypeActionClaim,
		Tags: []string{"kind:action-claim"},
	}, nil)
	if err != nil {
		return nil, "", false
	}
	for _, row := range rows {
		if row.Status != ledger.StatusActive {
			continue
		}
		if row.CreatedBy != sess.UserID {
			continue
		}
		var payload struct {
			AgentID          string   `json:"agent_id"`
			PermissionsClaim []string `json:"permissions_claim"`
		}
		if err := json.Unmarshal(row.Structured, &payload); err != nil {
			continue
		}
		if payload.AgentID == "" {
			continue
		}
		return payload.PermissionsClaim, payload.AgentID, true
	}
	return nil, "", false
}

// sessionDefaultPatterns returns the orchestrator agent's patterns
// from the most recent kind:session-default-install ledger row for
// this session.
func (r *Resolver) sessionDefaultPatterns(ctx context.Context, sess session.Session) ([]string, string, bool) {
	rows, err := r.Ledger.List(ctx, "", ledger.ListOptions{
		Type: ledger.TypeDecision,
		Tags: []string{"kind:session-default-install", "session:" + sess.SessionID},
	}, nil)
	if err != nil {
		return nil, "", false
	}
	for _, row := range rows {
		if row.Status != ledger.StatusActive {
			continue
		}
		var payload struct {
			AgentID            string   `json:"agent_id"`
			PermissionPatterns []string `json:"permission_patterns"`
		}
		if err := json.Unmarshal(row.Structured, &payload); err != nil {
			continue
		}
		if len(payload.PermissionPatterns) == 0 {
			continue
		}
		return payload.PermissionPatterns, payload.AgentID, true
	}
	return nil, "", false
}

func decide(patterns []string, agentID, tool, source string) Result {
	if Match(patterns, tool) {
		return Result{
			Decision: DecisionAllow,
			Reason:   "matched",
			Source:   source,
			Patterns: patterns,
			AgentID:  agentID,
		}
	}
	return Result{
		Decision: DecisionDeny,
		Reason:   "no_pattern_match",
		Source:   source,
		Patterns: patterns,
		AgentID:  agentID,
	}
}

func deny(reason, source string) Result {
	return Result{
		Decision: DecisionDeny,
		Reason:   reason,
		Source:   source,
	}
}

// LookupOrchestratorAgent walks the project > system scope chain and
// returns the highest-precedence orchestrator agent doc when one
// exists. story_c08856b2 AC3.
//
// Today the system seed is `agent_claude_orchestrator` (loaded by
// configseed from config/seed/system/agents/claude_orchestrator.md per
// sty_db196ff4). A project can author its own `orchestrator_role`
// agent to override the seeded baseline. The story AC named a
// workspace tier; the document substrate restricts workspace-scope to
// type=role only, so the workspace tier is collapsed into the project
// tier for now — a future story can lift the workspace agent restriction
// if a use case emerges.
func LookupOrchestratorAgent(ctx context.Context, docs document.Store, workspaceID, projectID string) (document.Document, bool) {
	if docs == nil {
		return document.Document{}, false
	}
	// Project scope wins.
	if projectID != "" {
		if d, err := docs.GetByName(ctx, projectID, "orchestrator_role", nil); err == nil && d.Type == document.TypeAgent {
			return d, true
		}
	}
	// System fallback (the configseed-loaded baseline).
	if d, err := docs.GetByName(ctx, "", "agent_claude_orchestrator", nil); err == nil && d.Type == document.TypeAgent {
		return d, true
	}
	return document.Document{}, false
}

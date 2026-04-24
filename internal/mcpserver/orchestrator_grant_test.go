package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/session"
)

// orchestratorTestServer mirrors the boot wiring a narrow subset of
// Server needs for the SessionStart grant path: docs store (seeded with
// role_orchestrator + agent_claude_orchestrator), grants store, session
// store, ledger store. cfg.GrantsEnforced stays false (6.5 flip).
func orchestratorTestServer(t *testing.T, seed bool) *Server {
	t.Helper()
	docs := document.NewMemoryStore()
	grants := rolegrant.NewMemoryStore(docs)
	sessions := session.NewMemoryStore()
	ldgr := ledger.NewMemoryStore()
	s := &Server{
		cfg:      &config.Config{GrantsEnforced: false},
		docs:     docs,
		grants:   grants,
		sessions: sessions,
		ledger:   ldgr,
	}
	if seed {
		ctx := context.Background()
		now := time.Now().UTC()
		role, err := docs.Create(ctx, document.Document{
			WorkspaceID: "wksp_sys",
			Type:        document.TypeRole,
			Name:        SeedRoleOrchestratorName,
			Scope:       document.ScopeSystem,
			Status:      document.StatusActive,
			Structured:  []byte(`{"allowed_mcp_verbs":["document_*","story_*"],"required_hooks":["SessionStart"]}`),
		}, now)
		require.NoError(t, err)
		_, err = docs.Create(ctx, document.Document{
			WorkspaceID: "wksp_sys",
			Type:        document.TypeAgent,
			Name:        SeedAgentOrchestratorName,
			Scope:       document.ScopeSystem,
			Status:      document.StatusActive,
			Structured:  []byte(`{"permitted_roles":["` + role.ID + `"],"tool_ceiling":["*"]}`),
		}, now)
		require.NoError(t, err)
	}
	return s
}

func callRegister(t *testing.T, s *Server, userID, sessionID string) map[string]any {
	t.Helper()
	ctx := context.WithValue(context.Background(), userKey, CallerIdentity{UserID: userID, Email: userID + "@example.com", Source: "apikey"})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"session_id": sessionID, "source": session.SourceSessionStart}
	res, err := s.handleSessionRegister(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "register error: %+v", res)
	text := res.Content[0].(mcpgo.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out
}

func TestSessionRegister_IssuesOrchestratorGrant_WhenSeedsPresent(t *testing.T) {
	t.Parallel()
	s := orchestratorTestServer(t, true)
	out := callRegister(t, s, "u1", "sess_aaa")
	grantID, _ := out["orchestrator_grant_id"].(string)
	require.NotEmpty(t, grantID, "session_register should stamp orchestrator_grant_id when seeds are present")

	// Grant row resolves + status=active + grantee matches session.
	grant, err := s.grants.GetByID(context.Background(), grantID, nil)
	require.NoError(t, err)
	assert.Equal(t, rolegrant.StatusActive, grant.Status)
	assert.Equal(t, rolegrant.GranteeSession, grant.GranteeKind)
	assert.Equal(t, "sess_aaa", grant.GranteeID)
}

func TestSessionRegister_SkipsGrant_WhenSeedsMissing(t *testing.T) {
	t.Parallel()
	s := orchestratorTestServer(t, false)
	out := callRegister(t, s, "u1", "sess_noseed")
	if gid, _ := out["orchestrator_grant_id"].(string); gid != "" {
		t.Fatalf("no seeds → no grant; got %q", gid)
	}

	// Session row still exists.
	sess, err := s.sessions.Get(context.Background(), "u1", "sess_noseed")
	require.NoError(t, err)
	assert.Equal(t, "sess_noseed", sess.SessionID)
	assert.Empty(t, sess.OrchestratorGrantID)
}

func TestSessionRegister_DistinctGrants_PerSession(t *testing.T) {
	t.Parallel()
	s := orchestratorTestServer(t, true)
	a := callRegister(t, s, "u1", "sess_alpha")
	b := callRegister(t, s, "u1", "sess_beta")
	ga, _ := a["orchestrator_grant_id"].(string)
	gb, _ := b["orchestrator_grant_id"].(string)
	require.NotEmpty(t, ga)
	require.NotEmpty(t, gb)
	assert.NotEqual(t, ga, gb, "each session should receive a distinct grant id")

	// Both grants coexist active in the store.
	active, err := s.grants.List(context.Background(), rolegrant.ListOptions{Status: rolegrant.StatusActive}, nil)
	require.NoError(t, err)
	assert.Len(t, active, 2)
}

func TestSessionWhoami_ReturnsOrchestratorGrant(t *testing.T) {
	t.Parallel()
	s := orchestratorTestServer(t, true)
	callRegister(t, s, "u1", "sess_whoami")

	ctx := context.WithValue(context.Background(), userKey, CallerIdentity{UserID: "u1", Email: "u1@example.com", Source: "apikey"})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"session_id": "sess_whoami"}
	res, err := s.handleSessionWhoami(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcpgo.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	grantID, _ := out["orchestrator_grant_id"].(string)
	assert.NotEmpty(t, grantID, "whoami should include orchestrator_grant_id")
	verbs, _ := out["effective_verbs"].([]any)
	assert.NotEmpty(t, verbs, "whoami should include effective_verbs when grant is live")
}

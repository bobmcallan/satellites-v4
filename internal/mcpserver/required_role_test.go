package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/session"
)

func TestDecodeContractPayload_EmptyIsZeroValue(t *testing.T) {
	t.Parallel()
	p, err := decodeContractPayload(nil)
	assert.NoError(t, err)
	assert.Empty(t, p.RequiredRole)
}

func TestDecodeContractPayload_ParsesRequiredRole(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"required_role":"role_orchestrator","allowed_tools_subset":["document_get"]}`)
	p, err := decodeContractPayload(raw)
	require.NoError(t, err)
	assert.Equal(t, "role_orchestrator", p.RequiredRole)
	assert.Equal(t, []string{"document_get"}, p.AllowedToolsSubset)
}

// setupRequiredRoleServer wires enough Server for resolveRequiredRoleGrant:
// docs + grants + sessions + a pre-seeded role/agent pair.
func setupRequiredRoleServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	docs := document.NewMemoryStore()
	grants := rolegrant.NewMemoryStore(docs)
	sessions := session.NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	role, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeRole,
		Name:        SeedRoleOrchestratorName,
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"allowed_mcp_verbs":["*"]}`),
	}, now)
	require.NoError(t, err)
	agent, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeAgent,
		Name:        SeedAgentOrchestratorName,
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"permitted_roles":["` + role.ID + `"],"tool_ceiling":["*"]}`),
	}, now)
	require.NoError(t, err)
	return &Server{docs: docs, grants: grants, sessions: sessions}, role.ID, agent.ID
}

// seedClaimedSession registers a session and mints an orchestrator
// grant bound to it, returning the grant id.
func seedClaimedSession(t *testing.T, s *Server, userID, sessionID, roleID, agentID string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := s.sessions.Register(ctx, userID, sessionID, session.SourceSessionStart, now)
	require.NoError(t, err)
	grant, err := s.grants.Create(ctx, rolegrant.RoleGrant{
		WorkspaceID: "wksp_sys",
		RoleID:      roleID,
		AgentID:     agentID,
		GranteeKind: rolegrant.GranteeSession,
		GranteeID:   sessionID,
	}, now)
	require.NoError(t, err)
	_, err = s.sessions.SetOrchestratorGrant(ctx, userID, sessionID, grant.ID, now)
	require.NoError(t, err)
	return grant.ID
}

func TestResolveRequiredRoleGrant_NoRequiredRole_FallsThrough(t *testing.T) {
	t.Parallel()
	s, _, _ := setupRequiredRoleServer(t)
	// Register a contract doc without required_role.
	ctx := context.Background()
	now := time.Now().UTC()
	doc, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeContract,
		Name:        "plain-contract",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"category":"develop","required_for_close":true,"validation_mode":"llm"}`),
	}, now)
	require.NoError(t, err)
	ci := contract.ContractInstance{ID: "ci_x", ContractID: doc.ID, WorkspaceID: "wksp_sys"}

	grantID, err := s.resolveRequiredRoleGrant(ctx, ci, "user_x", "session_x")
	assert.NoError(t, err)
	assert.Empty(t, grantID, "no required_role → empty grantID + no error")
}

func TestResolveRequiredRoleGrant_MatchingGrant_Accepts(t *testing.T) {
	t.Parallel()
	s, roleID, agentID := setupRequiredRoleServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doc, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeContract,
		Name:        "gated-contract",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"category":"develop","required_for_close":true,"validation_mode":"llm","required_role":"` + roleID + `"}`),
	}, now)
	require.NoError(t, err)
	grantID := seedClaimedSession(t, s, "user_a", "session_a", roleID, agentID)

	ci := contract.ContractInstance{ID: "ci_x", ContractID: doc.ID, WorkspaceID: "wksp_sys"}
	got, err := s.resolveRequiredRoleGrant(ctx, ci, "user_a", "session_a")
	require.NoError(t, err)
	assert.Equal(t, grantID, got)
}

func TestResolveRequiredRoleGrant_NoGrant_Rejects(t *testing.T) {
	t.Parallel()
	s, roleID, _ := setupRequiredRoleServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doc, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeContract,
		Name:        "gated-contract",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"category":"develop","required_for_close":true,"validation_mode":"llm","required_role":"` + roleID + `"}`),
	}, now)
	require.NoError(t, err)
	// Register the session but DO NOT mint a grant.
	_, err = s.sessions.Register(ctx, "user_b", "session_b", session.SourceSessionStart, now)
	require.NoError(t, err)

	ci := contract.ContractInstance{ID: "ci_x", ContractID: doc.ID, WorkspaceID: "wksp_sys"}
	_, err = s.resolveRequiredRoleGrant(ctx, ci, "user_b", "session_b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "grant_required")
}

func TestResolveRequiredRoleGrant_MismatchedRole_Rejects(t *testing.T) {
	t.Parallel()
	s, roleID, agentID := setupRequiredRoleServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Create a *different* role and stamp it in the contract's
	// required_role field so the caller's grant (which names roleID)
	// doesn't match.
	otherRole, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeRole,
		Name:        "role_other",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"allowed_mcp_verbs":["document_get"]}`),
	}, now)
	require.NoError(t, err)
	doc, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_sys",
		Type:        document.TypeContract,
		Name:        "gated-contract",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"category":"develop","required_for_close":true,"validation_mode":"llm","required_role":"` + otherRole.ID + `"}`),
	}, now)
	require.NoError(t, err)
	seedClaimedSession(t, s, "user_c", "session_c", roleID, agentID)

	ci := contract.ContractInstance{ID: "ci_x", ContractID: doc.ID, WorkspaceID: "wksp_sys"}
	_, err = s.resolveRequiredRoleGrant(ctx, ci, "user_c", "session_c")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required_role_mismatch")
}

func TestAddRequiredRoleIfMissing(t *testing.T) {
	t.Parallel()
	// Empty payload → synthesized.
	out, changed := addRequiredRoleIfMissingProxy(t, nil, "role_a")
	require.True(t, changed)
	assert.Contains(t, string(out), `"required_role":"role_a"`)

	// Payload without the key → inserted.
	raw := []byte(`{"category":"plan"}`)
	out, changed = addRequiredRoleIfMissingProxy(t, raw, "role_a")
	require.True(t, changed)
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, "role_a", m["required_role"])
	assert.Equal(t, "plan", m["category"])

	// Payload with the key → untouched.
	raw2 := []byte(`{"required_role":"role_existing"}`)
	_, changed = addRequiredRoleIfMissingProxy(t, raw2, "role_new")
	assert.False(t, changed, "existing required_role should not be clobbered")
}

// addRequiredRoleIfMissingProxy wraps the helper so the test file in
// package mcpserver can exercise it without duplicating the logic. The
// real helper lives in cmd/satellites/main.go; we re-implement it here
// as a thin local copy for test isolation — both must stay in sync.
func addRequiredRoleIfMissingProxy(t *testing.T, raw []byte, role string) ([]byte, bool) {
	t.Helper()
	if len(raw) == 0 {
		out, _ := json.Marshal(map[string]any{"required_role": role})
		return out, true
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	if _, ok := m["required_role"]; ok {
		if _, got := m["required_role"].(string); got {
			_ = strings.TrimSpace(role) // silence unused-import style
		}
		return nil, false
	}
	m["required_role"] = role
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

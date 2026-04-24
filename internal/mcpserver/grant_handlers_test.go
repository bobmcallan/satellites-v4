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
)

// grantTestServer wires a minimal Server with docs + ledger + grants
// stores for handler-level unit tests. No HTTP plumbing.
func grantTestServer(t *testing.T) *Server {
	t.Helper()
	docs := document.NewMemoryStore()
	grants := rolegrant.NewMemoryStore(docs)
	ldgr := ledger.NewMemoryStore()
	return &Server{
		cfg:    &config.Config{GrantsEnforced: false},
		docs:   docs,
		grants: grants,
		ledger: ldgr,
	}
}

// seedAgentRole creates an agent + role document with the given payloads
// and returns their ids.
func seedAgentRole(t *testing.T, s *Server, agentPayload, rolePayload string) (agentID, roleID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	agent, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeAgent,
		Name:        "agent_test",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(agentPayload),
	}, now)
	require.NoError(t, err)
	role, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeRole,
		Name:        "role_test",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(rolePayload),
	}, now)
	require.NoError(t, err)
	return agent.ID, role.ID
}

// callGrantTool invokes handler on req synchronously and returns the
// structured payload (decoded from the first text content) or the
// error-string result.
func callGrantTool(t *testing.T, handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error), args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	return res
}

func payloadFromResult(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	require.Greater(t, len(res.Content), 0, "result has no content")
	text, ok := res.Content[0].(mcpgo.TextContent)
	require.True(t, ok, "content[0] is not TextContent")
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &out))
	return out
}

func TestAgentRoleClaim_HappyPath(t *testing.T) {
	t.Parallel()
	s := grantTestServer(t)
	agentID, roleID := seedAgentRole(t,
		s,
		`{"permitted_roles":["placeholder"],"tool_ceiling":["document_*"]}`,
		`{"allowed_mcp_verbs":["document_get","document_list"]}`,
	)
	// Patch the agent doc so permitted_roles contains the real role id
	// (test-time shortcut: re-create with the resolved id).
	ctx := context.Background()
	payload := []byte(`{"permitted_roles":["` + roleID + `"],"tool_ceiling":["document_*"]}`)
	_, err := s.docs.Update(ctx, agentID, document.UpdateFields{Structured: &payload}, "test", time.Now().UTC(), nil)
	require.NoError(t, err)

	res := callGrantTool(t, s.handleAgentRoleClaim, map[string]any{
		"workspace_id": "wksp_a",
		"role_id":      roleID,
		"agent_id":     agentID,
		"grantee_kind": "session",
		"grantee_id":   "session_1",
	})
	require.False(t, res.IsError, "claim rejected: %+v", res)
	out := payloadFromResult(t, res)
	assert.NotEmpty(t, out["grant_id"], "grant_id missing")
	assert.Equal(t, "active", out["status"])
	effective, _ := out["effective_verbs"].([]any)
	assert.ElementsMatch(t, []any{"document_get", "document_list"}, effective, "effective verbs = intersection(ceiling, allowed)")
}

func TestAgentRoleClaim_RoleNotInPermittedRoles(t *testing.T) {
	t.Parallel()
	s := grantTestServer(t)
	agentID, _ := seedAgentRole(t,
		s,
		`{"permitted_roles":["role_other"],"tool_ceiling":["*"]}`,
		`{"allowed_mcp_verbs":["document_get"]}`,
	)
	// Use a different role id that exists but isn't in permitted_roles.
	ctx := context.Background()
	now := time.Now().UTC()
	otherRole, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeRole,
		Name:        "role_other_name",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"allowed_mcp_verbs":["document_get"]}`),
	}, now)
	require.NoError(t, err)

	res := callGrantTool(t, s.handleAgentRoleClaim, map[string]any{
		"workspace_id": "wksp_a",
		"role_id":      otherRole.ID,
		"agent_id":     agentID,
		"grantee_kind": "session",
		"grantee_id":   "session_1",
	})
	assert.True(t, res.IsError, "claim should reject when role not in permitted_roles")
}

func TestAgentRoleClaim_CeilingDoesNotCoverRole(t *testing.T) {
	t.Parallel()
	s := grantTestServer(t)
	agentID, roleID := seedAgentRole(t,
		s,
		`{"permitted_roles":["__to_patch__"],"tool_ceiling":["document_*"]}`,
		`{"allowed_mcp_verbs":["story_create"]}`,
	)
	ctx := context.Background()
	payload := []byte(`{"permitted_roles":["` + roleID + `"],"tool_ceiling":["document_*"]}`)
	_, err := s.docs.Update(ctx, agentID, document.UpdateFields{Structured: &payload}, "test", time.Now().UTC(), nil)
	require.NoError(t, err)

	res := callGrantTool(t, s.handleAgentRoleClaim, map[string]any{
		"workspace_id": "wksp_a",
		"role_id":      roleID,
		"agent_id":     agentID,
		"grantee_kind": "session",
		"grantee_id":   "session_1",
	})
	assert.True(t, res.IsError, "claim should reject when tool_ceiling does not cover role verbs")
}

func TestAgentRoleRelease_Idempotent(t *testing.T) {
	t.Parallel()
	s := grantTestServer(t)
	agentID, roleID := seedAgentRole(t,
		s,
		`{"permitted_roles":["__to_patch__"],"tool_ceiling":["*"]}`,
		`{"allowed_mcp_verbs":["document_get"]}`,
	)
	ctx := context.Background()
	payload := []byte(`{"permitted_roles":["` + roleID + `"],"tool_ceiling":["*"]}`)
	_, err := s.docs.Update(ctx, agentID, document.UpdateFields{Structured: &payload}, "test", time.Now().UTC(), nil)
	require.NoError(t, err)

	claimed := callGrantTool(t, s.handleAgentRoleClaim, map[string]any{
		"workspace_id": "wksp_a",
		"role_id":      roleID,
		"agent_id":     agentID,
		"grantee_kind": "session",
		"grantee_id":   "session_1",
	})
	claimedPayload := payloadFromResult(t, claimed)
	grantID, _ := claimedPayload["grant_id"].(string)
	require.NotEmpty(t, grantID)

	// First release flips status → released.
	released := callGrantTool(t, s.handleAgentRoleRelease, map[string]any{
		"grant_id": grantID,
		"reason":   "task_close",
	})
	require.False(t, released.IsError)
	relPayload := payloadFromResult(t, released)
	assert.Equal(t, "released", relPayload["status"])
	assert.Equal(t, false, relPayload["redundant"])

	// Second release is a no-op that still succeeds.
	again := callGrantTool(t, s.handleAgentRoleRelease, map[string]any{
		"grant_id": grantID,
		"reason":   "redundant",
	})
	require.False(t, again.IsError)
	againPayload := payloadFromResult(t, again)
	assert.Equal(t, "released", againPayload["status"])
	assert.Equal(t, true, againPayload["redundant"])
}

func TestAgentRoleList_FilterByGrantee(t *testing.T) {
	t.Parallel()
	s := grantTestServer(t)
	agentID, roleID := seedAgentRole(t,
		s,
		`{"permitted_roles":["__to_patch__"],"tool_ceiling":["*"]}`,
		`{"allowed_mcp_verbs":["document_get"]}`,
	)
	ctx := context.Background()
	payload := []byte(`{"permitted_roles":["` + roleID + `"],"tool_ceiling":["*"]}`)
	_, err := s.docs.Update(ctx, agentID, document.UpdateFields{Structured: &payload}, "test", time.Now().UTC(), nil)
	require.NoError(t, err)

	for _, grantee := range []string{"session_a", "session_b"} {
		callGrantTool(t, s.handleAgentRoleClaim, map[string]any{
			"workspace_id": "wksp_a",
			"role_id":      roleID,
			"agent_id":     agentID,
			"grantee_kind": "session",
			"grantee_id":   grantee,
		})
	}

	res := callGrantTool(t, s.handleAgentRoleList, map[string]any{
		"grantee_id": "session_a",
	})
	require.False(t, res.IsError)
	require.Greater(t, len(res.Content), 0)
	text := res.Content[0].(mcpgo.TextContent).Text
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &rows))
	require.Len(t, rows, 1, "list filtered by grantee_id=session_a should return exactly one row")
	assert.Equal(t, "session_a", rows[0]["grantee_id"])
}

func TestCeilingCovers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ceiling []string
		want    []string
		expect  bool
	}{
		{"wildcard covers all", []string{"*"}, []string{"document_get", "story_create"}, true},
		{"prefix covers all prefixed", []string{"document_*"}, []string{"document_get", "document_list"}, true},
		{"prefix does not cover other namespace", []string{"document_*"}, []string{"document_get", "story_create"}, false},
		{"exact matches only itself", []string{"document_get"}, []string{"document_get"}, true},
		{"exact does not cover prefix", []string{"document_get"}, []string{"document_list"}, false},
		{"empty ceiling covers nothing", []string{}, []string{"document_get"}, false},
		{"empty want trivially true", []string{"document_get"}, []string{}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ceilingCovers(tc.ceiling, tc.want)
			assert.Equal(t, tc.expect, got)
		})
	}
}

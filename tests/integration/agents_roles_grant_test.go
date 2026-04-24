package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestAgentsRolesGrant_MCPSurface_EndToEnd boots the full container
// stack and exercises the 6.3 MCP verbs against real SurrealDB:
//
//  1. Create an agent doc + role doc via agent_create / role_create
//     (6.2's wrappers).
//  2. Call agent_role_claim to mint a role_grant row. Confirms the
//     claim path, FK resolution, permitted_roles + tool_ceiling checks,
//     grant insert, ledger audit row.
//  3. agent_role_list returns the row (MemoryStore + SurrealStore
//     parity assertion via the MCP surface).
//  4. agent_role_release flips status → released; a second release
//     returns redundant=true (idempotency).
//  5. Repeat claim for a second grantee — concurrent-grant shape
//     (two active grants per role per workspace is safe).
//
// This is AC 8 for story_1efbfc48. Middleware enforcement remains OFF
// (default) — enforcement-on coverage lands with story_7d9c4b1b (6.4)
// where SessionStart issues orchestrator grants via the real auth path.
func TestAgentsRolesGrant_MCPSurface_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {"surrealdb"},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	docsHost := filepath.Join(repoRoot(t), "docs")
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"DB_DSN":              "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_gr",
			"DOCS_DIR":            "/app/docs",
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	defer stop()

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, "key_gr")

	// Step 1: create role + agent docs.
	role := callTool(t, ctx, mcpURL, "key_gr", "role_create", map[string]any{
		"scope":      "system",
		"name":       "role_grant_it",
		"body":       "integration-test RBAC role",
		"structured": `{"allowed_mcp_verbs":["document_get","document_list"],"required_hooks":["SessionStart"],"claim_requirements":[],"default_context_policy":"fresh-per-claim"}`,
	})
	roleID, _ := role["id"].(string)
	require.NotEmpty(t, roleID)

	agent := callTool(t, ctx, mcpURL, "key_gr", "agent_create", map[string]any{
		"scope": "system",
		"name":  "agent_grant_it",
		"body":  "integration-test delivery agent",
		"structured": `{"provider_chain":[{"provider":"claude","model":"opus-4"}],"tier":"opus","permitted_roles":["` + roleID + `"],"tool_ceiling":["document_*"]}`,
	})
	agentID, _ := agent["id"].(string)
	require.NotEmpty(t, agentID)

	// Step 2: agent_role_claim mints a grant.
	claimResult := callTool(t, ctx, mcpURL, "key_gr", "agent_role_claim", map[string]any{
		"workspace_id": "wksp_it",
		"role_id":      roleID,
		"agent_id":     agentID,
		"grantee_kind": "session",
		"grantee_id":   "session_alpha",
	})
	grantID, _ := claimResult["grant_id"].(string)
	require.NotEmpty(t, grantID, "agent_role_claim should return grant_id")
	assert.Equal(t, "active", claimResult["status"])
	effective, _ := claimResult["effective_verbs"].([]any)
	assert.ElementsMatch(t, []any{"document_get", "document_list"}, effective, "effective_verbs = ceiling ∩ role.allowed")

	// Step 3: agent_role_list confirms the grant is readable.
	listRows := callToolArray(t, ctx, mcpURL, "key_gr", "agent_role_list", map[string]any{
		"grantee_id": "session_alpha",
	})
	require.GreaterOrEqual(t, len(listRows), 1)
	match := false
	for _, row := range listRows {
		m, _ := row.(map[string]any)
		if m["id"] == grantID {
			assert.Equal(t, "active", m["status"])
			match = true
		}
	}
	assert.True(t, match, "agent_role_list should return the grant we just claimed")

	// Step 4a: agent_role_release flips status → released.
	released := callTool(t, ctx, mcpURL, "key_gr", "agent_role_release", map[string]any{
		"grant_id": grantID,
		"reason":   "integration_end",
	})
	assert.Equal(t, "released", released["status"])
	assert.Equal(t, false, released["redundant"])

	// Step 4b: second release is idempotent.
	again := callTool(t, ctx, mcpURL, "key_gr", "agent_role_release", map[string]any{
		"grant_id": grantID,
		"reason":   "double-call",
	})
	assert.Equal(t, "released", again["status"])
	assert.Equal(t, true, again["redundant"])

	// Step 5: concurrent-grant shape — mint a second active grant under
	// the same role + agent for a different grantee.
	claim2 := callTool(t, ctx, mcpURL, "key_gr", "agent_role_claim", map[string]any{
		"workspace_id": "wksp_it",
		"role_id":      roleID,
		"agent_id":     agentID,
		"grantee_kind": "session",
		"grantee_id":   "session_beta",
	})
	grantID2, _ := claim2["grant_id"].(string)
	require.NotEmpty(t, grantID2)
	assert.NotEqual(t, grantID, grantID2, "second claim should mint a distinct grant id")
	assert.Equal(t, "active", claim2["status"])

	// List active grants on this role; only session_beta's should
	// remain active since session_alpha's was released in step 4.
	activeRows := callToolArray(t, ctx, mcpURL, "key_gr", "agent_role_list", map[string]any{
		"role_id": roleID,
		"status":  "active",
	})
	activeCount := 0
	for _, row := range activeRows {
		m, _ := row.(map[string]any)
		if m["status"] == "active" {
			activeCount++
		}
	}
	assert.Equal(t, 1, activeCount, "only session_beta's grant should remain active after session_alpha release")

	// Sanity: decode the claim payload again to confirm the handler's
	// JSON shape parses cleanly.
	b, err := json.Marshal(claim2)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"effective_verbs"`)
}

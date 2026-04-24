package integration

import (
	"context"
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

// TestAgentsRolesOrchestrator_SessionStart_IssuesGrant boots the full
// container stack, seeds role_orchestrator + agent_claude_orchestrator
// at server bootstrap (inline in cmd/satellites/main.go), then exercises
// the SessionStart grant issuance path end-to-end:
//
//  1. session_register → server mints a role-grant, stamps
//     orchestrator_grant_id on the session row.
//  2. session_whoami returns the grant id + effective_verbs.
//  3. Two distinct session_ids receive distinct grants coexisting
//     active — the concurrent-grant shape that resolves v3's
//     multi-session session_id collision.
//
// GrantsEnforced stays off (default); 6.5 handles the flip + enforce
// hook re-enable.
func TestAgentsRolesOrchestrator_SessionStart_IssuesGrant(t *testing.T) {
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
			"SATELLITES_API_KEYS": "key_oc",
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
	rpcInit(t, ctx, mcpURL, "key_oc")

	// Step 1: register session α.
	reg1 := callTool(t, ctx, mcpURL, "key_oc", "session_register", map[string]any{
		"session_id": "sess_alpha",
	})
	grant1, _ := reg1["orchestrator_grant_id"].(string)
	require.NotEmpty(t, grant1, "SessionStart should mint a grant when seed docs are live")

	// Step 2: whoami returns grant metadata + effective_verbs.
	whoami1 := callTool(t, ctx, mcpURL, "key_oc", "session_whoami", map[string]any{
		"session_id": "sess_alpha",
	})
	assert.Equal(t, grant1, whoami1["orchestrator_grant_id"])
	verbs, _ := whoami1["effective_verbs"].([]any)
	assert.NotEmpty(t, verbs, "session_whoami should surface effective_verbs when grant is live")

	// Step 3: register session β; confirm distinct grant + both coexist.
	reg2 := callTool(t, ctx, mcpURL, "key_oc", "session_register", map[string]any{
		"session_id": "sess_beta",
	})
	grant2, _ := reg2["orchestrator_grant_id"].(string)
	require.NotEmpty(t, grant2)
	assert.NotEqual(t, grant1, grant2, "distinct sessions should receive distinct grants (v3 collision fix)")

	// agent_role_list filtered by status=active should return both.
	active := callToolArray(t, ctx, mcpURL, "key_oc", "agent_role_list", map[string]any{
		"status": "active",
	})
	activeCount := 0
	for _, row := range active {
		m, _ := row.(map[string]any)
		if m["status"] == "active" {
			activeCount++
		}
	}
	assert.GreaterOrEqual(t, activeCount, 2, "both alpha + beta grants should be active")

	// The successful grant issuance on sess_alpha is itself the
	// strongest evidence the boot-time seed ran — issueOrchestratorGrant
	// would have short-circuited without minting a grant if either doc
	// were absent. No separate document_get sanity probe needed (and
	// document_get defaults project_id to the caller's first owned
	// project, which does not match the system-scope seed).
}

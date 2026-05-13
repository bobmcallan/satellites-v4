package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestAgentCompose_FullStack drives story_b19260d8 against a real
// testcontainers satellites server:
//
//  1. Boot Surreal + satellites server.
//  2. Create a project + a system-scope contract + a workspace skill bound
//     to that contract.
//  3. Compose an ephemeral agent for a fresh story; assert the response
//     carries the agent doc + a kind:agent-compose ledger row id.
//  4. Call agent_ephemeral_summary; assert the count + skill-set
//     grouping match the seed.
func TestAgentCompose_FullStack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
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
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	docsHost := filepath.Join(repoRoot(t), "docs")
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"SATELLITES_DB_DSN":   "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_agentcompose",
			"SATELLITES_DOCS_DIR": "/app/docs",
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
	rpcInit(t, ctx, mcpURL, "key_agentcompose")

	project := callTool(t, ctx, mcpURL, "key_agentcompose", "project_create", map[string]any{
		"name": "agent-compose-project",
	})
	projectID, _ := project["id"].(string)

	// Seed a project-scope contract document. The contract_add wrapper
	// (internal/mcpserver/wrappers.go) requires a structured payload with
	// category + required_for_close + validation_mode.
	contractDoc := callTool(t, ctx, mcpURL, "key_agentcompose", "contract_add", map[string]any{
		"scope":      "project",
		"project_id": projectID,
		"name":       "develop",
		"body":       "develop contract for agent_compose test",
		"structured": `{"category":"develop","required_for_close":true,"validation_mode":"agent"}`,
	})
	contractID, _ := contractDoc["id"].(string)

	// Seed a project-scope skill bound to the contract.
	skillDoc := callTool(t, ctx, mcpURL, "key_agentcompose", "skill_add", map[string]any{
		"scope":            "project",
		"project_id":       projectID,
		"name":             "golang-testing",
		"body":             "skill body",
		"contract_binding": contractID,
	})
	skillID, _ := skillDoc["id"].(string)

	// Create a story scoped to the project. story_add was removed
	// from MCP in sty_4db0e025 C1+B2 (operator authoring per
	// sty_3dc39a5c "Removed from MCP"); route via /api/v1.
	storyResp := callAPIv1(t, ctx, baseURL, "key_agentcompose", "story_add", map[string]any{
		"project_id": projectID,
		"title":      "story for agent compose",
	})
	storyID, _ := storyResp["id"].(string)

	// Compose an ephemeral agent.
	composeResp := callTool(t, ctx, mcpURL, "key_agentcompose", "agent_compose", map[string]any{
		"name":                "story_X_developer",
		"project_id":          projectID,
		"skill_refs":          []string{skillID},
		"permission_patterns": []string{"Edit:internal/portal/**", "Bash:go_test"},
		"ephemeral":           true,
		"story_id":            storyID,
		"reason":              "compose for full-stack test",
	})
	if id, _ := composeResp["agent_compose_ledger_id"].(string); id == "" {
		t.Fatalf("agent_compose_ledger_id missing in response: %+v", composeResp)
	}
	agent, _ := composeResp["agent"].(map[string]any)
	if agentName, _ := agent["name"].(string); agentName != "story_X_developer" {
		t.Errorf("agent.name = %q, want story_X_developer", agentName)
	}

	// Verify the kind:agent-compose row is queryable via ledger_list.
	rows := callToolArray(t, ctx, mcpURL, "key_agentcompose", "ledger_list", map[string]any{
		"project_id": projectID,
		"type":       "agent-compose",
	})
	if len(rows) == 0 {
		t.Fatalf("ledger_list type=agent-compose returned no rows")
	}
	row, _ := rows[0].(map[string]any)
	if got, _ := row["type"].(string); got != "agent-compose" {
		t.Errorf("ledger row type = %q, want agent-compose", got)
	}

	// agent_ephemeral_summary returns the count + skill-set grouping.
	summary := callTool(t, ctx, mcpURL, "key_agentcompose", "agent_ephemeral_summary", map[string]any{
		"project_id": projectID,
	})
	count, _ := summary["ephemeral_agent_count"].(float64)
	if int(count) != 1 {
		t.Errorf("ephemeral_agent_count = %v, want 1", count)
	}
	groups, _ := summary["by_skill_set"].([]any)
	if len(groups) != 1 {
		t.Errorf("by_skill_set groups = %d, want 1", len(groups))
	}
}

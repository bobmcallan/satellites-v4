package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestDocumentWrappers_Registered_AndPrincipleHappyPath asserts the 24
// wrapper verbs are registered (4 kinds × 6 ops) and exercises a
// principle_add → principle_list happy path.
func TestDocumentWrappers_Registered_AndPrincipleHappyPath(t *testing.T) {
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
			"SATELLITES_API_KEYS": "key_wrap",
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
	rpcInit(t, ctx, mcpURL, "key_wrap")

	listResp := rpcCall(t, ctx, mcpURL, "key_wrap", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	want := map[string]bool{}
	for _, kind := range []string{"principle", "contract", "skill", "reviewer"} {
		for _, op := range []string{"_add", "_get", "_list", "_update", "_delete", "_search"} {
			want[kind+op] = false
		}
	}
	if result, _ := listResp["result"].(map[string]any); result != nil {
		if tools, _ := result["tools"].([]any); tools != nil {
			for _, raw := range tools {
				if tool, ok := raw.(map[string]any); ok {
					if name, _ := tool["name"].(string); name != "" {
						if _, tracked := want[name]; tracked {
							want[name] = true
						}
					}
				}
			}
		}
	}
	missing := 0
	for k, ok := range want {
		if !ok {
			t.Errorf("tools/list missing wrapper %q", k)
			missing++
		}
	}
	if missing == 0 {
		t.Logf("all 24 wrapper verbs present")
	}

	// principle_add — happy path with required scope + tags.
	created := callTool(t, ctx, mcpURL, "key_wrap", "principle_add", map[string]any{
		"scope": "system",
		"name":  "wrapper-principle",
		"body":  "wrapper test principle",
		"tags":  []any{"v4", "wrapper"},
	})
	if got, _ := created["type"].(string); got != "principle" {
		t.Errorf("principle_add returned type=%q, want principle", got)
	}

	// principle_add caller-supplied type rejected.
	bogus := callToolRaw(t, ctx, mcpURL, "key_wrap", "principle_add", map[string]any{
		"type":  "artifact",
		"scope": "system",
		"name":  "tampered",
		"tags":  []any{"v4"},
	})
	if !isToolError(bogus) {
		t.Errorf("principle_add with caller type should isError; got %+v", bogus)
	}

	// skill_add without contract_binding rejected.
	skillBad := callToolRaw(t, ctx, mcpURL, "key_wrap", "skill_add", map[string]any{
		"scope": "system",
		"name":  "skill-bad",
	})
	if !isToolError(skillBad) {
		t.Errorf("skill_add without contract_binding should isError; got %+v", skillBad)
	}

	// principle_list returns the seeded principle and pins type=principle
	// even when the caller tries to escape with type=contract.
	rows := callToolArray(t, ctx, mcpURL, "key_wrap", "principle_list", map[string]any{
		"type": "contract",
	})
	if len(rows) == 0 {
		t.Errorf("principle_list returned no rows; expected the seeded principle")
	}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		if m["type"] != "principle" {
			t.Errorf("principle_list returned non-principle row: %+v", m)
		}
	}

	// sty_690b1653: the system-tier lifecycle contracts (plan,
	// story_close) must be reachable by name via contract_get and each
	// must declare a `## Review policy` heading. The api-key caller has
	// no wksp_5b3257d1 membership, so workspace-tier contracts (develop,
	// push, merge_to_main, review) fall through to the system tier and
	// return ErrNotFound on this surface — their loader-side coverage
	// lives in TestRunWorkspace_RealSeedShipsFourLifecycleContracts.
	for _, name := range []string{"plan", "story_close"} {
		got := callTool(t, ctx, mcpURL, "key_wrap", "contract_get", map[string]any{"name": name})
		if scope, _ := got["scope"].(string); scope != "system" {
			t.Errorf("contract_get(name=%q) scope=%q, want system", name, scope)
		}
		body, _ := got["body"].(string)
		if !strings.Contains(body, "## Review policy") {
			t.Errorf("contract_get(name=%q) body missing ## Review policy heading; body=%q", name, body)
		}
	}
}

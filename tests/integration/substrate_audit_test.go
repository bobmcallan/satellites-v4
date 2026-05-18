// substrate_audit_test.go — sty_2f0db922 end-to-end coverage.
//
// Boots a real satellites-server testcontainer wired to a Surreal
// instance + bind-mounted docs/, drives the substrate_audit verb via
// both transports (/api/v1/substrate/audit and the /mcp tools/call
// envelope), and asserts the verb mints a kind=work
// action=contract:substrate_audit task naming the system-scope
// substrate_auditor agent.
//
// Per pr_substrate_model the audit rubric lives in the contract +
// agent markdown — the dispatched agent runs the rubric and writes
// the kind:audit-report ledger row. This test covers the substrate-
// owned mint surface; the rubric-output assertion (AC6 a-e verdict
// mapping) lives in the live-pprod dogfood evidence (AC9).
//
// Sub-tests cover:
//   - AC1: contract_get(name=substrate_audit) returns the seeded contract.
//   - AC2: agent_get(name=substrate_auditor) returns the seeded agent.
//   - AC3: tools/list advertises substrate_audit; /mcp envelope mints a task.
//   - AC4: /api/v1/substrate/audit mints a task with the canonical shape.
//   - AC5: the audit-report ledger schema is consumable end-to-end —
//          a synthesised audit-report row tagged verdict:audit:pass
//          is reachable via ledger_list filtering on the tag.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestSubstrateAudit is the AC1 + AC2 + AC3 + AC4 + AC5 evidence
// anchor.
func TestSubstrateAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration harness requires a posix shell + docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping testcontainers-backed test")
	}
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	const bearer = "key_substrate_audit_test"
	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv: map[string]string{"SATELLITES_API_KEYS": bearer},
	})
	defer stack.Stop()
	baseURL := stack.BaseURL

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, bearer)

	t.Run("contract_seeded", func(t *testing.T) {
		// AC1: configseed plants config/seed/system/contracts/substrate_audit.md
		// at boot. contract_get(name=...) resolves it.
		contract := callTool(t, ctx, mcpURL, bearer, "contract_get", map[string]any{
			"name": "substrate_audit",
		})
		if got, _ := contract["name"].(string); got != "substrate_audit" {
			t.Errorf("contract_get(substrate_audit) name=%q, want substrate_audit (raw=%+v)", got, contract)
		}
	})

	t.Run("agent_seeded", func(t *testing.T) {
		// AC2: configseed plants config/seed/system/agents/substrate_auditor.md
		// at boot. agent_get(name=...) resolves it. The structured
		// payload declares delivers: ["contract:substrate_audit"] so
		// the TaskAdd capability check passes when the audit verb mints
		// the task.
		agent := callTool(t, ctx, mcpURL, bearer, "agent_get", map[string]any{
			"name": "substrate_auditor",
		})
		if got, _ := agent["name"].(string); got != "substrate_auditor" {
			t.Errorf("agent_get(substrate_auditor) name=%q, want substrate_auditor (raw=%+v)", got, agent)
		}
		if scope, _ := agent["scope"].(string); scope != "system" {
			t.Errorf("agent_get(substrate_auditor) scope=%q, want system", scope)
		}
	})

	t.Run("api_v1_mints_task", func(t *testing.T) {
		// AC4 (CLI verb path uses /api/v1/substrate/audit per
		// cliremote.ToolNameToPath): /api/v1/substrate/audit returns
		// the canonical {task_id, story_id, agent_id, scope} envelope.
		body, _ := json.Marshal(map[string]any{})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/substrate/audit", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
		}
		var got struct {
			TaskID  string `json:"task_id"`
			StoryID string `json:"story_id"`
			AgentID string `json:"agent_id"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, string(raw))
		}
		if got.TaskID == "" {
			t.Errorf("task_id empty (raw=%s)", string(raw))
		}
		if got.StoryID == "" {
			t.Errorf("story_id empty (raw=%s)", string(raw))
		}
		if got.AgentID == "" {
			t.Errorf("agent_id empty (raw=%s)", string(raw))
		}
		if got.Scope == "" {
			t.Errorf("scope empty (raw=%s)", string(raw))
		}

		// Round-trip the task to confirm the canonical shape.
		tk := callTool(t, ctx, mcpURL, bearer, "task_get", map[string]any{
			"id": got.TaskID,
		})
		if kind, _ := tk["kind"].(string); kind != "work" {
			t.Errorf("task.kind=%q, want work (raw=%+v)", kind, tk)
		}
		if action, _ := tk["action"].(string); action != "contract:substrate_audit" {
			t.Errorf("task.action=%q, want contract:substrate_audit (raw=%+v)", action, tk)
		}
		if status, _ := tk["status"].(string); status != "published" {
			t.Errorf("task.status=%q, want published (raw=%+v)", status, tk)
		}
		if agentID, _ := tk["agent_id"].(string); agentID != got.AgentID {
			t.Errorf("task.agent_id=%q, want %q (raw=%+v)", agentID, got.AgentID, tk)
		}
	})

	t.Run("mcp_envelope_parity", func(t *testing.T) {
		// AC3: the same verb over the /mcp tools/call envelope
		// returns the same canonical shape (the typed surface is
		// shared per pr_mcp_cli_shared_path so parity is structural).
		out := callTool(t, ctx, mcpURL, bearer, "substrate_audit", map[string]any{})
		taskID, _ := out["task_id"].(string)
		if taskID == "" {
			t.Errorf("mcp substrate_audit returned empty task_id (raw=%+v)", out)
		}
		agentID, _ := out["agent_id"].(string)
		if agentID == "" {
			t.Errorf("mcp substrate_audit returned empty agent_id (raw=%+v)", out)
		}

		// tools/list MUST advertise the verb name.
		listResp := rpcCall(t, ctx, mcpURL, bearer, map[string]any{
			"jsonrpc": "2.0", "id": 4242, "method": "tools/list",
		})
		found := false
		if result, _ := listResp["result"].(map[string]any); result != nil {
			if tools, _ := result["tools"].([]any); tools != nil {
				for _, raw := range tools {
					if tool, ok := raw.(map[string]any); ok {
						if name, _ := tool["name"].(string); name == "substrate_audit" {
							found = true
							break
						}
					}
				}
			}
		}
		if !found {
			t.Errorf("tools/list did not advertise substrate_audit")
		}
	})

	t.Run("audit_report_schema_consumable", func(t *testing.T) {
		// AC5: the audit-report ledger schema is consumable end-to-end.
		// Mint an audit task via /api/v1, then synthesise the report
		// row the dispatched agent would write (the rubric output is
		// the agent's reasoning surface; this test covers the schema's
		// downstream consumability — verdict-tag filtering, structured
		// content shape). AC6 a-e verdict mapping is exercised in the
		// live-pprod dogfood (AC9) where the agent actually runs the
		// rubric.
		body, _ := json.Marshal(map[string]any{})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/substrate/audit", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var mint struct {
			TaskID  string `json:"task_id"`
			StoryID string `json:"story_id"`
			AgentID string `json:"agent_id"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(raw, &mint); err != nil {
			t.Fatalf("decode mint: %v\n%s", err, string(raw))
		}
		// task_get to read the bound project_id (the report row's
		// substrate scope).
		tk := callTool(t, ctx, mcpURL, bearer, "task_get", map[string]any{
			"id": mint.TaskID,
		})
		projectID, _ := tk["project_id"].(string)
		if projectID == "" {
			t.Fatalf("task_get(%s) returned empty project_id (raw=%+v)", mint.TaskID, tk)
		}

		// Synthesised report row content matches the AC5 schema.
		report := map[string]any{
			"verdict": "audit:pass",
			"checks": []map[string]any{
				{"name": "non_drift", "status": "pass", "findings": []any{}, "recommended_fix": ""},
				{"name": "agent_capability", "status": "pass", "findings": []any{}, "recommended_fix": ""},
				{"name": "canonical_chain_coverage", "status": "pass", "findings": []any{}, "recommended_fix": ""},
				{"name": "principle_citation_validity", "status": "pass", "findings": []any{}, "recommended_fix": ""},
				{"name": "story_template_field_validity", "status": "warn", "findings": []any{}, "recommended_fix": "Allowlist not configured."},
				{"name": "orphan_check", "status": "pass", "findings": []any{}, "recommended_fix": ""},
			},
			"scope":      mint.Scope,
			"audited_at": time.Now().UTC().Format(time.RFC3339),
		}
		reportJSON, _ := json.Marshal(report)
		appendOut := callTool(t, ctx, mcpURL, bearer, "ledger_append", map[string]any{
			"project_id": projectID,
			"type":       "evidence",
			"content":    string(reportJSON),
			"tags": []any{
				"kind:audit-report",
				"phase:substrate_audit",
				"task_id:" + mint.TaskID,
				"verdict:audit:pass",
			},
		})
		reportID, _ := appendOut["id"].(string)
		if reportID == "" {
			t.Fatalf("ledger_append did not return id: %+v", appendOut)
		}

		// Verdict-tag filtering surfaces the report row — proves the
		// schema's downstream consumability without parsing JSON
		// content.
		listOut := callToolArray(t, ctx, mcpURL, bearer, "ledger_list", map[string]any{
			"project_id": projectID,
			"tags":       []any{"verdict:audit:pass"},
		})
		found := false
		for _, row := range listOut {
			m, _ := row.(map[string]any)
			if m == nil {
				continue
			}
			if id, _ := m["id"].(string); id == reportID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ledger_list(tags=verdict:audit:pass) did not surface report row id=%s (got %d rows)", reportID, len(listOut))
		}
	})
}

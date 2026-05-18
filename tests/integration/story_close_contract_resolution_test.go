package integration

// story_close_contract_resolution_test.go — sty_44fdf9f4 anchor.
//
// Locks the wire-level contract for the close-evidence `contract_id`
// field. The mechanical story_close gate resolves the `story_close`
// contract via Documents.ResolveByName (project → workspace → system)
// and records the resolved document id on the kind:close-evidence
// row's payload. Two scenarios cover the AC matrix:
//
//   - SystemScopeContractRecorded — only the system-seeded
//     `story_close` contract is present; the payload's contract_id
//     is that system contract's id. Anchors AC1 (gate reads the
//     contract by name) + AC4 (pr_mandate_configuration_over_code
//     cited via the new contract body).
//   - ProjectScopeOverrideRecorded — a project-scope override
//     authored via document_add(scope=project, type=contract,
//     name=story_close, project_id=<…>) wins the resolver; the
//     payload's contract_id is the project doc's id (NOT the
//     system one). Anchors AC3 (adding a project-scope close
//     contract requires only a new contract doc; no Go change).
//
// Both scenarios assert AC2 (status walks to done in the same call —
// observable as story.status == "done" on the immediate story_get).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/tests/common/containers"
)

// TestStoryCloseContractResolution boots one container stack and runs
// the two scenarios as subtests sharing the same project + agent
// fixtures (the heavy boot amortises). Each scenario mints its own
// story so the close-evidence payload is unambiguously the
// scenario's.
func TestStoryCloseContractResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers story_close contract-resolution test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()

	const bearer = "key_contract_resolution"
	stack := containers.StartStack(t, ctx, containers.Options{
		ServerEnv:     map[string]string{"SATELLITES_API_KEYS": bearer},
		SkipDocsMount: true,
	})
	defer stack.Stop()
	baseURL := stack.BaseURL

	mcpURL := baseURL + "/mcp"
	rpcInit(t, ctx, mcpURL, bearer)

	// Resolve the system-seeded story_close contract's id. The configseed
	// loader lands `config/seed/system/contracts/story_close.md` at boot
	// (sty_44fdf9f4), so the row is present from first connect; we look
	// it up by name + type so the test survives doc-id churn.
	sysContractID := resolveSystemStoryCloseID(t, ctx, mcpURL, bearer)
	if sysContractID == "" {
		t.Fatal("system story_close contract not seeded; sty_44fdf9f4 seed file missing or unparseable")
	}

	// SystemScopeContractRecorded uses its own project so the project-
	// scope override authored in the second scenario cannot bleed in.
	t.Run("SystemScopeContractRecorded", func(t *testing.T) {
		ff := mintContractResolutionFixtures(t, ctx, baseURL, mcpURL, bearer, "story-close-system-contract")
		storyID := mintAndChainStoryForResolution(t, ctx, ff, "system-scope contract recorded")
		resp := callStoryClose(t, ctx, ff, storyID)
		assertResolutionPass(t, resp)
		payload := readClosePayload(t, ctx, ff, storyID)
		if got, _ := payload["contract_id"].(string); got != sysContractID {
			t.Errorf("contract_id = %q, want system contract id %q (full payload: %+v)", got, sysContractID, payload)
		}
		assertStoryReachedDone(t, ctx, ff, storyID)
	})

	// ProjectScopeOverrideRecorded mints its own project, authors a
	// project-scope story_close contract via document_add, then runs the
	// close on a fresh story. The system-scope row is still present, so
	// passing here proves the project tier wins the resolver. AC3 anchor.
	t.Run("ProjectScopeOverrideRecorded", func(t *testing.T) {
		ff := mintContractResolutionFixtures(t, ctx, baseURL, mcpURL, bearer, "story-close-project-override")
		overrideID := authorProjectStoryCloseContract(t, ctx, ff)
		if overrideID == "" {
			t.Fatal("project-scope story_close document_add returned no id")
		}
		if overrideID == sysContractID {
			t.Fatal("fixture sanity: project override id must differ from system contract id")
		}
		storyID := mintAndChainStoryForResolution(t, ctx, ff, "project-scope override recorded")
		resp := callStoryClose(t, ctx, ff, storyID)
		assertResolutionPass(t, resp)
		payload := readClosePayload(t, ctx, ff, storyID)
		if got, _ := payload["contract_id"].(string); got != overrideID {
			t.Errorf("contract_id = %q, want project override id %q (full payload: %+v)", got, overrideID, payload)
		}
		if got, _ := payload["contract_id"].(string); got == sysContractID {
			t.Errorf("contract_id = system id %q, expected project override to win", sysContractID)
		}
		assertStoryReachedDone(t, ctx, ff, storyID)
	})
}

// mintContractResolutionFixtures provisions one project + the fixture
// agents (developer + system-seeded story_reviewer) the chain helpers
// need. Each subtest gets its own project so a project-scope contract
// authored in one subtest cannot leak into the other.
func mintContractResolutionFixtures(t *testing.T, ctx context.Context, baseURL, mcpURL, bearer, projectName string) *storyCloseFixtures {
	t.Helper()
	createResp := callAPIv1(t, ctx, baseURL, bearer, "project_add", map[string]any{"name": projectName})
	projectID, _ := createResp["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", createResp)
	}

	developerAgent := ensureSystemAgentFixture(t, ctx, baseURL, bearer,
		"developer_agent_contract_resolution_"+projectName,
		[]string{"contract:plan", "contract:develop"}, nil)

	agentArr := callToolArray(t, ctx, mcpURL, bearer, "document_list", map[string]any{
		"type":  "agent",
		"limit": 50,
	})
	reviewAgent := agentIDFromList(agentArr, "story_reviewer")
	if developerAgent == "" || reviewAgent == "" {
		t.Fatalf("agents not resolvable: developer=%q review=%q", developerAgent, reviewAgent)
	}

	return &storyCloseFixtures{
		baseURL:          baseURL,
		mcpURL:           mcpURL,
		bearer:           bearer,
		projectID:        projectID,
		developerAgent:   developerAgent,
		storyReviewAgent: reviewAgent,
	}
}

// mintAndChainStoryForResolution mints an improvement-category story
// and walks the minimal chain the close gate requires (one closed
// develop work task + one closed story_review task carrying a
// kind:verdict verdict:pass row + a kind:release-evidence row
// matching pprod's reported commit). Returns the story id.
//
// Template-field carriers populate before_after / fix_commit /
// regression_test_path via kind:template-field:<name> ledger rows
// (sty_4885dc89), so the close gate's template roll-up passes
// without an operator-side story_update(fields=…) call.
func mintAndChainStoryForResolution(t *testing.T, ctx context.Context, ff *storyCloseFixtures, title string) string {
	t.Helper()
	storyResp := callAPIv1(t, ctx, ff.baseURL, ff.bearer, "story_add", map[string]any{
		"project_id":          ff.projectID,
		"title":               title,
		"description":         "sty_44fdf9f4 contract-resolution fixture",
		"acceptance_criteria": "see plan ldg_43a39416",
		"category":            "improvement",
		"fields": map[string]any{
			"motivation": "anchor sty_44fdf9f4 contract resolution",
		},
	})
	storyID, _ := storyResp["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add returned no id: %+v", storyResp)
	}

	_ = addAndCloseWorkTask(t, ctx, ff, storyID, "develop for "+title)
	reviewID := addReviewTask(t, ctx, ff, storyID, true)
	appendVerdictRow(t, ctx, ff, storyID, reviewID, "pass")
	appendReleaseEvidenceRow(t, ctx, ff, storyID, "unknown")

	appendTemplateFieldRow(t, ctx, ff, storyID, "before_after",
		"before: gate cited stale runReviewer prose; after: gate cites story_close contract id on close-evidence")
	appendTemplateFieldRow(t, ctx, ff, storyID, "fix_commit",
		"sty_44fdf9f4-fix")
	appendTemplateFieldRow(t, ctx, ff, storyID, "regression_test_path",
		"tests/integration/story_close_contract_resolution_test.go")

	return storyID
}

// resolveSystemStoryCloseID returns the document id of the system-
// scope `story_close` contract seeded from
// `config/seed/system/contracts/story_close.md`. Tests would otherwise
// have to hard-code the id (brittle across re-seeds), so we look up by
// (name, type, scope) via document_list.
func resolveSystemStoryCloseID(t *testing.T, ctx context.Context, mcpURL, bearer string) string {
	t.Helper()
	rows := callToolArray(t, ctx, mcpURL, bearer, "document_list", map[string]any{
		"type":  "contract",
		"scope": "system",
		"limit": 100,
	})
	for _, raw := range rows {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		if name, _ := m["name"].(string); name == "story_close" {
			id, _ := m["id"].(string)
			return id
		}
	}
	return ""
}

// authorProjectStoryCloseContract mints a project-scope contract row
// named `story_close` scoped to ff.projectID via the HTTP API surface
// (`POST /api/v1/contract/add`). Returns the new document id so the
// test can assert it lands on the close-evidence payload.
//
// The contract body is intentionally a short override stub; the gate
// records the document id, not its body, so the prose contents do not
// affect the close. The Go layer ignores the structured payload
// entirely on the close path — the resolver only looks at (Type,
// Name, Scope, ProjectID), and only the document id appears on the
// close-evidence row.
//
// HTTP/CLI is the write surface for typed document verbs after
// sty_4db0e025 slice B1 — `contract_add` is not registered on MCP.
// `pr_mcp_cli_shared_path` keeps the business logic shared via
// internal/client, but the wire route differs.
func authorProjectStoryCloseContract(t *testing.T, ctx context.Context, ff *storyCloseFixtures) string {
	t.Helper()
	resp := callAPIv1(t, ctx, ff.baseURL, ff.bearer, "contract_add", map[string]any{
		"scope":      "project",
		"name":       "story_close",
		"project_id": ff.projectID,
		"body":       "# project-scope story_close override\n\nFixture override for sty_44fdf9f4 integration test.\n",
		"tags":       []string{"sty_44fdf9f4", "fixture", "project-override"},
	})
	id, _ := resp["id"].(string)
	return id
}

// readClosePayload reads back the kind:close-evidence ledger row for
// the story and returns the decoded JSON payload. Fails the test if
// no row is present, or if the row's content is not valid JSON.
func readClosePayload(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) map[string]any {
	t.Helper()
	rawResp := callToolRaw(t, ctx, ff.mcpURL, ff.bearer, "ledger_list", map[string]any{
		"project_id": ff.projectID,
		"story_id":   storyID,
		"tags":       []any{"kind:close-evidence"},
		"limit":      10,
	})
	if isToolError(rawResp) {
		t.Fatalf("ledger_list error: %+v", rawResp)
	}
	text := extractToolText(t, rawResp)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("ledger_list response is not JSON array: %v: %s", err, text)
	}
	if len(rows) == 0 {
		t.Fatalf("no kind:close-evidence rows present on story %q", storyID)
	}
	content, _ := rows[0]["content"].(string)
	if content == "" {
		t.Fatalf("close-evidence row has empty content: %+v", rows[0])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("close-evidence content is not JSON: %v: %s", err, content)
	}
	return payload
}

// assertResolutionPass checks the story_close response shape — status
// must be "pass" with a non-empty evidence_id and story_status=done.
func assertResolutionPass(t *testing.T, resp map[string]any) {
	t.Helper()
	if status, _ := resp["status"].(string); status != "pass" {
		t.Fatalf("story_close status = %v, want pass; resp=%+v", resp["status"], resp)
	}
	if id, _ := resp["evidence_id"].(string); id == "" {
		t.Errorf("evidence_id missing on pass: %+v", resp)
	}
	if got, _ := resp["story_status"].(string); got != "done" {
		t.Errorf("response story_status = %q, want done", got)
	}
}

// assertStoryReachedDone re-fetches the story via story_get and
// confirms it is at status=done. AC2 (status walks immediately in
// the same call) anchor.
func assertStoryReachedDone(t *testing.T, ctx context.Context, ff *storyCloseFixtures, storyID string) {
	t.Helper()
	getResp := callTool(t, ctx, ff.mcpURL, ff.bearer, "story_get", map[string]any{"id": storyID})
	st, _ := getResp["story"].(map[string]any)
	if got, _ := st["status"].(string); got != "done" {
		t.Errorf("story_get status = %q, want done after PASS", got)
	}
}

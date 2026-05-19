package configseed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

const sampleAgentMD = `---
name: test_agent
permission_patterns:
  - "Read:**"
  - "Bash:git_status"
tags: [test]
---
# Test Agent

Body content for the test agent.
`

// sampleContractMD intentionally carries a permitted_actions key in
// frontmatter so TestRun_ContractStructuredOmitsPermittedActions can
// assert the loader IGNORES it (story_b7bf3a5f). The substrate sources
// permission_patterns from the agent doc, not the contract.
const sampleContractMD = `---
name: test_contract
category: develop
required_role: role_orchestrator
required_categories: [develop]
permitted_actions:
  - "Read:**"
evidence_required: |
  Build + test outputs.
validation_mode: llm
---
# Test Contract

Body content.
`

const sampleWorkflowMD = `---
name: test_workflow
required_slots:
  - { contract_name: plan, required: true, min_count: 1, max_count: 1 }
  - { contract_name: develop, required: true, min_count: 1, max_count: 5 }
---
# Test Workflow

Two-slot demo.
`

// writeFile writes content to dir/relPath, creating the directory tree.
func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestRun_CreatesAgentsContractsWorkflows covers AC1+AC2: the loader
// reads agents/, contracts/, workflows/ subdirs and Run upserts each
// into the document store.
func TestRun_CreatesAgentsContractsWorkflows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "system/agents/test_agent.md", sampleAgentMD)
	writeFile(t, dir, "system/contracts/test_contract.md", sampleContractMD)
	writeFile(t, dir, "system/workflows/test_workflow.md", sampleWorkflowMD)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	summary, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Loaded != 3 {
		t.Errorf("loaded = %d, want 3", summary.Loaded)
	}
	if summary.Created != 3 {
		t.Errorf("created = %d, want 3", summary.Created)
	}
	if len(summary.Errors) != 0 {
		t.Errorf("unexpected errors: %v", summary.Errors)
	}

	// Spot-check one document round-trip.
	got, err := docs.GetByName(context.Background(), "", "test_agent", nil)
	if err != nil {
		t.Fatalf("GetByName test_agent: %v", err)
	}
	if got.Type != document.TypeAgent {
		t.Errorf("type = %q, want %q", got.Type, document.TypeAgent)
	}
	if got.Scope != document.ScopeSystem {
		t.Errorf("scope = %q, want system", got.Scope)
	}
}

// TestRun_AgentStructuredCarriesInstruction (story_b7bf3a5f AC2) — the
// agent document carries the `instruction` frontmatter key in its
// Structured payload alongside permission_patterns. This is the
// concrete vehicle for agent-level execution guidance now that the
// contract no longer carries it (see TestRun_ContractStructuredOmitsPermittedActions).
//
// The configseed loader's mergeFrontmatterIntoJSON preserves arbitrary
// non-AgentSettings keys (parsers.go:181), so adding `instruction:` to
// frontmatter Just Works without parser changes — this test guards
// against accidental regression.
func TestRun_AgentStructuredCarriesInstruction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const agentWithInstructionMD = `---
name: test_agent_with_instruction
instruction: |
  This is the agent's execution guidance: do X, do not do Y.
permission_patterns:
  - "Read:**"
tags: [test]
---
# Test Agent

Body.
`
	writeFile(t, dir, "system/agents/test_agent_with_instruction.md", agentWithInstructionMD)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	agentDoc, err := docs.GetByName(context.Background(), "", "test_agent_with_instruction", nil)
	if err != nil {
		t.Fatalf("GetByName test_agent_with_instruction: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(agentDoc.Structured, &payload); err != nil {
		t.Fatalf("decode agent Structured: %v", err)
	}
	instruction, ok := payload["instruction"].(string)
	if !ok {
		t.Fatalf("agent Structured missing instruction string: payload=%v", payload)
	}
	if !strings.Contains(instruction, "do X") {
		t.Errorf("instruction = %q, want substring 'do X' (round-trip from frontmatter)", instruction)
	}
	// Sanity: permission_patterns still present alongside.
	patterns, _ := payload["permission_patterns"].([]any)
	if len(patterns) == 0 {
		t.Errorf("agent Structured missing permission_patterns alongside instruction")
	}
}

// TestRun_RealSeedAgentsCarryInstruction (story_b7bf3a5f AC2 +
// sty_a12e0f5d retier) — every lifecycle agent declares an
// `instruction` field, the canonical home for agent-level execution
// guidance now that contracts carry only audit shape.
//
// Sty_a12e0f5d moved developer_agent + releaser_agent from
// config/seed/system/agents/ to config/seed/wksp_5b3257d1/agents/ —
// the work agents are workspace-tier so a workspace authors them once
// and every project in the workspace inherits.
func TestRun_RealSeedAgentsCarryInstruction(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, seedDir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("Run real seed: %v", err)
	}
	if _, err := RunWorkspace(context.Background(), docs, seedDir, "wksp_5b3257d1", "system", now); err != nil {
		t.Fatalf("RunWorkspace real seed: %v", err)
	}
	for _, name := range []string{
		"developer_agent", "releaser_agent",
	} {
		agentDoc, err := docs.GetByName(context.Background(), "", name, nil)
		if err != nil {
			t.Errorf("GetByName %s: %v", name, err)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(agentDoc.Structured, &payload); err != nil {
			t.Errorf("%s: decode Structured: %v", name, err)
			continue
		}
		instruction, ok := payload["instruction"].(string)
		if !ok || strings.TrimSpace(instruction) == "" {
			t.Errorf("%s: missing or empty `instruction` field in Structured payload", name)
		}
	}
}

// TestRun_ContractStructuredOmitsPermittedActions (story_b7bf3a5f AC1+5)
// — even when the seed file's frontmatter carries `permitted_actions`,
// the loader must NOT write it into the contract document's Structured
// payload. The action-claim path sources permission_patterns from the
// agent doc (story_b39b393f / story_cc55e093); the contract's field is
// dead data.
func TestRun_ContractStructuredOmitsPermittedActions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "system/contracts/test_contract.md", sampleContractMD)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	contractDoc, err := docs.GetByName(context.Background(), "", "test_contract", nil)
	if err != nil {
		t.Fatalf("GetByName test_contract: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(contractDoc.Structured, &payload); err != nil {
		t.Fatalf("decode contract Structured: %v", err)
	}
	if _, has := payload["permitted_actions"]; has {
		t.Errorf("contract Structured carries permitted_actions key %v — story_b7bf3a5f drops the dead field; the action-claim path sources permission_patterns from the agent doc", payload["permitted_actions"])
	}
	// Sanity: contract-level fields preserved.
	for _, key := range []string{"category", "evidence_required", "validation_mode"} {
		if _, has := payload[key]; !has {
			t.Errorf("contract Structured missing %q (must be preserved)", key)
		}
	}
}

// TestRun_ContractStructuredDropsDispatchClass (sty_447b9fe0) — the
// contract document's Structured payload MUST NOT carry a
// `dispatch_class` field. Dispatch is action-agnostic post-hot-runner
// deletion; any contract that still declares the legacy field in its
// frontmatter has the key silently dropped by contractToInput, so the
// worker has no surface to branch on.
func TestRun_ContractStructuredDropsDispatchClass(t *testing.T) {
	t.Parallel()
	const stragglerContractMD = `---
name: test_straggler_contract
category: push
dispatch_class: hot
validation_mode: llm
---
# Test Straggler Contract (frontmatter still carries dispatch_class — must be dropped)
`
	dir := t.TempDir()
	writeFile(t, dir, "system/contracts/test_straggler_contract.md", stragglerContractMD)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	doc, err := docs.GetByName(context.Background(), "", "test_straggler_contract", nil)
	if err != nil {
		t.Fatalf("GetByName test_straggler_contract: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(doc.Structured, &payload); err != nil {
		t.Fatalf("decode Structured: %v", err)
	}
	if _, has := payload["dispatch_class"]; has {
		t.Errorf("contract Structured carries dispatch_class %v — sty_447b9fe0 dropped the field; contractToInput must not pass it through", payload["dispatch_class"])
	}
}

// TestRun_Idempotent covers AC2: a second Run pass with unchanged
// files produces zero creates/updates (body-hash convergence in
// document.Upsert).
func TestRun_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "system/agents/test_agent.md", sampleAgentMD)
	writeFile(t, dir, "system/contracts/test_contract.md", sampleContractMD)
	writeFile(t, dir, "system/workflows/test_workflow.md", sampleWorkflowMD)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	summary, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary.Created != 0 {
		t.Errorf("second Run created %d, want 0", summary.Created)
	}
	if summary.Updated != 0 {
		t.Errorf("second Run updated %d, want 0 (body unchanged)", summary.Updated)
	}
	if summary.Skipped != 3 {
		t.Errorf("second Run skipped %d, want 3", summary.Skipped)
	}
}

// sampleConfigurationMD references the contract by the same name as
// `sampleContractMD` plus the six lifecycle contract names a real
// system_default Configuration would carry. The test only seeds
// TestRun_MissingDirIsNoOp covers the resilience path: missing seed
// dirs are not an error — Run reports zero loaded.
func TestRun_MissingDirIsNoOp(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	summary, err := Run(context.Background(), docs, t.TempDir(), "wksp_sys", "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Loaded != 0 {
		t.Errorf("loaded = %d, want 0", summary.Loaded)
	}
}

// TestRun_BadFileRecordedAsError covers AC1's resilience: a malformed
// markdown file produces an error entry but does not abort sibling
// files.
func TestRun_BadFileRecordedAsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "system/agents/good.md", sampleAgentMD)
	writeFile(t, dir, "system/agents/bad.md", "no frontmatter here\n")

	docs := document.NewMemoryStore()
	summary, err := Run(context.Background(), docs, dir, "wksp_sys", "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Created != 1 {
		t.Errorf("created = %d, want 1 (only good.md)", summary.Created)
	}
	if len(summary.Errors) != 1 {
		t.Errorf("errors = %d, want 1", len(summary.Errors))
	}
}

// TestRun_StoryTemplatesLoadFromRealSeed covers sty_d2a03cea: every
// markdown file under config/seed/story_templates/ parses into a
// type=story_template document whose Structured payload carries
// `category`, `fields[]`, and per-status `hooks` with both
// `structured` and `natural_language` branches preserved verbatim.
// One template per category — tested by listing the resulting docs.
func TestRun_StoryTemplatesLoadFromRealSeed(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, seedDir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("Run real seed: %v", err)
	}
	for _, category := range []string{"bug", "feature", "improvement", "infrastructure", "documentation"} {
		doc, err := docs.GetByName(context.Background(), "", category, nil)
		if err != nil {
			t.Errorf("GetByName story_template %q: %v", category, err)
			continue
		}
		if doc.Type != document.TypeStoryTemplate {
			t.Errorf("%s: type = %q, want story_template", category, doc.Type)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(doc.Structured, &payload); err != nil {
			t.Errorf("%s: decode Structured: %v", category, err)
			continue
		}
		if got, _ := payload["category"].(string); got != category {
			t.Errorf("%s: payload.category = %q, want %q", category, got, category)
		}
		fields, ok := payload["fields"].([]any)
		if !ok || len(fields) == 0 {
			t.Errorf("%s: fields missing or empty: %v", category, payload["fields"])
		}
		hooks, ok := payload["hooks"].(map[string]any)
		if !ok || len(hooks) == 0 {
			t.Errorf("%s: hooks missing or empty: %v", category, payload["hooks"])
		}
	}

	// Bug template specifically must declare the canonical fields the
	// post-deploy methodology relies on.
	bugDoc, _ := docs.GetByName(context.Background(), "", "bug", nil)
	body := string(bugDoc.Structured)
	for _, name := range []string{"repro", "observed", "expected", "root_cause", "fix_commit", "regression_test_path", "post_deploy_check"} {
		if !strings.Contains(body, name) {
			t.Errorf("bug template structured payload missing %q field", name)
		}
	}
}

// TestResolveSeedDir_EnvOverride covers AC5: SATELLITES_SEED_DIR
// overrides the default path.
func TestResolveSeedDir_EnvOverride(t *testing.T) {
	t.Setenv(SeedDirEnv, "/custom/seed")
	if got := ResolveSeedDir(); got != "/custom/seed" {
		t.Errorf("ResolveSeedDir = %q, want /custom/seed", got)
	}
}

// TestResolveSeedDir_Default covers AC5 default path.
func TestResolveSeedDir_Default(t *testing.T) {
	t.Setenv(SeedDirEnv, "")
	if got := ResolveSeedDir(); got != DefaultSeedDir {
		t.Errorf("ResolveSeedDir = %q, want %q", got, DefaultSeedDir)
	}
}

// samplePrincipleMD covers the principle frontmatter shape per
// story_ac3dc4d0: id (documentation slug, not consumed by parser),
// name (friendly title, used by Upsert.GetByName), scope (system),
// tags (free-form). Body becomes Document.Body (the description).
const samplePrincipleMD = `---
id: pr_test_principle
name: Test principle for seed loader
scope: system
tags:
  - test
---
This is the description body of the test principle. It survives
through the loader unchanged and lands in Document.Body.
`

// TestPrincipleSeedLoad covers story_ac3dc4d0 ACs 1, 3, 4, 5, 6, 7:
// the principle phase loads `principles/*.md`, upserts as
// type=principle scope=system, and is idempotent on re-run. story_ac3dc4d0.
func TestPrincipleSeedLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "system/principles/pr_test_principle.md", samplePrincipleMD)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	// First run: creates the principle doc.
	summary, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(summary.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", summary.Errors)
	}
	if summary.Created < 1 {
		t.Fatalf("created = %d, want >=1", summary.Created)
	}

	got, err := docs.GetByName(context.Background(), "", "Test principle for seed loader", nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Type != document.TypePrinciple {
		t.Errorf("type = %q, want %q", got.Type, document.TypePrinciple)
	}
	if got.Scope != document.ScopeSystem {
		t.Errorf("scope = %q, want system", got.Scope)
	}
	if !strings.Contains(got.Body, "description body of the test principle") {
		t.Errorf("body did not survive: %q", got.Body)
	}

	// Second run: idempotent — body hash unchanged → 0 changes for this
	// principle (skipped count goes up).
	prevSkipped := summary.Skipped
	summary2, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if len(summary2.Errors) != 0 {
		t.Fatalf("unexpected errors on re-run: %v", summary2.Errors)
	}
	if summary2.Created != 0 {
		t.Errorf("re-run created = %d, want 0", summary2.Created)
	}
	if summary2.Skipped <= prevSkipped {
		t.Errorf("re-run skipped = %d, want > prev %d (principle should have skipped)", summary2.Skipped, prevSkipped)
	}
}

// TestRunProject_RealSeedDirShipsSatellitesPrinciples checks the
// loader's structural contract on the real
// config/seed/wksp_5b3257d1/principles/ directory: every .md file
// becomes one type=principle scope=workspace document with a non-empty
// body and workspace_id stamped on the row. Sty_7f5585e9 moved the
// satellites principles up from the project tier to the workspace
// tier so a second project added to the workspace inherits them
// without per-project copying.
func TestRunWorkspace_RealSeedDirShipsSatellitesPrinciples(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const ws = "wksp_5b3257d1"
	files, err := filepath.Glob(filepath.Join(seedDir, ws, "principles", "*.md"))
	if err != nil {
		t.Fatalf("glob principles: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no principle files under %s/%s/principles", seedDir, ws)
	}

	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	summary, err := RunWorkspace(context.Background(), docs, seedDir, ws, "system", now)
	if err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}
	if len(summary.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", summary.Errors)
	}

	principles, err := docs.List(context.Background(), document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeWorkspace,
		Limit: len(files) * 2,
	}, nil)
	if err != nil {
		t.Fatalf("List principles: %v", err)
	}
	if len(principles) != len(files) {
		t.Fatalf("principles count = %d, want %d (one per .md file)", len(principles), len(files))
	}
	for _, p := range principles {
		if p.Body == "" {
			t.Errorf("principle %q has empty body", p.Name)
		}
		if p.WorkspaceID != ws {
			t.Errorf("principle %q workspace_id = %q, want %s", p.Name, p.WorkspaceID, ws)
		}
		if p.ProjectID != nil {
			t.Errorf("principle %q project_id = %v, want nil", p.Name, p.ProjectID)
		}
	}
}

// TestRun_SystemPrinciplesDirEmpty asserts the post-sty_8f6b90c8
// state: no principle .md files live under config/seed/system/. The
// satellites principles all moved to the project tier; any future
// system-scope principle (truly substrate-generic) would be the first
// to repopulate this directory — at which point this test would
// flip to an "at least one" assertion or be retired.
func TestRun_SystemPrinciplesDirEmpty(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(seedDir, SystemSubdir, "principles", "*.md"))
	if err != nil {
		t.Fatalf("glob principles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("system principles dir not empty: %v", files)
	}
}

// TestRun_RoleSeedNotPresent (epic:roleless-agents) — confirms that
// after the role tier was retired, the seed loader writes no role docs.
// Replaces the legacy sty_a1a77518 test that expected role_orchestrator
// and role_reviewer to be loaded. With agents as the sole capability
// tier, GetByName("role_orchestrator") must miss.
func TestRun_RoleSeedNotPresent(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, seedDir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("Run real seed: %v", err)
	}
	for _, name := range []string{"role_orchestrator", "role_reviewer"} {
		if _, err := docs.GetByName(context.Background(), "", name, nil); err == nil {
			t.Errorf("expected role doc %q to be absent post-retire; found it", name)
		}
	}
}

// TestRun_FrontmatterOnlyEditReseeds (sty_34130d76 AC3) — a seed file
// whose ONLY change between version N and version N+1 is in the
// frontmatter (here `permission_patterns:` gains an entry) MUST be
// re-applied. Pre-fix this test fails: docs.Upsert short-circuited on
// HashBody(in.Body), which matched between runs because the body was
// untouched, leaving in.Structured / in.Tags / in.ContractBinding
// silently dropped on the floor. Post-fix HashContent covers all four
// fields, so the structured-only change drives summary.Updated == 1
// and the live row's Structured payload reflects the new value.
func TestRun_FrontmatterOnlyEditReseeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const v1 = `---
name: test_agent
permission_patterns:
  - "Read:**"
  - "Bash:git_status"
tags: [test]
---
# Test Agent

Body content for the test agent.
`
	const v2 = `---
name: test_agent
permission_patterns:
  - "Read:**"
  - "Bash:git_status"
  - "Bash:git_diff"
tags: [test]
---
# Test Agent

Body content for the test agent.
`
	writeFile(t, dir, "system/agents/test_agent.md", v1)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Rewrite the seed file body-identical but frontmatter-different.
	writeFile(t, dir, "system/agents/test_agent.md", v2)

	summary, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary.Updated != 1 {
		t.Errorf("second Run updated = %d, want 1 (frontmatter-only edit must reseed)", summary.Updated)
	}
	if summary.Skipped != 0 {
		t.Errorf("second Run skipped = %d, want 0 (the only seed file changed)", summary.Skipped)
	}

	// The Structured payload must reflect the post-edit permission_patterns.
	got, err := docs.GetByName(context.Background(), "", "test_agent", nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Structured, &payload); err != nil {
		t.Fatalf("decode Structured: %v", err)
	}
	patterns, _ := payload["permission_patterns"].([]any)
	if len(patterns) != 3 {
		t.Fatalf("permission_patterns len = %d, want 3 (frontmatter edit landed)", len(patterns))
	}
	if !equalAny(patterns[2], "Bash:git_diff") {
		t.Errorf("permission_patterns[2] = %v, want %q", patterns[2], "Bash:git_diff")
	}
	if got.Version != 2 {
		t.Errorf("row version = %d, want 2 (one bump on the reseed)", got.Version)
	}
}

// TestRun_CommentOnlyFrontmatterStillNoOp (sty_34130d76 AC4) — a YAML
// comment-only edit between runs must NOT trigger a reseed. The YAML
// decoder discards comments before mergeFrontmatterIntoJSON, so the
// Structured payload (and the new HashContent over it) are identical
// between v1 and v2. Pins option (a)'s contract that whitespace /
// comment-only edits do not pollute version history.
func TestRun_CommentOnlyFrontmatterStillNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const v1 = `---
name: test_agent
permission_patterns:
  - "Read:**"
  - "Bash:git_status"
tags: [test]
---
# Test Agent

Body content.
`
	const v2 = `---
name: test_agent
# patterns the agent is allowed to claim
permission_patterns:
  - "Read:**"
  - "Bash:git_status"
tags: [test]
---
# Test Agent

Body content.
`
	writeFile(t, dir, "system/agents/test_agent.md", v1)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	writeFile(t, dir, "system/agents/test_agent.md", v2)
	summary, err := Run(context.Background(), docs, dir, "wksp_sys", "system", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary.Skipped != 1 {
		t.Errorf("second Run skipped = %d, want 1 (comment-only edit must stay no-op)", summary.Skipped)
	}
	if summary.Updated != 0 {
		t.Errorf("second Run updated = %d, want 0", summary.Updated)
	}

	got, err := docs.GetByName(context.Background(), "", "test_agent", nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("row version = %d, want 1 (comment-only edit must not bump version)", got.Version)
	}
}

// equalAny compares the small subset of types JSON unmarshal yields here
// (string, []any with string contents) without pulling in reflect.DeepEqual's
// surprises around nil vs empty slices.
func equalAny(a, b any) bool {
	switch ax := a.(type) {
	case string:
		bx, ok := b.(string)
		return ok && ax == bx
	case []any:
		bx, ok := b.([]any)
		if !ok {
			return false
		}
		if len(ax) != len(bx) {
			return false
		}
		for i := range ax {
			if !equalAny(ax[i], bx[i]) {
				return false
			}
		}
		return true
	case nil:
		bx, ok := b.([]any)
		if ok && len(bx) == 0 {
			return true
		}
		return b == nil
	}
	return false
}

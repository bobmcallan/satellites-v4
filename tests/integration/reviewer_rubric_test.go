package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReviewerRubric_PostDriftFix asserts the seeded rubric markdown
// honours sty_7a061d73's invariants: deleted-concept enforcement is
// gone (sty_92218a87 purged required_role / role_grant), the rubric
// instructs the reviewer to read structural state via task_walk
// rather than demanding prose recital, evidence_ledger_ids are
// first-class evidence, and the develop-close gate enforces a
// "rubric updates" checklist when substrate primitives change.
//
// The test reads the seed files directly. It does NOT spin a server
// or call Gemini — those would either need a real API key (flaky,
// costs money) or a stub reviewer that always accepts (meaningless).
// sty_51571015 retired the in-process reviewer service; reviewer
// agents are now dispatched by the orchestrator session per the
// seed-prescribed dispatch mechanism, and the dispatched subprocess
// reads its own agent doc body as the system prompt. So the seeded
// markdown IS what the live reviewer reads at runtime — file-level
// assertions remain the faithful proxy for "the live reviewer will
// behave per the rubric." Drift back into deleted concepts will
// fail this test before it reaches a contract close.
//
// The "synthetic 7-step workflow" framing in sty_7a061d73's AC #4
// is a description of the BEHAVIOURAL invariant the rubric must
// support: a plan-md that cites "see task_walk for ordered chain"
// in lieu of reciting the chain in prose must be acceptable. The
// invariant is encoded here as TC2 (task_walk mentioned in the
// sequence-verification context) — when this passes, the rubric
// body the live reviewer reads instructs it to accept that form.
func TestReviewerRubric_PostDriftFix(t *testing.T) {
	root := repoRoot(t)
	storyReviewer := readSeedFile(t, root, "config/seed/agents/story_reviewer.md")
	devReviewer := readSeedFile(t, root, "config/seed/agents/development_reviewer.md")
	planContract := readSeedFile(t, root, "config/seed/contracts/plan.md")

	t.Run("TC1_no_deleted_role_concepts_in_reviewer_rubric", func(t *testing.T) {
		// sty_92218a87 purged required_role, permitted_roles,
		// role_grant, OrchestratorGrantID, RoleGrant from the
		// substrate. The rubric must not enforce them.
		bannedTokens := []string{
			"required_role",
			"permitted_roles",
			"role_grant",
			"OrchestratorGrantID",
			"RoleGrant",
		}
		for _, tok := range bannedTokens {
			assert.NotContainsf(t, storyReviewer, tok,
				"story_reviewer.md must not enforce deleted concept %q (purged by sty_92218a87)", tok)
			assert.NotContainsf(t, devReviewer, tok,
				"development_reviewer.md must not enforce deleted concept %q (purged by sty_92218a87)", tok)
		}
	})

	t.Run("TC2_task_walk_referenced_for_sequence_verification", func(t *testing.T) {
		// The rubric must instruct the reviewer to call task_walk
		// rather than demand prose recital of the contract chain.
		require.Contains(t, storyReviewer, "task_walk",
			"story_reviewer.md must mention task_walk for sequence verification")
		// Verify the mention is in a sequence-verification context
		// (mandate compliance / contract sequence section), not just
		// a passing reference.
		assert.Regexp(t,
			`(?s)task_walk.{0,400}(chain|sequence)`,
			storyReviewer,
			"task_walk reference in story_reviewer.md must appear near sequence-verification language")
	})

	t.Run("TC3_evidence_ledger_ids_first_class_in_both_rubrics", func(t *testing.T) {
		require.Contains(t, storyReviewer, "evidence_ledger_ids",
			"story_reviewer.md must instruct dereferencing evidence_ledger_ids")
		require.Contains(t, devReviewer, "evidence_ledger_ids",
			"development_reviewer.md must instruct dereferencing evidence_ledger_ids")
		// Verify the mention is in a dereference-first context,
		// not a "reject see-row-X citations" context.
		for _, body := range []struct {
			name, content string
		}{
			{"story_reviewer.md", storyReviewer},
			{"development_reviewer.md", devReviewer},
		} {
			assert.Regexp(t,
				`(?s)evidence_ledger_ids.{0,500}(dereference|ledger_get|first-class)`,
				body.content,
				"%s must describe evidence_ledger_ids as dereferenceable first-class evidence", body.name)
		}
	})

	t.Run("TC4_rubric_updates_gate_documented", func(t *testing.T) {
		// The "rubric updates" plan-md gate must be described in
		// both reviewer rubrics — story_reviewer enforces at plan
		// close, development_reviewer enforces at develop close.
		for _, body := range []struct {
			name, content string
		}{
			{"story_reviewer.md", storyReviewer},
			{"development_reviewer.md", devReviewer},
		} {
			contentLower := strings.ToLower(body.content)
			assert.Containsf(t, contentLower, "rubric update",
				"%s must document the 'rubric updates' plan-md gate (sty_7a061d73)", body.name)
			// The gate must reference substrate primitive change as
			// the trigger condition.
			assert.Regexp(t,
				`(?si)rubric update.{0,800}(substrate.{0,40}primitive|verb add|schema field|MCP signature)`,
				body.content,
				"%s rubric-updates section must trigger on substrate-primitive changes", body.name)
		}
	})

	t.Run("TC5_plan_contract_doc_free_of_deleted_role_examples", func(t *testing.T) {
		bannedExamples := []string{
			"required_role:developer",
			"required_role:reviewer",
			"required_role:releaser",
		}
		for _, ex := range bannedExamples {
			assert.NotContainsf(t, planContract, ex,
				"plan.md contract doc must not show example %q (sty_92218a87 deleted required_role)", ex)
		}
		// Generic required_role token also banned in plan.md.
		assert.NotContains(t, planContract, "required_role",
			"plan.md contract doc must not reference required_role at all (sty_92218a87)")
	})

	t.Run("TC6_agent_process_v3_documents_rubric_maintenance", func(t *testing.T) {
		// AC #5 of sty_7a061d73 — docs/agent_process_v3.md must
		// gain a "Reviewer rubric maintenance" subsection.
		v3 := readSeedFile(t, root, "docs/agent_process_v3.md")
		assert.Contains(t, v3, "Reviewer rubric maintenance",
			"docs/agent_process_v3.md must document rubric maintenance (sty_7a061d73 AC#5)")
		// Subsection must cover all three points from the AC.
		v3Lower := strings.ToLower(v3)
		assert.Contains(t, v3Lower, "rubric updates ride with",
			"agent_process_v3.md rubric-maintenance section must say rubric updates ride with substrate-evolution stories")
		assert.Contains(t, v3Lower, "develop close",
			"agent_process_v3.md rubric-maintenance section must describe the develop-close gate")
	})
}

// readSeedFile reads a path relative to the repo root, failing the
// test with a useful message if the file is missing.
func readSeedFile(t *testing.T, root, rel string) string {
	t.Helper()
	abs := filepath.Join(root, rel)
	body, err := os.ReadFile(abs)
	require.NoErrorf(t, err, "read %s", rel)
	return string(body)
}

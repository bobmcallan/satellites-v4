package configseed

import (
	"path/filepath"
	"sort"
	"testing"
)

// TestAgentSeed_RoleFieldPresent (sty_58c4839a) walks every type=agent
// seed file under config/seed/ and asserts the frontmatter carries a
// `role:` key whose value is one of the closed set
// {orchestration, execution, review}. The role field is the structural
// pin for the authority surface gridded by `pr_role_grid`; future
// stories (Story 2 enforcement onwards) read it to admit verbs.
//
// Failure mode: collect every offending file then fail once with the
// full surface, same shape as TestSeedNameConvention.
func TestAgentSeed_RoleFieldPresent(t *testing.T) {
	t.Parallel()

	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}

	allowed := map[string]struct{}{
		"orchestration": {},
		"execution":     {},
		"review":        {},
	}

	agents := loadAgentFrontmatters(t, seedDir)

	type offence struct {
		name   string
		role   string
		reason string
	}
	var offenders []offence

	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		fm := agents[name]
		role := fm.String("role")
		if role == "" {
			offenders = append(offenders, offence{
				name:   name,
				role:   role,
				reason: "frontmatter missing required `role:` key",
			})
			continue
		}
		if _, ok := allowed[role]; !ok {
			offenders = append(offenders, offence{
				name:   name,
				role:   role,
				reason: "role value not in allowed set {orchestration, execution, review}",
			})
		}
	}

	for _, o := range offenders {
		t.Errorf("agent %q: role=%q — %s", o.name, o.role, o.reason)
	}
}

// TestAgentSeed_RoleMatchesGrid (sty_58c4839a) asserts the seven
// seeded agents each carry the role value the `pr_role_grid` workspace
// principle prescribes. The grid is the source of truth; this lint
// catches a re-seed that drifts an agent's role away from the grid.
// Unknown agents are skipped — the closed seven-row table is the
// regression surface, and a future story extending the agent catalog
// updates the grid (in the principle body) before adding a row here.
func TestAgentSeed_RoleMatchesGrid(t *testing.T) {
	t.Parallel()

	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}

	grid := map[string]string{
		"claude_orchestrator":  "orchestration",
		"developer_agent":      "execution",
		"releaser_agent":       "execution",
		"substrate_auditor":    "execution",
		"development_reviewer": "review",
		"story_reviewer":       "review",
		"gemini_reviewer":      "review",
	}

	agents := loadAgentFrontmatters(t, seedDir)

	names := make([]string, 0, len(grid))
	for n := range grid {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		expected := grid[name]
		fm, ok := agents[name]
		if !ok {
			t.Errorf("grid agent %q: no agent seed file under config/seed/", name)
			continue
		}
		got := fm.String("role")
		if got != expected {
			t.Errorf("grid agent %q: role=%q want %q", name, got, expected)
		}
	}
}

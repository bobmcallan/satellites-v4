package auth_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/auth"
)

// TestRoleVerbDefaults_ClosedRoleSet asserts the role keys are
// exactly the three allowed values. pr_role_grid forbids a fourth.
func TestRoleVerbDefaults_ClosedRoleSet(t *testing.T) {
	t.Parallel()
	got := auth.KnownRoles()
	want := []string{auth.RoleExecution, auth.RoleOrchestration, auth.RoleReview}
	if len(got) != len(want) {
		t.Fatalf("KnownRoles() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("KnownRoles()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDefaultsForRole_UnknownRole returns nil for unknown roles —
// caller (AgentAPIKeyCreate) treats nil as a reject.
func TestDefaultsForRole_UnknownRole(t *testing.T) {
	t.Parallel()
	if got := auth.DefaultsForRole("manager"); got != nil {
		t.Errorf("DefaultsForRole(manager) = %v, want nil", got)
	}
	if got := auth.DefaultsForRole(""); got != nil {
		t.Errorf("DefaultsForRole(\"\") = %v, want nil", got)
	}
}

// TestDefaultsForRole_RolesNonEmpty asserts each role has at least
// one verb. An empty role set would silently deny every verb at the
// gate.
func TestDefaultsForRole_RolesNonEmpty(t *testing.T) {
	t.Parallel()
	for _, role := range auth.KnownRoles() {
		defaults := auth.DefaultsForRole(role)
		if len(defaults) == 0 {
			t.Errorf("DefaultsForRole(%q) is empty — every role must allow at least one verb", role)
		}
	}
}

// TestIsAllowedForRole asserts membership + non-membership flow.
func TestIsAllowedForRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role, verb string
		want       bool
	}{
		// Orchestration owns the full surface.
		{auth.RoleOrchestration, "story_add", true},
		{auth.RoleOrchestration, "task_add", true},
		{auth.RoleOrchestration, "ledger_append", true},
		// Execution: can close its own task + append to its own ledger;
		// cannot author stories or other tasks.
		{auth.RoleExecution, "task_update", true},
		{auth.RoleExecution, "ledger_append", true},
		{auth.RoleExecution, "task_add", false},
		{auth.RoleExecution, "story_add", false},
		{auth.RoleExecution, "story_update", false},
		{auth.RoleExecution, "principle_list", false},
		// Review: read-broad + own-task ledger_append; no authoring.
		{auth.RoleReview, "story_get", true},
		{auth.RoleReview, "task_walk", true},
		{auth.RoleReview, "ledger_list", true},
		{auth.RoleReview, "ledger_append", true},
		{auth.RoleReview, "principle_list", true},
		{auth.RoleReview, "task_add", false},
		{auth.RoleReview, "story_add", false},
		{auth.RoleReview, "story_update", false},
	}
	for _, tc := range cases {
		got := auth.IsAllowedForRole(tc.role, tc.verb)
		if got != tc.want {
			t.Errorf("IsAllowedForRole(%q, %q) = %v, want %v", tc.role, tc.verb, got, tc.want)
		}
	}
}

// TestVerbAllowlistSubset_AcceptSubset confirms a strict subset of
// the role's defaults is accepted.
func TestVerbAllowlistSubset_AcceptSubset(t *testing.T) {
	t.Parallel()
	subset := []string{"task_update", "ledger_append"}
	ok, offending := auth.VerbAllowlistSubset(auth.RoleExecution, subset)
	if !ok {
		t.Errorf("expected subset accepted, got offending=%v", offending)
	}
}

// TestVerbAllowlistSubset_RejectSuperset confirms a superset is
// rejected with the offending verbs (AC6 / pr_no_unrequested_compat).
func TestVerbAllowlistSubset_RejectSuperset(t *testing.T) {
	t.Parallel()
	subset := []string{"task_update", "story_add"}
	ok, offending := auth.VerbAllowlistSubset(auth.RoleExecution, subset)
	if ok {
		t.Fatalf("expected superset rejected, got ok=true")
	}
	if len(offending) != 1 || offending[0] != "story_add" {
		t.Errorf("offending = %v, want [story_add]", offending)
	}
}

// TestVerbAllowlistSubset_UnknownRoleRejectsAll asserts an unknown
// role rejects every verb.
func TestVerbAllowlistSubset_UnknownRoleRejectsAll(t *testing.T) {
	t.Parallel()
	subset := []string{"story_get"}
	ok, offending := auth.VerbAllowlistSubset("manager", subset)
	if ok {
		t.Fatalf("expected unknown role to reject, got ok=true")
	}
	if len(offending) != 1 || offending[0] != "story_get" {
		t.Errorf("offending = %v, want [story_get]", offending)
	}
}

// TestRoleGridLint_MatchesPrinciple is the grid lint: parse the
// `pr_role_grid` markdown table and assert the entries are
// represented in the runtime map. The principle is the design-time
// authority; the Go map is the runtime expression. They MUST agree
// on the orchestration / execution / review columns or the gate
// silently drifts from the seeded principle. sty_056b68f6.
func TestRoleGridLint_MatchesPrinciple(t *testing.T) {
	t.Parallel()
	// Walk up from the test cwd to the repo root (where config/ lives)
	// — the test is run from internal/auth/ but config/ is the repo
	// root, two ancestors up.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := cwd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(root, "config", "seed", "wksp_5b3257d1", "principles", "role_grid.md")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	path := filepath.Join(root, "config", "seed", "wksp_5b3257d1", "principles", "role_grid.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("role_grid.md not reachable from %s: %v", cwd, err)
	}

	// Extract the verbs from each role column. The grid is a
	// markdown table; rows look like `| story_add / story_update | ✓ | ✗ | ✗ |`.
	// "✓" is the affirmative marker; any non-"✗" content other than
	// pure "✓" (e.g. "gated by …", "own-task namespace only",
	// "inlined") is treated as conditional and skipped — those cells
	// represent verbs the orchestrator inlines into the prompt or
	// gates with another principle, NOT a wire-level allowlist
	// membership claim.
	lines := strings.Split(string(body), "\n")
	wantByRole := map[string]map[string]struct{}{
		auth.RoleOrchestration: {},
		auth.RoleExecution:     {},
		auth.RoleReview:        {},
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}
		// parts[0] is empty (leading pipe); parts[1] is the verb column.
		verbCol := strings.TrimSpace(parts[1])
		if verbCol == "" || strings.HasPrefix(verbCol, "Verb") || strings.HasPrefix(verbCol, "---") {
			continue
		}
		// Pull the individual verbs out of "story_add / story_update".
		verbs := splitVerbs(verbCol)
		cols := [3]string{
			strings.TrimSpace(parts[2]),
			strings.TrimSpace(parts[3]),
			strings.TrimSpace(parts[4]),
		}
		roleByIdx := [3]string{auth.RoleOrchestration, auth.RoleExecution, auth.RoleReview}
		for i, c := range cols {
			if c != "✓" {
				continue
			}
			for _, v := range verbs {
				wantByRole[roleByIdx[i]][v] = struct{}{}
			}
		}
	}

	// For every (role, verb) the grid says ✓, the runtime map MUST
	// contain the verb. The runtime map may legitimately carry MORE
	// verbs than the grid lists (the grid abbreviates "ledger_append
	// (own task tag)" → the runtime stores the bare `ledger_append`,
	// and the grid omits seed-only verbs like `satellites_info` that
	// every role can call).
	for role, want := range wantByRole {
		for v := range want {
			if !auth.IsAllowedForRole(role, v) {
				t.Errorf("grid says role=%q ✓ on verb=%q but IsAllowedForRole returns false — Go map drift from pr_role_grid", role, v)
			}
		}
	}
}

// splitVerbs handles "story_add / story_update" → ["story_add",
// "story_update"], strips parenthetical qualifiers like "(own task
// tag)", and ignores anything that isn't a bare snake_case identifier.
func splitVerbs(col string) []string {
	// Drop parenthetical qualifiers.
	if i := strings.Index(col, "("); i >= 0 {
		col = col[:i]
	}
	raw := strings.Split(col, "/")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		v := strings.TrimSpace(r)
		if v == "" {
			continue
		}
		// Bare snake_case identifier.
		if strings.ContainsAny(v, " \t→") {
			continue
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

package configseed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestContractBodies_HaveReviewPolicySection (sty_690b1653) locks
// the post-V4-rewrite shape: every lifecycle contract body declares
// its review policy explicitly. Reviewer dispatch is contract prose
// (sty_9f3562b8 retired the substrate-side pairing), so the body is
// the audit-of-record for whether and how a reviewer is dispatched.
func TestContractBodies_HaveReviewPolicySection(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	cases := []struct {
		name string
		path string
	}{
		{"plan", filepath.Join(seedDir, "system", "contracts", "plan.md")},
		{"story_review", filepath.Join(seedDir, "system", "contracts", "story_review.md")},
		{"develop", filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "develop.md")},
		{"commit", filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "commit.md")},
		{"merge_to_main", filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "merge_to_main.md")},
		{"review", filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "review.md")},
	}
	for _, c := range cases {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Errorf("%s: read %s: %v", c.name, c.path, err)
			continue
		}
		if !strings.Contains(string(body), "## Review policy") {
			t.Errorf("%s: body missing `## Review policy` heading (path=%s)", c.name, c.path)
		}
	}
}

// TestReviewContractBody_TerminationSentinel locks the
// recursion-termination prose in the new review.md (sty_690b1653
// AC #3): the chain stops at review — there is no reviewer of
// reviewer.
func TestReviewContractBody_TerminationSentinel(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "review.md"))
	if err != nil {
		t.Fatalf("read review.md: %v", err)
	}
	const sentinel = "no reviewer of reviewer"
	if !strings.Contains(string(body), sentinel) {
		t.Errorf("review.md missing recursion-termination sentinel %q", sentinel)
	}
}

// TestExecutionShapeContracts_NoReviewerSentinel locks the
// execution-shape policy on commit.md and merge_to_main.md: both
// must state that no reviewer is dispatched, matching their
// execution-shape role in the chain.
func TestExecutionShapeContracts_NoReviewerSentinel(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const sentinel = "No reviewer is dispatched"
	for _, name := range []string{"commit", "merge_to_main"} {
		body, err := os.ReadFile(filepath.Join(seedDir, "wksp_5b3257d1", "contracts", name+".md"))
		if err != nil {
			t.Errorf("%s: read: %v", name, err)
			continue
		}
		if !strings.Contains(string(body), sentinel) {
			t.Errorf("%s.md missing execution-shape sentinel %q", name, sentinel)
		}
	}
}

// TestRunWorkspace_RealSeedShipsFourLifecycleContracts asserts the
// workspace contracts catalogue after seeding the real
// config/seed/wksp_5b3257d1/ tree contains the four workspace
// lifecycle contracts: develop, commit, merge_to_main, review. Locks
// review.md being picked up by the workspace loader at boot.
func TestRunWorkspace_RealSeedShipsFourLifecycleContracts(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const ws = "wksp_5b3257d1"

	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	if _, err := RunWorkspace(context.Background(), docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}

	rows, err := docs.List(context.Background(), document.ListOptions{
		Type:  document.TypeContract,
		Scope: document.ScopeWorkspace,
	}, nil)
	if err != nil {
		t.Fatalf("List contracts: %v", err)
	}
	want := map[string]bool{"develop": false, "commit": false, "merge_to_main": false, "review": false}
	for _, r := range rows {
		if r.WorkspaceID != ws {
			continue
		}
		if _, tracked := want[r.Name]; tracked {
			want[r.Name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("workspace contracts catalogue missing %q after RunWorkspace on real seed", name)
		}
	}
}

// TestVersionBumpRubric_PresentInBothBodies (sty_f8d0157f) locks the
// `.version`-bump rubric clause into the loaded `commit` and
// `merge_to_main` contract rows. Exercises the seed-loader
// apply-to-store path (per pr_local_iteration) so the assertion runs
// against the row body the reviewer agent will read at dispatch,
// not raw markdown on disk. Models the precedent at L96-131.
func TestVersionBumpRubric_PresentInBothBodies(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const ws = "wksp_5b3257d1"

	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	if _, err := RunWorkspace(context.Background(), docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}

	rows, err := docs.List(context.Background(), document.ListOptions{
		Type:  document.TypeContract,
		Scope: document.ScopeWorkspace,
	}, nil)
	if err != nil {
		t.Fatalf("List contracts: %v", err)
	}

	bodies := map[string]string{}
	for _, r := range rows {
		if r.WorkspaceID != ws {
			continue
		}
		if r.Name == "commit" || r.Name == "merge_to_main" {
			bodies[r.Name] = r.Body
		}
	}
	for _, name := range []string{"commit", "merge_to_main"} {
		if _, ok := bodies[name]; !ok {
			t.Fatalf("expected loaded contract row %q in workspace %s", name, ws)
		}
	}

	commonSentinels := []string{
		"## Version bump policy",
		"[satellites-server]",
		"[satellites-client]",
		"[satellites-agent]",
		"cmd/satellites-server/**",
		"cmd/satellites-client/**",
		"cmd/satellites-agent/**",
	}
	for name, body := range bodies {
		for _, s := range commonSentinels {
			if !strings.Contains(body, s) {
				t.Errorf("%s.md loaded body missing sentinel %q", name, s)
			}
		}
	}

	// merge_to_main must scope the check across the branch
	// collectively (not per-commit at merge time), so a docs-only
	// tail commit doesn't trip the rule when an earlier commit
	// already bumped the binary.
	if mb := bodies["merge_to_main"]; !strings.Contains(mb, "collectively") && !strings.Contains(mb, "across the branch") {
		t.Errorf("merge_to_main.md loaded body missing branch-aggregate scoping sentinel (expected %q or %q)", "collectively", "across the branch")
	}
}

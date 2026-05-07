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
		{"story_close", filepath.Join(seedDir, "system", "contracts", "story_close.md")},
		{"develop", filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "develop.md")},
		{"push", filepath.Join(seedDir, "wksp_5b3257d1", "contracts", "push.md")},
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
// execution-shape policy on push.md and merge_to_main.md
// (sty_690b1653 AC #4 / AC #5): both must state that no reviewer
// is dispatched, matching their execution-shape role in the chain.
func TestExecutionShapeContracts_NoReviewerSentinel(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const sentinel = "No reviewer is dispatched"
	for _, name := range []string{"push", "merge_to_main"} {
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

// TestRunWorkspace_RealSeedShipsFourLifecycleContracts (sty_690b1653)
// asserts the workspace contracts catalogue after seeding the real
// config/seed/wksp_5b3257d1/ tree contains the four workspace
// lifecycle contracts: develop, push, merge_to_main, review. Locks
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
	want := map[string]bool{"develop": false, "push": false, "merge_to_main": false, "review": false}
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

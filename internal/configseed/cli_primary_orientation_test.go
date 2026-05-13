package configseed

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestSeedLoad_CliPrimaryOrientationDocs verifies the seed loader
// applies the load-bearing prose introduced by sty_c8a488ae across
// three surfaces, fetched from the document store after a loader pass
// on the real config/seed directory:
//
//   - system-tier agent claude_orchestrator (config/seed/system/agents/)
//   - workspace-tier principle substrate_model
//     (config/seed/wksp_5b3257d1/principles/)
//   - project-tier artifact project_intent
//     (config/seed/wksp_5b3257d1/proj_7a62aedb/artifacts/)
//
// The assertion runs against the loaded document.Body, not the file
// on disk, so a regression in the seed→store path (loader skipping
// the file, body truncated, frontmatter strip dropping content) is
// caught as well as a regression in the prose itself.
func TestSeedLoad_CliPrimaryOrientationDocs(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const (
		ws  = "wksp_5b3257d1"
		pid = "proj_7a62aedb"
	)

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	if _, err := Run(ctx, docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("Run (system tier): %v", err)
	}
	if _, err := RunWorkspace(ctx, docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}
	if _, err := RunProject(ctx, docs, seedDir, pid, ws, "system", now); err != nil {
		t.Fatalf("RunProject: %v", err)
	}

	cases := []struct {
		label    string
		fetch    func() (document.Document, error)
		required []string
	}{
		{
			label: "agent:claude_orchestrator",
			fetch: func() (document.Document, error) {
				return docs.GetByName(ctx, "", "claude_orchestrator", nil)
			},
			required: []string{
				"Two surfaces, one separation",
				"MCP = authoring surface",
				"satellites-client = execution surface",
				"satellites-client task run <task_id>",
				"Forbidden alternatives",
				"Claude Code's `Agent` tool",
				"satellites_init",
				"sty_796b8fe1",
				"sty_e0c3d615",
			},
		},
		{
			label: "principle:substrate_model",
			fetch: func() (document.Document, error) {
				return docs.GetByName(ctx, "", "substrate_model", nil)
			},
			required: []string{
				"Worked failure mode — sty_4db0e025",
				"Claude Code's `Agent` tool",
				"satellites-client task run",
				"per-task permission",
			},
		},
		{
			label: "artifact:project_intent",
			fetch: func() (document.Document, error) {
				return docs.GetByName(ctx, pid, "project_intent", nil)
			},
			required: []string{
				"Authoring vs execution",
				"MCP = authoring surface",
				"satellites-client = execution surface",
				"satellites-client task run",
				"Forbidden alternatives",
				"Claude Code's `Agent` tool",
				"satellites_init",
				"sty_796b8fe1",
				"sty_e0c3d615",
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			doc, err := tc.fetch()
			if err != nil {
				t.Fatalf("fetch %s: %v", tc.label, err)
			}
			if doc.Body == "" {
				t.Fatalf("%s: loaded body is empty", tc.label)
			}
			for _, want := range tc.required {
				if !strings.Contains(doc.Body, want) {
					t.Errorf("%s: loaded body missing required prose: %q", tc.label, want)
				}
			}
		})
	}
}

package configseed

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestSeedLoad_AsyncDispatchDocs asserts the orchestrator async-dispatch
// polling pattern (sty_7b24acec C3) is present in BOTH seed documents
// after a real seed load:
//
//   - config/seed/system/agents/claude_orchestrator.md
//   - config/seed/system/artifacts/default_agent_process.md
//
// AC1-AC5 substring assertions run against each loaded document body
// (catches both a prose regression and a seed→store regression). The
// AC2 sync check extracts each doc's `Async dispatch pattern`
// subsection and asserts byte-equality modulo the header-level
// `#` prefix on the first line — that proves the two docs carry the
// same pattern / cadence / recovery / worked-example prose.
func TestSeedLoad_AsyncDispatchDocs(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const ws = "wksp_5b3257d1"

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	if _, err := Run(ctx, docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("Run (system tier): %v", err)
	}

	required := []string{
		"Async dispatch pattern",              // AC1 subsection header
		"task run --async",                    // AC1 pattern names the flag
		"task walk --story-id",                // AC1 polling verb 1
		"ledger list --task-id",               // AC1 polling verb 2
		"kind:agent-execute-evidence",         // AC1 terminal row form
		"Default 30s",                         // AC3 cadence default
		"30-60s",                              // AC3 cadence range
		"satellites-client serve start",       // AC4 recovery step 1
		"kind:daemon-orphaned-subprocess",     // AC4 recovery step 2
		"task_add(prior_task_id=",             // AC4 recovery step 3
		"Worked example",                      // AC5 parallel-example anchor
		"two stories",                         // AC5 anchor body
		"develop slices in parallel",          // AC5 anchor body
	}

	docNames := []string{"claude_orchestrator", "default_agent_process"}
	for _, name := range docNames {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc, err := docs.GetByName(ctx, "", name, nil)
			if err != nil {
				t.Fatalf("fetch %s: %v", name, err)
			}
			if doc.Body == "" {
				t.Fatalf("%s: loaded body is empty", name)
			}
			for _, want := range required {
				if !strings.Contains(doc.Body, want) {
					t.Errorf("%s: loaded body missing required prose: %q", name, want)
				}
			}
		})
	}

	// AC2 sync check: extract each doc's `Async dispatch pattern`
	// subsection body and assert byte-equality modulo the
	// header-level `#` prefix on the first line.
	t.Run("ac2_sync", func(t *testing.T) {
		t.Parallel()
		extract := func(name string) string {
			doc, err := docs.GetByName(ctx, "", name, nil)
			if err != nil {
				t.Fatalf("fetch %s: %v", name, err)
			}
			return extractAsyncDispatchSubsection(doc.Body)
		}
		orch := extract("claude_orchestrator")
		artifact := extract("default_agent_process")
		if orch == "" {
			t.Fatalf("claude_orchestrator: Async dispatch pattern subsection not found")
		}
		if artifact == "" {
			t.Fatalf("default_agent_process: Async dispatch pattern subsection not found")
		}
		if normalizeHeaderHashes(orch) != normalizeHeaderHashes(artifact) {
			t.Errorf("AC2 sync drift: claude_orchestrator subsection body != default_agent_process subsection body (modulo header `#` prefix)")
		}
	})
}

// extractAsyncDispatchSubsection returns the literal body of the
// `Async dispatch pattern` subsection (header line included),
// stopping at the next ATX header of the same-or-shallower depth.
func extractAsyncDispatchSubsection(body string) string {
	lines := strings.Split(body, "\n")
	headerIdx := -1
	headerDepth := 0
	for i, line := range lines {
		trim := strings.TrimRight(line, " \t")
		if !strings.HasPrefix(trim, "#") {
			continue
		}
		j := 0
		for j < len(trim) && trim[j] == '#' {
			j++
		}
		rest := strings.TrimSpace(trim[j:])
		if rest == "Async dispatch pattern" {
			headerIdx = i
			headerDepth = j
			break
		}
	}
	if headerIdx < 0 {
		return ""
	}
	end := len(lines)
	for i := headerIdx + 1; i < len(lines); i++ {
		trim := strings.TrimRight(lines[i], " \t")
		if !strings.HasPrefix(trim, "#") {
			continue
		}
		j := 0
		for j < len(trim) && trim[j] == '#' {
			j++
		}
		if j <= headerDepth && j > 0 && j < len(trim) && trim[j] == ' ' {
			end = i
			break
		}
	}
	return strings.Join(lines[headerIdx:end], "\n")
}

// normalizeHeaderHashes strips leading `#` characters and surrounding
// whitespace from the FIRST line of the subsection so the `###` vs
// `####` header-level difference does not register as drift.
func normalizeHeaderHashes(section string) string {
	idx := strings.Index(section, "\n")
	if idx < 0 {
		return strings.TrimLeft(strings.TrimSpace(section), "#")
	}
	first := strings.TrimSpace(strings.TrimLeft(section[:idx], "#"))
	return first + section[idx:]
}

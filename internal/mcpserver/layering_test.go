package mcpserver

// TestTransportLayering (sty_3dc39a5c) enforces the
// pr_mcp_cli_shared_path workspace principle: transport-handler
// files in internal/mcpserver/ MUST NOT import substrate domain
// packages directly. The only allowed delegation surface is
// internal/client (plus internal/auth, internal/config,
// internal/arbor, stdlib, and the MCP-go transport library).
//
// The test walks every non-test *.go file in internal/mcpserver/,
// uses go/parser in ImportsOnly mode to enumerate each file's
// import paths, and flags any forbidden substrate import that is
// not pre-allowlisted.
//
// The legacy allowlist holds the 11 transport files that still
// import substrate packages directly post-sty_f3f7bf9b. Each entry
// names the migration story (sty_4db0e025 / order:07d). The
// allowlist shrinks to zero as 07d's per-noun convergence ships;
// new entries require explicit opt-in and a citation of this
// principle in the AC.
//
// The test also flags STALE allowlist entries (a file is
// pre-allowlisted for a substrate import it no longer uses). This
// forces 07d's per-noun convergence PRs to remove allowlist rows
// in lock-step with the import deletions — the allowlist cannot
// rot.
//
// Synthetic-violation drill is documented in review-criteria
// (ldg_52aca6ec) §3.3: inject a forbidden import into a converged
// handler, confirm the test FAILS naming the file + import; revert
// the import, confirm it PASSES.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// forbiddenSubstrateImports enumerates the substrate domain
// packages that transport handlers must reach only through
// *internal/client.Client. The list is intentionally broader than
// the set of packages currently importable from internal/mcpserver/
// — it preempts new substrate packages (e.g. internal/principle,
// internal/role, internal/skill, internal/reviewer, internal/agent,
// internal/contract) that may be carved out from internal/document
// over time.
var forbiddenSubstrateImports = []string{
	"github.com/bobmcallan/satellites/internal/document",
	"github.com/bobmcallan/satellites/internal/story",
	"github.com/bobmcallan/satellites/internal/task",
	"github.com/bobmcallan/satellites/internal/principle",
	"github.com/bobmcallan/satellites/internal/skill",
	"github.com/bobmcallan/satellites/internal/reviewer",
	"github.com/bobmcallan/satellites/internal/role",
	"github.com/bobmcallan/satellites/internal/agent",
	"github.com/bobmcallan/satellites/internal/contract",
	"github.com/bobmcallan/satellites/internal/repo",
	"github.com/bobmcallan/satellites/internal/kv",
	"github.com/bobmcallan/satellites/internal/changelog",
	"github.com/bobmcallan/satellites/internal/workspace",
	"github.com/bobmcallan/satellites/internal/project",
	"github.com/bobmcallan/satellites/internal/ledger",
	"github.com/bobmcallan/satellites/internal/portalreplicate",
	"github.com/bobmcallan/satellites/internal/agentprocess",
	"github.com/bobmcallan/satellites/internal/session",
}

// legacyAllowlist names the pre-sty_3dc39a5c transport-handler
// files that still import substrate packages directly. Each entry
// is keyed by basename and maps to the substrate import paths the
// file is permitted to retain until its noun is converged onto
// *client.Client.
//
// Cadence: each TODO(sty_4db0e025) entry is removed by the
// per-noun convergence PR in order:07d. Removing the entry in the
// same diff as the import-deletion is the explicit exit criterion
// — the stale-entry check below makes a forgotten removal a hard
// failure.
//
// Snapshot taken 2026-05-12 against c321e6a (post-sty_f3f7bf9b
// slice 12). 11 files; one-line characterisation per entry below.
var legacyAllowlist = map[string]map[string]struct{}{
	// catalogue.go — tool catalogue / help surface; reads document
	// types directly. TODO(sty_4db0e025): route through
	// client.CatalogueList.
	"catalogue.go": {
		"github.com/bobmcallan/satellites/internal/document": {},
	},
	// mcp.go — server struct + boot-time wiring; holds typed store
	// pointers for the legacy handlers below. TODO(sty_4db0e025):
	// the struct narrows to *client.Client only once every legacy
	// handler is migrated.
	"mcp.go": {
		"github.com/bobmcallan/satellites/internal/agentprocess":    {},
		"github.com/bobmcallan/satellites/internal/changelog":       {},
		"github.com/bobmcallan/satellites/internal/document":        {},
		"github.com/bobmcallan/satellites/internal/ledger":          {},
		"github.com/bobmcallan/satellites/internal/portalreplicate": {},
		"github.com/bobmcallan/satellites/internal/project":         {},
		"github.com/bobmcallan/satellites/internal/repo":            {},
		"github.com/bobmcallan/satellites/internal/session":         {},
		"github.com/bobmcallan/satellites/internal/story":           {},
		"github.com/bobmcallan/satellites/internal/task":            {},
		"github.com/bobmcallan/satellites/internal/workspace":       {},
	},
	// portal_replicate.go — wire adapter for portal_replicate;
	// keeps a portalreplicate.Vocabulary accessor on the server.
	// TODO(sty_4db0e025): move the vocabulary accessor onto
	// *client.Client so the transport file imports only client.
	"portal_replicate.go": {
		"github.com/bobmcallan/satellites/internal/portalreplicate": {},
	},
}

// TestTransportLayering enforces pr_mcp_cli_shared_path.
func TestTransportLayering(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}

	forbiddenSet := make(map[string]struct{}, len(forbiddenSubstrateImports))
	for _, p := range forbiddenSubstrateImports {
		forbiddenSet[p] = struct{}{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}

	// observedLegacy[file][import] is true once we see the file
	// actually use the legacy import. After the walk we compare
	// against legacyAllowlist to surface stale entries.
	observedLegacy := make(map[string]map[string]struct{})

	type violation struct {
		file       string
		importPath string
	}
	var violations []violation

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", name, perr)
			continue
		}
		for _, imp := range file.Imports {
			// imp.Path.Value is the quoted literal; unquote.
			pathVal := strings.Trim(imp.Path.Value, "\"")
			if _, ok := forbiddenSet[pathVal]; !ok {
				continue
			}
			// Forbidden import in this file. Is it allowlisted?
			if allowed, ok := legacyAllowlist[name]; ok {
				if _, ok2 := allowed[pathVal]; ok2 {
					if observedLegacy[name] == nil {
						observedLegacy[name] = make(map[string]struct{})
					}
					observedLegacy[name][pathVal] = struct{}{}
					continue
				}
			}
			violations = append(violations, violation{file: name, importPath: pathVal})
		}
	}

	// Sort for stable output.
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].importPath < violations[j].importPath
	})
	for _, v := range violations {
		t.Errorf("pr_mcp_cli_shared_path violation: %s imports %s — route through internal/client instead. If this is intentional and approved, add an entry to legacyAllowlist with a TODO citing the migration story.",
			v.file, v.importPath)
	}

	// Stale-allowlist check: every (file, import) pair listed in
	// legacyAllowlist must be actually present in the source. A
	// forgotten allowlist entry is itself a layering bug because it
	// hides future re-introductions.
	type stale struct {
		file       string
		importPath string
	}
	var stales []stale
	for file, imports := range legacyAllowlist {
		for imp := range imports {
			if _, ok := observedLegacy[file][imp]; !ok {
				stales = append(stales, stale{file: file, importPath: imp})
			}
		}
	}
	sort.Slice(stales, func(i, j int) bool {
		if stales[i].file != stales[j].file {
			return stales[i].file < stales[j].file
		}
		return stales[i].importPath < stales[j].importPath
	})
	for _, s := range stales {
		t.Errorf("stale legacyAllowlist entry: %s no longer imports %s — remove the entry from legacyAllowlist (it shipped via sty_4db0e025 per-noun convergence).",
			s.file, s.importPath)
	}
}

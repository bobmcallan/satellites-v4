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
	"go/ast"
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
// Sty_4db0e025 slice A11 cleared the final entry (mcp.go), so the
// allowlist is now empty. Every transport file in internal/mcpserver/
// must route substrate operations through *client.Client per
// pr_mcp_cli_shared_path; a forbidden import here surfaces as a hard
// build-time failure with no escape hatch.
var legacyAllowlist = map[string]map[string]struct{}{}

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

// TestSatellitesInitNoMapAnyPayload (sty_a4c98504) guards against
// re-introducing the field-selection bug fixed by this story. The
// MCP `satellites_init` handler must marshal the typed
// SatellitesInitOutput directly — wrapping it (or any value) in a
// `map[string]any{...}` / `map[string]interface{}{...}` literal
// passed to `json.Marshal` re-creates the dropped-fields class
// (the original eight-field map omitted `workspace_overrides` +
// `chain_coverage`).
//
// The test parses satellites_init_handlers.go in full AST mode
// (not parser.ImportsOnly) and walks handleSatellitesInit's
// function body looking for any CallExpr that resolves to
// json.Marshal (or its qualified equivalent) whose argument list
// contains a map[string]any / map[string]interface{} composite
// literal. A hit fails the test with file:line.
//
// Synthetic-violation drill is documented in review-criteria
// (ldg_23b8622a) §4: re-wrap the typed value in a map literal,
// confirm the test fails naming the call site; revert, confirm it
// passes.
func TestSatellitesInitNoMapAnyPayload(t *testing.T) {
	t.Parallel()

	const targetFunc = "handleSatellitesInit"
	const targetFile = "satellites_init_handlers.go"

	fset := token.NewFileSet()
	path, err := filepath.Abs(targetFile)
	if err != nil {
		t.Fatalf("abs %s: %v", targetFile, err)
	}
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", targetFile, err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Name == nil || fd.Name.Name != targetFunc {
			continue
		}
		fn = fd
		break
	}
	if fn == nil {
		t.Fatalf("could not locate %s in %s", targetFunc, targetFile)
	}
	if fn.Body == nil {
		t.Fatalf("%s has no body", targetFunc)
	}

	type hit struct {
		pos     token.Position
		argDesc string
	}
	var hits []hit

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isJSONMarshalCall(call.Fun) {
			return true
		}
		for _, arg := range call.Args {
			if desc, bad := mapAnyCompositeDesc(arg); bad {
				hits = append(hits, hit{pos: fset.Position(arg.Pos()), argDesc: desc})
			}
		}
		return true
	})

	for _, h := range hits {
		t.Errorf("pr_mcp_cli_shared_path / sty_a4c98504 regression: %s:%d:%d passes %s to json.Marshal — marshal the typed *client.SatellitesInitOutput directly instead.",
			targetFile, h.pos.Line, h.pos.Column, h.argDesc)
	}
}

// isJSONMarshalCall returns true when the call expression's Fun
// resolves to a top-level `json.Marshal` (i.e. a SelectorExpr whose
// X is an Ident named `json` and whose Sel is `Marshal`). Anything
// else — `json.MarshalIndent`, a re-aliased import, a shadowed local
// — does not match. The handler is the only call site we guard;
// over-matching elsewhere is intentional out-of-scope.
func isJSONMarshalCall(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel == nil || sel.Sel.Name != "Marshal" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "json"
}

// mapAnyCompositeDesc returns a non-empty description + true when
// expr is a composite literal of type `map[string]any` or
// `map[string]interface{}`. Used to flag arguments passed to
// json.Marshal that re-introduce the dropped-fields class.
func mapAnyCompositeDesc(expr ast.Expr) (string, bool) {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	mt, ok := comp.Type.(*ast.MapType)
	if !ok {
		return "", false
	}
	keyIdent, ok := mt.Key.(*ast.Ident)
	if !ok || keyIdent.Name != "string" {
		return "", false
	}
	switch v := mt.Value.(type) {
	case *ast.Ident:
		if v.Name == "any" {
			return "map[string]any composite literal", true
		}
	case *ast.InterfaceType:
		if v.Methods == nil || len(v.Methods.List) == 0 {
			return "map[string]interface{} composite literal", true
		}
	}
	return "", false
}

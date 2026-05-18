package dispatchteam

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot returns the satellites repo root inferred from the
// position of this test file. The test walks back up from the
// package directory three levels: dispatchteam -> agent -> internal -> repo.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(here))))
	return root
}

// TestExtractionComplete enforces sty_5aa20f1b AC8: the lifecycle
// telemetry helpers (emitLifecycle, runHeartbeat, appendTaskLogPointer)
// have been moved out of cmd/satellites-client/task_run.go into this
// package. The CLI must NOT redeclare them and MUST import
// internal/agent/dispatchteam.
func TestExtractionComplete(t *testing.T) {
	root := repoRoot(t)
	target := filepath.Join(root, "cmd", "satellites-client", "task_run.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.AllErrors|parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}

	forbidden := map[string]struct{}{
		"emitLifecycle":        {},
		"runHeartbeat":         {},
		"appendTaskLogPointer": {},
		"newTaskLogUploader":   {},
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Recv != nil {
			continue
		}
		if _, bad := forbidden[fd.Name.Name]; bad {
			t.Errorf("cmd/satellites-client/task_run.go still declares forbidden symbol %q — must move to internal/agent/dispatchteam", fd.Name.Name)
		}
	}

	imports := map[string]struct{}{}
	for _, imp := range file.Imports {
		v := strings.Trim(imp.Path.Value, "\"")
		imports[v] = struct{}{}
	}
	if _, ok := imports["github.com/bobmcallan/satellites/internal/agent/dispatchteam"]; !ok {
		t.Errorf("cmd/satellites-client/task_run.go does not import internal/agent/dispatchteam")
	}
}

// TestUploaderRemovedFromCLI asserts the standalone uploader file in
// cmd/satellites-client was deleted (its contents now live in this
// package). pr_no_unrequested_compat: no shim file is left behind.
func TestUploaderRemovedFromCLI(t *testing.T) {
	root := repoRoot(t)
	uploaderPath := filepath.Join(root, "cmd", "satellites-client", "task_log_uploader.go")
	if exists(t, uploaderPath) {
		t.Errorf("expected cmd/satellites-client/task_log_uploader.go to be deleted post-extraction; it still exists")
	}
}

// TestNoSubstrateImports enforces pr_mcp_cli_shared_path against
// internal/agent/dispatchteam. The package routes substrate calls
// through *cliremote.Client; it must NOT import the substrate-domain
// packages enumerated in the principle's forbidden list.
func TestNoSubstrateImports(t *testing.T) {
	pkgDir := mustPackageDir(t)
	forbidden := []string{
		"github.com/bobmcallan/satellites/internal/document",
		"github.com/bobmcallan/satellites/internal/story",
		"github.com/bobmcallan/satellites/internal/task",
		"github.com/bobmcallan/satellites/internal/principle",
		"github.com/bobmcallan/satellites/internal/skill",
		"github.com/bobmcallan/satellites/internal/reviewer",
		"github.com/bobmcallan/satellites/internal/role",
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
	forbiddenSet := map[string]struct{}{}
	for _, p := range forbidden {
		forbiddenSet[p] = struct{}{}
	}
	files := mustGoFiles(t, pkgDir)
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range parsed.Imports {
			pathVal := strings.Trim(imp.Path.Value, "\"")
			if _, bad := forbiddenSet[pathVal]; bad {
				t.Errorf("dispatchteam: %s imports forbidden substrate package %s", filepath.Base(f), pathVal)
			}
		}
	}
}

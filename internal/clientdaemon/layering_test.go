package clientdaemon

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLayering enforces pr_mcp_cli_shared_path against
// internal/clientdaemon: substrate calls travel through
// *cliremote.Client, so this package must NOT import any
// substrate-domain package directly.
func TestLayering(t *testing.T) {
	t.Parallel()
	_, here, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(here)
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	forbidden := map[string]struct{}{
		"github.com/bobmcallan/satellites/internal/document":        {},
		"github.com/bobmcallan/satellites/internal/story":           {},
		"github.com/bobmcallan/satellites/internal/task":            {},
		"github.com/bobmcallan/satellites/internal/principle":       {},
		"github.com/bobmcallan/satellites/internal/skill":           {},
		"github.com/bobmcallan/satellites/internal/reviewer":        {},
		"github.com/bobmcallan/satellites/internal/role":            {},
		"github.com/bobmcallan/satellites/internal/contract":        {},
		"github.com/bobmcallan/satellites/internal/repo":            {},
		"github.com/bobmcallan/satellites/internal/kv":              {},
		"github.com/bobmcallan/satellites/internal/changelog":       {},
		"github.com/bobmcallan/satellites/internal/workspace":       {},
		"github.com/bobmcallan/satellites/internal/project":         {},
		"github.com/bobmcallan/satellites/internal/ledger":          {},
		"github.com/bobmcallan/satellites/internal/portalreplicate": {},
		"github.com/bobmcallan/satellites/internal/agentprocess":    {},
		"github.com/bobmcallan/satellites/internal/session":         {},
	}

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
		path := filepath.Join(pkgDir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		for _, imp := range file.Imports {
			pathVal := strings.Trim(imp.Path.Value, "\"")
			if _, bad := forbidden[pathVal]; bad {
				t.Errorf("clientdaemon: %s imports forbidden substrate package %s", name, pathVal)
			}
		}
	}
}

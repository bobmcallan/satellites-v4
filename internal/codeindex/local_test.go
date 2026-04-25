package codeindex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bootstrapFixtureRepo creates a fresh git repo on disk with two
// known Go files and one README. Returns the repo path (used as the
// "git remote" for the LocalIndexer — git supports cloning from a
// local path). Skips the test if the `git` binary is unavailable so
// CI on minimal images doesn't hard-fail.
func bootstrapFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary unavailable: %v", err)
	}
	repoDir := t.TempDir()

	mustWrite(t, filepath.Join(repoDir, "foo.go"), `package fixture

// Foo is the fixture function the indexer should pick up.
func Foo() string {
	return "foo"
}

// FooBar is a second function so we can test substring search.
func FooBar(x int) int {
	return x + 1
}
`)
	mustWrite(t, filepath.Join(repoDir, "bar.go"), `package fixture

// BarType is a top-level type — proves type extraction works.
type BarType struct {
	Name string
}

// Method on BarType — receiver should appear in the symbol name.
func (b *BarType) Greet() string {
	// TODO: localize
	return "hello, " + b.Name
}

const Pi = 3.14
var GlobalCounter int
`)
	mustWrite(t, filepath.Join(repoDir, "README.md"), "# Fixture\n")

	runIn(t, repoDir, "git", "init", "-q", "-b", "main")
	runIn(t, repoDir, "git", "config", "user.email", "fixture@example.com")
	runIn(t, repoDir, "git", "config", "user.name", "Fixture")
	runIn(t, repoDir, "git", "add", ".")
	runIn(t, repoDir, "git", "commit", "-q", "-m", "fixture")

	return repoDir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runIn(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v / %s", name, strings.Join(args, " "), err, string(out))
	}
}

func newTestIndexer(t *testing.T) *LocalIndexer {
	t.Helper()
	return NewLocalIndexer(t.TempDir())
}

func indexFixture(t *testing.T, idx *LocalIndexer) (remote string, result IndexResult) {
	t.Helper()
	remote = bootstrapFixtureRepo(t)
	got, err := idx.IndexRepo(context.Background(), remote, "main")
	if err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}
	return remote, got
}

func TestLocalIndexer_IndexFixture(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	_, got := indexFixture(t, idx)

	if got.HeadSHA == "" {
		t.Errorf("HeadSHA empty after IndexRepo")
	}
	if got.FileCount < 3 {
		t.Errorf("FileCount = %d, want >=3 (foo.go, bar.go, README.md)", got.FileCount)
	}
	// foo.go: Foo, FooBar (2). bar.go: BarType, BarType.Greet, Pi, GlobalCounter (4).
	if got.SymbolCount != 6 {
		t.Errorf("SymbolCount = %d, want 6", got.SymbolCount)
	}
}

func TestLocalIndexer_SearchSymbols(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	remote, _ := indexFixture(t, idx)

	raw, err := idx.SearchSymbols(context.Background(), remote, "foo", "", "")
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	var syms []Symbol
	if err := json.Unmarshal(raw, &syms); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(syms) < 2 {
		t.Fatalf("expected 2 'foo*' funcs, got %d (%v)", len(syms), syms)
	}

	// kind=type filter must exclude funcs.
	rawType, _ := idx.SearchSymbols(context.Background(), remote, "", "type", "")
	var typeSyms []Symbol
	_ = json.Unmarshal(rawType, &typeSyms)
	if len(typeSyms) != 1 || typeSyms[0].Name != "BarType" {
		t.Fatalf("kind=type returned %v, want only BarType", typeSyms)
	}
}

func TestLocalIndexer_SearchText(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	remote, _ := indexFixture(t, idx)

	raw, err := idx.SearchText(context.Background(), remote, "TODO", "")
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	var matches []map[string]any
	if err := json.Unmarshal(raw, &matches); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected at least one TODO match, got 0")
	}
	first := matches[0]
	if !strings.Contains(first["snippet"].(string), "TODO") {
		t.Errorf("snippet missing TODO: %v", first)
	}
	if first["file"] != "bar.go" {
		t.Errorf("match file = %v, want bar.go", first["file"])
	}
}

func TestLocalIndexer_GetFileContent(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	remote, _ := indexFixture(t, idx)

	raw, err := idx.GetFileContent(context.Background(), remote, "foo.go")
	if err != nil {
		t.Fatalf("GetFileContent: %v", err)
	}
	var resp map[string]string
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp["content"], "func Foo()") {
		t.Errorf("content missing func Foo(): %s", resp["content"])
	}
	if resp["path"] != "foo.go" {
		t.Errorf("path = %q, want foo.go", resp["path"])
	}
}

func TestLocalIndexer_GetFileOutline(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	remote, _ := indexFixture(t, idx)

	raw, err := idx.GetFileOutline(context.Background(), remote, "bar.go")
	if err != nil {
		t.Fatalf("GetFileOutline: %v", err)
	}
	var resp struct {
		Path    string   `json:"path"`
		Symbols []Symbol `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Path != "bar.go" {
		t.Errorf("path = %q, want bar.go", resp.Path)
	}
	// bar.go expected: BarType, BarType.Greet, Pi, GlobalCounter
	if len(resp.Symbols) != 4 {
		t.Fatalf("outline len = %d, want 4: %+v", len(resp.Symbols), resp.Symbols)
	}
}

func TestLocalIndexer_RepoNotIndexed(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	_, err := idx.SearchSymbols(context.Background(), "git@host:nope.git", "x", "", "")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for un-indexed repo, got %v", err)
	}
}

func TestLocalIndexer_GetSymbolSource(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	remote, _ := indexFixture(t, idx)

	syms, err := idx.SearchSymbols(context.Background(), remote, "Foo", "func", "go")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var list []Symbol
	_ = json.Unmarshal(syms, &list)
	if len(list) == 0 {
		t.Fatalf("no Foo symbols")
	}
	target := list[0]

	raw, err := idx.GetSymbolSource(context.Background(), remote, target.ID)
	if err != nil {
		t.Fatalf("GetSymbolSource: %v", err)
	}
	var resp struct {
		Symbol Symbol `json:"symbol"`
		Source string `json:"source"`
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.Source == "" {
		t.Errorf("source empty for %s", target.ID)
	}
	if resp.Symbol.ID != target.ID {
		t.Errorf("symbol.ID = %q, want %q", resp.Symbol.ID, target.ID)
	}
}

func TestLocalIndexer_ReindexIsIdempotent(t *testing.T) {
	t.Parallel()
	idx := newTestIndexer(t)
	remote, first := indexFixture(t, idx)

	second, err := idx.IndexRepo(context.Background(), remote, "main")
	if err != nil {
		t.Fatalf("second IndexRepo: %v", err)
	}
	if second.HeadSHA != first.HeadSHA {
		t.Errorf("HeadSHA changed across reindex without commits: %q → %q", first.HeadSHA, second.HeadSHA)
	}
	if second.SymbolCount != first.SymbolCount {
		t.Errorf("SymbolCount drift: %d → %d", first.SymbolCount, second.SymbolCount)
	}
}

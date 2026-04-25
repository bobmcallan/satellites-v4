package codeindex

import (
	"context"
	"encoding/json"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// LocalIndexer is the production Indexer implementation. It clones
// each tracked repo into a workdir on the satellites server and
// answers symbol/text/file queries directly off the working tree.
//
// The index is in-memory per process: a fresh server boot triggers
// re-indexing on the first IndexRepo call for each repo. Persistence
// is intentionally out of scope for slice 12.5; see story_75a371c7's
// implementation note for the trade-off.
type LocalIndexer struct {
	workdir string

	mu    sync.Mutex
	repos map[string]*indexedRepo // key = git_remote
}

type indexedRepo struct {
	gitRemote  string
	cloneDir   string
	headSHA    string
	files      []string // workdir-relative paths (forward slash)
	symbols    []Symbol
	symbolByID map[string]Symbol
}

// NewLocalIndexer constructs a LocalIndexer rooted at workdir. The
// workdir is created if missing.
func NewLocalIndexer(workdir string) *LocalIndexer {
	return &LocalIndexer{
		workdir: workdir,
		repos:   make(map[string]*indexedRepo),
	}
}

// IndexRepo implements Indexer.
func (l *LocalIndexer) IndexRepo(ctx context.Context, gitRemote, defaultBranch string) (IndexResult, error) {
	if gitRemote == "" {
		return IndexResult{}, &UnavailableError{Op: "index_repo", Err: fmt.Errorf("git_remote empty")}
	}
	if err := os.MkdirAll(l.workdir, 0o755); err != nil {
		return IndexResult{}, &UnavailableError{Op: "index_repo", Err: err}
	}
	cloneDir := filepath.Join(l.workdir, slugify(gitRemote))
	branch := defaultBranch
	if branch == "" {
		branch = "main"
	}
	if err := cloneOrFetch(ctx, gitRemote, branch, cloneDir); err != nil {
		return IndexResult{}, &UnavailableError{Op: "index_repo", Err: err}
	}

	headSHA, err := gitRevParseHead(ctx, cloneDir)
	if err != nil {
		return IndexResult{}, &UnavailableError{Op: "index_repo", Err: err}
	}

	files, err := walkRepo(cloneDir)
	if err != nil {
		return IndexResult{}, &UnavailableError{Op: "index_repo", Err: err}
	}

	fset := token.NewFileSet()
	symbols := make([]Symbol, 0, 64)
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		abs := filepath.Join(cloneDir, f)
		got, perr := parseGoFile(fset, abs, f)
		if perr != nil {
			// Half-broken Go file — skip without aborting the index pass.
			continue
		}
		symbols = append(symbols, got...)
	}

	byID := make(map[string]Symbol, len(symbols))
	for _, s := range symbols {
		byID[s.ID] = s
	}

	l.mu.Lock()
	l.repos[gitRemote] = &indexedRepo{
		gitRemote:  gitRemote,
		cloneDir:   cloneDir,
		headSHA:    headSHA,
		files:      files,
		symbols:    symbols,
		symbolByID: byID,
	}
	l.mu.Unlock()

	return IndexResult{
		HeadSHA:     headSHA,
		SymbolCount: len(symbols),
		FileCount:   len(files),
	}, nil
}

// ListRepos implements Indexer.
func (l *LocalIndexer) ListRepos(ctx context.Context) ([]IndexedRepo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]IndexedRepo, 0, len(l.repos))
	for _, r := range l.repos {
		out = append(out, IndexedRepo{
			GitRemote:   r.gitRemote,
			HeadSHA:     r.headSHA,
			SymbolCount: len(r.symbols),
			FileCount:   len(r.files),
		})
	}
	return out, nil
}

// SearchSymbols implements Indexer.
func (l *LocalIndexer) SearchSymbols(ctx context.Context, repoKey, query, kind, language string) (json.RawMessage, error) {
	repo, err := l.lookupRepo(repoKey, "search_symbols")
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	out := make([]Symbol, 0)
	for _, s := range repo.symbols {
		if q != "" && !strings.Contains(strings.ToLower(s.Name), q) {
			continue
		}
		if kind != "" && s.Kind != kind {
			continue
		}
		if language != "" && s.Language != language {
			continue
		}
		out = append(out, s)
	}
	return jsonMarshal(out)
}

// SearchText implements Indexer.
func (l *LocalIndexer) SearchText(ctx context.Context, repoKey, query, filePattern string) (json.RawMessage, error) {
	repo, err := l.lookupRepo(repoKey, "search_text")
	if err != nil {
		return nil, err
	}
	if query == "" {
		return jsonMarshal([]any{})
	}
	re, rerr := regexp.Compile(query)
	if rerr != nil {
		return nil, &UnavailableError{Op: "search_text", Err: rerr}
	}
	type match struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Snippet string `json:"snippet"`
	}
	out := make([]match, 0)
	for _, f := range repo.files {
		if filePattern != "" {
			ok, perr := filepath.Match(filePattern, f)
			if perr != nil || !ok {
				continue
			}
		}
		body, rerr := os.ReadFile(filepath.Join(repo.cloneDir, f))
		if rerr != nil {
			continue
		}
		for i, line := range strings.Split(string(body), "\n") {
			if re.MatchString(line) {
				out = append(out, match{File: f, Line: i + 1, Snippet: strings.TrimSpace(line)})
			}
		}
	}
	return jsonMarshal(out)
}

// GetSymbolSource implements Indexer.
func (l *LocalIndexer) GetSymbolSource(ctx context.Context, repoKey, symbolID string) (json.RawMessage, error) {
	repo, err := l.lookupRepo(repoKey, "get_symbol_source")
	if err != nil {
		return nil, err
	}
	sym, ok := repo.symbolByID[symbolID]
	if !ok {
		return nil, &UnavailableError{Op: "get_symbol_source", Err: fmt.Errorf("symbol %q not found", symbolID)}
	}
	body, rerr := os.ReadFile(filepath.Join(repo.cloneDir, sym.File))
	if rerr != nil {
		return nil, &UnavailableError{Op: "get_symbol_source", Err: rerr}
	}
	lines := strings.Split(string(body), "\n")
	start := sym.StartLine - 1
	end := sym.EndLine
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	source := strings.Join(lines[start:end], "\n")
	return jsonMarshal(map[string]any{
		"symbol": sym,
		"source": source,
	})
}

// GetFileContent implements Indexer.
func (l *LocalIndexer) GetFileContent(ctx context.Context, repoKey, path string) (json.RawMessage, error) {
	repo, err := l.lookupRepo(repoKey, "get_file_content")
	if err != nil {
		return nil, err
	}
	body, rerr := os.ReadFile(filepath.Join(repo.cloneDir, path))
	if rerr != nil {
		return nil, &UnavailableError{Op: "get_file_content", Err: rerr}
	}
	return jsonMarshal(map[string]any{
		"path":    path,
		"content": string(body),
	})
}

// GetFileOutline implements Indexer.
func (l *LocalIndexer) GetFileOutline(ctx context.Context, repoKey, path string) (json.RawMessage, error) {
	repo, err := l.lookupRepo(repoKey, "get_file_outline")
	if err != nil {
		return nil, err
	}
	out := make([]Symbol, 0)
	for _, s := range repo.symbols {
		if s.File == path {
			out = append(out, s)
		}
	}
	return jsonMarshal(map[string]any{
		"path":    path,
		"symbols": out,
	})
}

func (l *LocalIndexer) lookupRepo(repoKey, op string) (*indexedRepo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	repo, ok := l.repos[repoKey]
	if !ok {
		return nil, &UnavailableError{Op: op, Err: fmt.Errorf("repo %q not indexed", repoKey)}
	}
	return repo, nil
}

// cloneOrFetch ensures cloneDir contains the requested branch HEAD.
// First call clones; subsequent calls fetch and reset.
func cloneOrFetch(ctx context.Context, gitRemote, branch, cloneDir string) error {
	gitDir := filepath.Join(cloneDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", branch, gitRemote, cloneDir)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			return fmt.Errorf("git clone: %w (%s)", runErr, strings.TrimSpace(string(out)))
		}
		return nil
	}
	fetchCmd := exec.CommandContext(ctx, "git", "-C", cloneDir, "fetch", "--depth", "1", "origin", branch)
	if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
		return fmt.Errorf("git fetch: %w (%s)", ferr, strings.TrimSpace(string(out)))
	}
	resetCmd := exec.CommandContext(ctx, "git", "-C", cloneDir, "reset", "--hard", "origin/"+branch)
	if out, rerr := resetCmd.CombinedOutput(); rerr != nil {
		return fmt.Errorf("git reset: %w (%s)", rerr, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitRevParseHead returns the current HEAD SHA of the clone dir.
func gitRevParseHead(ctx context.Context, cloneDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", cloneDir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// walkRepo lists every regular file in cloneDir, returning paths
// relative to cloneDir with forward-slash separators. Skips .git/.
func walkRepo(cloneDir string) ([]string, error) {
	files := make([]string, 0)
	err := filepath.Walk(cloneDir, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(cloneDir, p)
		if rerr != nil {
			return rerr
		}
		if info.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// slugify turns a git remote URL into a filesystem-safe directory name.
// The slug is not required to be reversible; callers store the
// gitRemote separately on indexedRepo.
func slugify(remote string) string {
	out := make([]byte, 0, len(remote))
	for i := 0; i < len(remote); i++ {
		c := remote[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func jsonMarshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return json.RawMessage(b), nil
}

// Compile-time assertion.
var _ Indexer = (*LocalIndexer)(nil)

// Package codeindex is the satellites-internal code indexer.
// Claude online (claude.ai/code) does not have an external code-index
// service available; the symbol / text / file-content surface required
// by repo_* MCP verbs must be served by satellites itself.
//
// The Indexer interface preserves the read shape of the prior proxy
// so the slice 12.2 handlers + slice 12.3 reindex worker re-bind to
// it without changing their AC. The production LocalIndexer clones
// each tracked repo into a workdir on the satellites server, parses
// Go source via `go/parser` for symbol extraction, and serves text /
// outline queries directly off the working tree.
package codeindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Symbol is one indexed top-level declaration (function, type,
// constant, variable). Slice 12.5 covers Go only — multi-language
// extraction is left to a follow-up.
type Symbol struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Language  string `json:"language"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Signature string `json:"signature,omitempty"`
}

// IndexResult is the structured outcome the reindex worker writes onto
// the repo row via UpdateIndexState.
type IndexResult struct {
	HeadSHA     string `json:"head_sha"`
	SymbolCount int    `json:"symbol_count"`
	FileCount   int    `json:"file_count"`
}

// IndexedRepo is the per-repo summary returned by ListRepos.
type IndexedRepo struct {
	GitRemote   string `json:"git_remote"`
	HeadSHA     string `json:"head_sha"`
	SymbolCount int    `json:"symbol_count"`
	FileCount   int    `json:"file_count"`
}

// Indexer is the surface MCP handlers + the reindex worker depend on.
// Implementations MUST return *UnavailableError for transport-level or
// clone failures so handlers can translate to a structured
// `code_index_unavailable` error rather than a 500.
type Indexer interface {
	IndexRepo(ctx context.Context, gitRemote, defaultBranch string) (IndexResult, error)
	ListRepos(ctx context.Context) ([]IndexedRepo, error)
	SearchSymbols(ctx context.Context, repoKey, query, kind, language string) (json.RawMessage, error)
	SearchText(ctx context.Context, repoKey, query, filePattern string) (json.RawMessage, error)
	GetSymbolSource(ctx context.Context, repoKey, symbolID string) (json.RawMessage, error)
	GetFileContent(ctx context.Context, repoKey, path string) (json.RawMessage, error)
	GetFileOutline(ctx context.Context, repoKey, path string) (json.RawMessage, error)
}

// UnavailableError is the typed error every Indexer method MUST surface
// when the indexer cannot serve the request (clone failure, missing
// repo, parser unavailable). Handlers compare with errors.Is to
// translate the failure to a structured MCP `code_index_unavailable`
// response.
type UnavailableError struct {
	Op  string
	Err error
}

// Error implements error.
func (e *UnavailableError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("code index unavailable: %s", e.Op)
	}
	return fmt.Sprintf("code index unavailable: %s: %v", e.Op, e.Err)
}

// Unwrap exposes the underlying transport error to errors.Is/As callers.
func (e *UnavailableError) Unwrap() error { return e.Err }

// ErrUnavailable is a sentinel that satisfies errors.Is(err, ErrUnavailable)
// for any *UnavailableError. Handlers check this rather than asserting
// the concrete type.
var ErrUnavailable = errors.New("code index unavailable")

// Is implements the errors.Is contract so any *UnavailableError matches
// the ErrUnavailable sentinel.
func (e *UnavailableError) Is(target error) bool {
	return target == ErrUnavailable
}

// Stub is the always-unavailable Indexer used as the fallback when the
// MCP server is constructed without a configured indexer (e.g. in test
// fixtures that exercise only the error-translation path). Production
// always wires LocalIndexer.
type Stub struct{}

// NewStub returns the always-unavailable indexer.
func NewStub() *Stub { return &Stub{} }

func (s *Stub) unavailable(op string) *UnavailableError {
	return &UnavailableError{Op: op}
}

// IndexRepo implements Indexer for Stub.
func (s *Stub) IndexRepo(ctx context.Context, gitRemote, defaultBranch string) (IndexResult, error) {
	return IndexResult{}, s.unavailable("index_repo")
}

// ListRepos implements Indexer for Stub.
func (s *Stub) ListRepos(ctx context.Context) ([]IndexedRepo, error) {
	return nil, s.unavailable("list_repos")
}

// SearchSymbols implements Indexer for Stub.
func (s *Stub) SearchSymbols(ctx context.Context, repoKey, query, kind, language string) (json.RawMessage, error) {
	return nil, s.unavailable("search_symbols")
}

// SearchText implements Indexer for Stub.
func (s *Stub) SearchText(ctx context.Context, repoKey, query, filePattern string) (json.RawMessage, error) {
	return nil, s.unavailable("search_text")
}

// GetSymbolSource implements Indexer for Stub.
func (s *Stub) GetSymbolSource(ctx context.Context, repoKey, symbolID string) (json.RawMessage, error) {
	return nil, s.unavailable("get_symbol_source")
}

// GetFileContent implements Indexer for Stub.
func (s *Stub) GetFileContent(ctx context.Context, repoKey, path string) (json.RawMessage, error) {
	return nil, s.unavailable("get_file_content")
}

// GetFileOutline implements Indexer for Stub.
func (s *Stub) GetFileOutline(ctx context.Context, repoKey, path string) (json.RawMessage, error) {
	return nil, s.unavailable("get_file_outline")
}

// Compile-time assertion.
var _ Indexer = (*Stub)(nil)

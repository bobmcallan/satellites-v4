// Package jcodemunch is the satellites-side proxy to the jcodemunch
// semantic-index service. The interface is the surface satellites
// handlers depend on; production deployments wire a real HTTP/MCP
// adapter, while unit tests inject a Stub that always reports the
// service unavailable so the handlers' error path stays exercised.
//
// Per docs/architecture.md §7 ("Repo + index"), the satellites server
// keeps only the repo pointer (id, git_remote, head_sha, …) and forwards
// query verbs to jcodemunch. The proxy keys are jcodemunch's own
// identifiers — handlers translate the satellites-facing repo_id to a
// stable proxy key (currently the git_remote) before each call so the
// jcodemunch identifier never reaches the caller.
package jcodemunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// IndexResult is the structured outcome of a successful index pass per
// the §7 "Index regeneration" protocol. Reindex callers read these
// fields and write them onto the repo row via UpdateIndexState.
type IndexResult struct {
	HeadSHA     string `json:"head_sha"`
	SymbolCount int    `json:"symbol_count"`
	FileCount   int    `json:"file_count"`
	IndexKey    string `json:"index_key,omitempty"`
}

// IndexedRepo is the per-repo summary returned by ListRepos. Slice 12.3
// uses it to refresh symbol/file counts after a reindex.
type IndexedRepo struct {
	GitRemote   string `json:"git_remote"`
	HeadSHA     string `json:"head_sha"`
	SymbolCount int    `json:"symbol_count"`
	FileCount   int    `json:"file_count"`
}

// Client is the surface satellites handlers depend on. Implementations
// must translate transport-level failures (HTTP 5xx, connection refused,
// timeouts) into a *UnavailableError so handlers can return a structured
// MCP error rather than a 500 — the AC for slice 12.2 is explicit on
// this point.
type Client interface {
	// IndexRepo runs (or re-runs) the symbol/text index on the given
	// remote. Used by slice 12.3's reindex worker.
	IndexRepo(ctx context.Context, gitRemote, defaultBranch string) (IndexResult, error)
	// ListRepos returns jcodemunch's view of every indexed repo it knows.
	ListRepos(ctx context.Context) ([]IndexedRepo, error)
	// SearchSymbols proxies jcodemunch__search_symbols.
	SearchSymbols(ctx context.Context, repoKey, query, kind, language string) (json.RawMessage, error)
	// SearchText proxies jcodemunch__search_text.
	SearchText(ctx context.Context, repoKey, query, filePattern string) (json.RawMessage, error)
	// GetSymbolSource proxies jcodemunch__get_symbol_source.
	GetSymbolSource(ctx context.Context, repoKey, symbolID string) (json.RawMessage, error)
	// GetFileContent proxies jcodemunch__get_file_content.
	GetFileContent(ctx context.Context, repoKey, path string) (json.RawMessage, error)
	// GetFileOutline proxies jcodemunch__get_file_outline.
	GetFileOutline(ctx context.Context, repoKey, path string) (json.RawMessage, error)
}

// UnavailableError is the typed error every client method MUST surface
// when jcodemunch cannot be reached or returns a transport-level
// failure. Handlers compare with errors.Is to translate the failure to
// a structured MCP `jcodemunch_unavailable` response.
type UnavailableError struct {
	Op  string
	Err error
}

// Error implements error.
func (e *UnavailableError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("jcodemunch unavailable: %s", e.Op)
	}
	return fmt.Sprintf("jcodemunch unavailable: %s: %v", e.Op, e.Err)
}

// Unwrap exposes the underlying transport error to errors.Is/As callers.
func (e *UnavailableError) Unwrap() error { return e.Err }

// ErrUnavailable is a sentinel that satisfies errors.Is(err, ErrUnavailable)
// for any *UnavailableError. Handlers check this rather than asserting
// the concrete type.
var ErrUnavailable = errors.New("jcodemunch unavailable")

// Is implements the errors.Is contract so any *UnavailableError matches
// the ErrUnavailable sentinel.
func (e *UnavailableError) Is(target error) bool {
	return target == ErrUnavailable
}

// Stub is the default Client wired into the MCP server when no real
// adapter is configured. Every method returns *UnavailableError so the
// handlers' "jcodemunch timeout → structured error" path is always
// exercised. Tests rely on this shape; production deployments override
// it via Deps.JcodemunchClient.
type Stub struct{}

// NewStub returns the always-unavailable client.
func NewStub() *Stub { return &Stub{} }

func (s *Stub) unavailable(op string) *UnavailableError {
	return &UnavailableError{Op: op}
}

// IndexRepo implements Client for Stub.
func (s *Stub) IndexRepo(ctx context.Context, gitRemote, defaultBranch string) (IndexResult, error) {
	return IndexResult{}, s.unavailable("index_repo")
}

// ListRepos implements Client for Stub.
func (s *Stub) ListRepos(ctx context.Context) ([]IndexedRepo, error) {
	return nil, s.unavailable("list_repos")
}

// SearchSymbols implements Client for Stub.
func (s *Stub) SearchSymbols(ctx context.Context, repoKey, query, kind, language string) (json.RawMessage, error) {
	return nil, s.unavailable("search_symbols")
}

// SearchText implements Client for Stub.
func (s *Stub) SearchText(ctx context.Context, repoKey, query, filePattern string) (json.RawMessage, error) {
	return nil, s.unavailable("search_text")
}

// GetSymbolSource implements Client for Stub.
func (s *Stub) GetSymbolSource(ctx context.Context, repoKey, symbolID string) (json.RawMessage, error) {
	return nil, s.unavailable("get_symbol_source")
}

// GetFileContent implements Client for Stub.
func (s *Stub) GetFileContent(ctx context.Context, repoKey, path string) (json.RawMessage, error) {
	return nil, s.unavailable("get_file_content")
}

// GetFileOutline implements Client for Stub.
func (s *Stub) GetFileOutline(ctx context.Context, repoKey, path string) (json.RawMessage, error) {
	return nil, s.unavailable("get_file_outline")
}

// Compile-time assertion.
var _ Client = (*Stub)(nil)

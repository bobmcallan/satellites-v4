package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/codeindex"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// handleRepoAdd implements `repo_add`. Thin forwarder onto
// client.RepoAdd; sty_14dfd05b canonicalisation + dedup + audit row
// live on the typed surface. sty_509a46fa removed the post-create
// reindex enqueue when the reindex worker pipeline was retired.
func (s *Server) handleRepoAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().RepoAdd(ctx, toClientCallerWith(caller, memberships), client.RepoAddInput{
		RawRemote:          req.GetString("git_remote", ""),
		DefaultBranch:      req.GetString("default_branch", "main"),
		RequestedProjectID: req.GetString("project_id", ""),
		ScopedProjectID:    ScopedProjectIDFrom(ctx),
		Memberships:        memberships,
		Now:                s.nowUTC(),
	})
	if err != nil {
		return mcpgo.NewToolResultError(repoErrMessage("repo_add", err)), nil
	}
	return jsonResult(out)
}

// handleRepoGet implements `repo_get`. Workspace-scoped via
// memberships; delegates to client.RepoGet.
func (s *Server) handleRepoGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	r, err := s.cli().RepoGet(ctx, toClientCallerWith(caller, memberships), client.RepoGetInput{
		RepoID:      req.GetString("repo_id", ""),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(repoErrMessage("repo_get", err)), nil
	}
	return jsonResult(r)
}

// handleRepoList implements `repo_list`. Default `status=active`;
// pass status="archived" to surface archived rows; status="all"
// returns both. Delegates to client.RepoList.
func (s *Server) handleRepoList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().RepoList(ctx, toClientCallerWith(caller, memberships), client.RepoListInput{
		RequestedProjectID: req.GetString("project_id", ""),
		ScopedProjectID:    ScopedProjectIDFrom(ctx),
		StatusFilter:       req.GetString("status", ""),
		Memberships:        memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(repoErrMessage("repo_list", err)), nil
	}
	return jsonResult(out)
}

// handleRepoSearch implements `repo_search` — thin forwarder onto
// client.RepoSearch (audit row + indexer proxy live on the typed
// surface).
func (s *Server) handleRepoSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	raw, err := s.cli().RepoSearch(ctx, toClientCallerWith(caller, memberships), client.RepoProxyInput{
		RepoID:      req.GetString("repo_id", ""),
		Query:       req.GetString("query", ""),
		Kind:        req.GetString("kind", ""),
		Language:    req.GetString("language", ""),
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
	return repoProxyResult("search", raw, err), nil
}

// handleRepoSearchText implements `repo_search_text`.
func (s *Server) handleRepoSearchText(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	raw, err := s.cli().RepoSearchText(ctx, toClientCallerWith(caller, memberships), client.RepoProxyInput{
		RepoID:      req.GetString("repo_id", ""),
		Query:       req.GetString("query", ""),
		FilePattern: req.GetString("file_pattern", ""),
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
	return repoProxyResult("search_text", raw, err), nil
}

// handleRepoGetSymbolSource implements `repo_get_symbol_source`.
func (s *Server) handleRepoGetSymbolSource(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	raw, err := s.cli().RepoGetSymbolSource(ctx, toClientCallerWith(caller, memberships), client.RepoProxyInput{
		RepoID:      req.GetString("repo_id", ""),
		SymbolID:    req.GetString("symbol_id", ""),
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
	return repoProxyResult("get_symbol_source", raw, err), nil
}

// handleRepoGetFile implements `repo_get_file`.
func (s *Server) handleRepoGetFile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	raw, err := s.cli().RepoGetFile(ctx, toClientCallerWith(caller, memberships), client.RepoProxyInput{
		RepoID:      req.GetString("repo_id", ""),
		Path:        req.GetString("path", ""),
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
	return repoProxyResult("get_file", raw, err), nil
}

// handleRepoGetOutline implements `repo_get_outline`.
func (s *Server) handleRepoGetOutline(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	raw, err := s.cli().RepoGetOutline(ctx, toClientCallerWith(caller, memberships), client.RepoProxyInput{
		RepoID:      req.GetString("repo_id", ""),
		Path:        req.GetString("path", ""),
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
	return repoProxyResult("get_outline", raw, err), nil
}

// toClientCallerWith threads the resolved memberships slice into the
// caller bundle. sty_f3f7bf9b slice 4: the repo verbs all run after
// resolveCallerMemberships, so they need a caller carrying that slice
// for downstream ResolveProjectID checks.
func toClientCallerWith(c auth.CallerIdentity, memberships []string) client.Caller {
	cc := toClientCaller(c)
	cc.Memberships = memberships
	return cc
}

// repoProxyResult is the shared wire-format adapter for the five
// indexer-proxy verbs. Forwards a successful payload via
// NewToolResultText (the indexer returns raw JSON); on error,
// translates code-index sentinels into structured envelopes.
func repoProxyResult(op string, raw json.RawMessage, err error) *mcpgo.CallToolResult {
	if err != nil {
		return repoProxyError(op, err)
	}
	return mcpgo.NewToolResultText(string(raw))
}

// repoProxyError unwraps typed client sentinels first (e.g.
// ErrRepoIDRequired) so the wire envelope matches the historical
// shape, then falls back to indexerErrorResult for codeindex faults.
func repoProxyError(op string, err error) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, client.ErrRepoStoreNotConfigured),
		errors.Is(err, client.ErrRepoIDRequired),
		errors.Is(err, client.ErrRepoQueryRequired),
		errors.Is(err, client.ErrRepoSymbolIDRequired),
		errors.Is(err, client.ErrRepoPathRequired):
		return mcpgo.NewToolResultError(repoErrMessage(op, err))
	}
	return indexerErrorResult(op, err)
}

// repoErrMessage maps typed repo sentinels to the historical wire
// envelopes the pre-extraction handlers emitted. Centralises the
// mapping so each adapter stays inside the ≤25-line ceiling per
// sty_f3f7bf9b's review-criteria.
func repoErrMessage(op string, err error) string {
	switch {
	case errors.Is(err, client.ErrRepoStoreNotConfigured):
		return op + " unavailable: repo store not configured"
	case errors.Is(err, client.ErrGitRemoteRequired):
		return "git_remote required"
	case errors.Is(err, client.ErrGitRemoteInvalid):
		return "git_remote_invalid"
	case errors.Is(err, client.ErrRepoIDRequired):
		return "repo_id required"
	case errors.Is(err, client.ErrRepoQueryRequired):
		return "query required"
	case errors.Is(err, client.ErrRepoSymbolIDRequired):
		return "symbol_id required"
	case errors.Is(err, client.ErrRepoPathRequired):
		return "path required"
	}
	return err.Error()
}

// indexerErrorResult translates a code-index failure into a structured
// MCP error result. errors.Is(err, codeindex.ErrUnavailable) produces
// the documented `code_index_unavailable` envelope; anything else is
// wrapped as a plain error string. Story_75a371c7 replaced the prior
// jcodemunch shape.
func indexerErrorResult(op string, err error) *mcpgo.CallToolResult {
	if errors.Is(err, codeindex.ErrUnavailable) {
		body, _ := json.Marshal(map[string]any{
			"error":  "code_index_unavailable",
			"op":     op,
			"detail": err.Error(),
		})
		return mcpgo.NewToolResultError(string(body))
	}
	return mcpgo.NewToolResultError(fmt.Sprintf("%s: %s", op, err))
}

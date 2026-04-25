package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/task"
)

// reindexPayload is the JSON envelope written to task.Payload when a
// repo_add or repo_scan enqueues a reindex. The slice 12.3 worker keys
// off `handler == "reindex_repo"` and the repo_id.
type reindexPayload struct {
	Handler   string `json:"handler"`
	RepoID    string `json:"repo_id"`
	GitRemote string `json:"git_remote"`
	Trigger   string `json:"trigger"`
}

// handleRepoAdd implements `repo_add`. Resolves the caller's project,
// dedups against (workspace, git_remote), creates the row, enqueues a
// reindex task, and writes a kind:repo-added audit row.
func (s *Server) handleRepoAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.repos == nil {
		return mcpgo.NewToolResultError("repo_add unavailable: repo store not configured"), nil
	}
	caller, _ := UserFrom(ctx)
	gitRemote, err := req.RequireString("git_remote")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	defaultBranch := req.GetString("default_branch", "main")
	memberships := s.resolveCallerMemberships(ctx, caller)
	projectID, err := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveProjectWorkspaceID(ctx, projectID)
	now := time.Now().UTC()

	if existing, err := s.repos.GetByRemote(ctx, wsID, gitRemote); err == nil {
		return jsonResult(map[string]any{
			"repo_id":        existing.ID,
			"deduplicated":   true,
			"git_remote":     existing.GitRemote,
			"default_branch": existing.DefaultBranch,
		})
	} else if !errors.Is(err, repo.ErrNotFound) {
		return mcpgo.NewToolResultError(fmt.Sprintf("repo_add: dedup probe: %s", err)), nil
	}

	created, err := s.repos.Create(ctx, repo.Repo{
		WorkspaceID:   wsID,
		ProjectID:     projectID,
		GitRemote:     gitRemote,
		DefaultBranch: defaultBranch,
		Status:        repo.StatusActive,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("repo_add: %s", err)), nil
	}

	taskID := s.enqueueReindex(ctx, created, "repo_add", caller.UserID, now)
	s.appendRepoAuditRow(ctx, created, "kind:repo-added", caller.UserID, map[string]any{
		"git_remote":     created.GitRemote,
		"default_branch": created.DefaultBranch,
	}, now)

	return jsonResult(map[string]any{
		"repo_id":        created.ID,
		"task_id":        taskID,
		"deduplicated":   false,
		"git_remote":     created.GitRemote,
		"default_branch": created.DefaultBranch,
	})
}

// handleRepoGet implements `repo_get`. Workspace-scoped via memberships.
func (s *Server) handleRepoGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.repos == nil {
		return mcpgo.NewToolResultError("repo_get unavailable"), nil
	}
	caller, _ := UserFrom(ctx)
	repoID, err := req.RequireString("repo_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	r, err := s.repos.GetByID(ctx, repoID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(r)
}

// handleRepoList implements `repo_list`. Default `status=active`; pass
// status="archived" to surface archived rows; status="all" returns both.
func (s *Server) handleRepoList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.repos == nil {
		return mcpgo.NewToolResultError("repo_list unavailable"), nil
	}
	caller, _ := UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	projectID, err := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	statusFilter := req.GetString("status", repo.StatusActive)

	rows, err := s.repos.List(ctx, projectID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if statusFilter != "all" {
		filtered := make([]repo.Repo, 0, len(rows))
		for _, r := range rows {
			if r.Status == statusFilter {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	return jsonResult(map[string]any{
		"project_id": projectID,
		"status":     statusFilter,
		"repos":      rows,
	})
}

// handleRepoScan implements `repo_scan`. Idempotent: if a reindex task
// for the repo is already enqueued/claimed/in_flight the existing
// task_id is returned with deduplicated=true.
func (s *Server) handleRepoScan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.repos == nil {
		return mcpgo.NewToolResultError("repo_scan unavailable"), nil
	}
	if s.tasks == nil {
		return mcpgo.NewToolResultError("repo_scan unavailable: task store not configured"), nil
	}
	caller, _ := UserFrom(ctx)
	repoID, err := req.RequireString("repo_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	r, err := s.repos.GetByID(ctx, repoID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	if existingID := s.findInFlightReindex(ctx, r, memberships); existingID != "" {
		return jsonResult(map[string]any{
			"task_id":      existingID,
			"deduplicated": true,
			"repo_id":      r.ID,
		})
	}

	now := time.Now().UTC()
	taskID := s.enqueueReindex(ctx, r, "repo_scan", caller.UserID, now)
	if taskID == "" {
		return mcpgo.NewToolResultError("repo_scan: task enqueue failed"), nil
	}
	s.appendRepoAuditRow(ctx, r, "kind:repo-scan-enqueued", caller.UserID, map[string]any{
		"task_id": taskID,
		"trigger": "manual",
	}, now)
	return jsonResult(map[string]any{
		"task_id":      taskID,
		"repo_id":      r.ID,
		"deduplicated": false,
	})
}

// handleRepoSearch implements `repo_search` — proxy to
// jcodemunch__search_symbols + kind:repo-query audit row.
func (s *Server) handleRepoSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := s.resolveRepoForProxy(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	args := req.GetArguments()
	query, qerr := req.RequireString("query")
	if qerr != nil {
		return mcpgo.NewToolResultError(qerr.Error()), nil
	}
	kind := getString(args, "kind")
	language := getString(args, "language")

	caller, _ := UserFrom(ctx)
	now := time.Now().UTC()
	s.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action":   "search",
		"query":    query,
		"kind":     kind,
		"language": language,
	}, now, "action:search")

	raw, err := s.indexer.SearchSymbols(ctx, r.GitRemote, query, kind, language)
	if err != nil {
		return indexerErrorResult("search", err), nil
	}
	return mcpgo.NewToolResultText(string(raw)), nil
}

// handleRepoSearchText implements `repo_search_text` — proxy to
// jcodemunch__search_text + audit row.
func (s *Server) handleRepoSearchText(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := s.resolveRepoForProxy(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	query, qerr := req.RequireString("query")
	if qerr != nil {
		return mcpgo.NewToolResultError(qerr.Error()), nil
	}
	filePattern := req.GetString("file_pattern", "")

	caller, _ := UserFrom(ctx)
	now := time.Now().UTC()
	s.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action":       "search_text",
		"query":        query,
		"file_pattern": filePattern,
	}, now, "action:search_text")

	raw, err := s.indexer.SearchText(ctx, r.GitRemote, query, filePattern)
	if err != nil {
		return indexerErrorResult("search_text", err), nil
	}
	return mcpgo.NewToolResultText(string(raw)), nil
}

// handleRepoGetSymbolSource implements `repo_get_symbol_source`.
func (s *Server) handleRepoGetSymbolSource(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := s.resolveRepoForProxy(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	symbolID, serr := req.RequireString("symbol_id")
	if serr != nil {
		return mcpgo.NewToolResultError(serr.Error()), nil
	}

	caller, _ := UserFrom(ctx)
	now := time.Now().UTC()
	s.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action":    "get_symbol_source",
		"symbol_id": symbolID,
	}, now, "action:get_symbol_source")

	raw, err := s.indexer.GetSymbolSource(ctx, r.GitRemote, symbolID)
	if err != nil {
		return indexerErrorResult("get_symbol_source", err), nil
	}
	return mcpgo.NewToolResultText(string(raw)), nil
}

// handleRepoGetFile implements `repo_get_file`.
func (s *Server) handleRepoGetFile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := s.resolveRepoForProxy(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	path, perr := req.RequireString("path")
	if perr != nil {
		return mcpgo.NewToolResultError(perr.Error()), nil
	}

	caller, _ := UserFrom(ctx)
	now := time.Now().UTC()
	s.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action": "get_file",
		"path":   path,
	}, now, "action:get_file")

	raw, err := s.indexer.GetFileContent(ctx, r.GitRemote, path)
	if err != nil {
		return indexerErrorResult("get_file", err), nil
	}
	return mcpgo.NewToolResultText(string(raw)), nil
}

// handleRepoGetOutline implements `repo_get_outline`.
func (s *Server) handleRepoGetOutline(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := s.resolveRepoForProxy(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	path, perr := req.RequireString("path")
	if perr != nil {
		return mcpgo.NewToolResultError(perr.Error()), nil
	}

	caller, _ := UserFrom(ctx)
	now := time.Now().UTC()
	s.appendRepoAuditRow(ctx, r, "kind:repo-query", caller.UserID, map[string]any{
		"action": "get_outline",
		"path":   path,
	}, now, "action:get_outline")

	raw, err := s.indexer.GetFileOutline(ctx, r.GitRemote, path)
	if err != nil {
		return indexerErrorResult("get_outline", err), nil
	}
	return mcpgo.NewToolResultText(string(raw)), nil
}

// resolveRepoForProxy is the shared boilerplate for the five proxy
// verbs: extract repo_id, scope by memberships, return the row.
func (s *Server) resolveRepoForProxy(ctx context.Context, req mcpgo.CallToolRequest) (repo.Repo, error) {
	if s.repos == nil {
		return repo.Repo{}, errors.New("repo verbs unavailable: repo store not configured")
	}
	caller, _ := UserFrom(ctx)
	repoID, err := req.RequireString("repo_id")
	if err != nil {
		return repo.Repo{}, err
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	r, err := s.repos.GetByID(ctx, repoID, memberships)
	if err != nil {
		return repo.Repo{}, err
	}
	return r, nil
}

// enqueueReindex is the shared task-enqueue helper for repo_add and
// repo_scan. Returns the new task id, or empty string when the task
// store rejected the enqueue (logged but non-fatal — the repo row is
// still created so a follow-up repo_scan can retry).
func (s *Server) enqueueReindex(ctx context.Context, r repo.Repo, trigger, actor string, now time.Time) string {
	if s.tasks == nil {
		return ""
	}
	payload := reindexPayload{
		Handler:   "reindex_repo",
		RepoID:    r.ID,
		GitRemote: r.GitRemote,
		Trigger:   trigger,
	}
	body, _ := json.Marshal(payload)
	t, err := s.tasks.Enqueue(ctx, task.Task{
		WorkspaceID:      r.WorkspaceID,
		ProjectID:        r.ProjectID,
		Origin:           task.OriginEvent,
		Payload:          body,
		Priority:         task.PriorityMedium,
		ExpectedDuration: 5 * time.Minute,
	}, now)
	if err != nil {
		s.logger.Warn().Str("repo_id", r.ID).Err(err).Msg("repo: reindex enqueue failed")
		return ""
	}
	if s.ledger != nil {
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: t.WorkspaceID,
			ProjectID:   t.ProjectID,
			Type:        ledger.TypeDecision,
			Tags: []string{
				"kind:task-enqueued",
				"task_id:" + t.ID,
				"origin:" + t.Origin,
				"handler:reindex_repo",
				"repo_id:" + r.ID,
			},
			Content:    fmt.Sprintf("reindex task enqueued for repo %s (trigger=%s)", r.ID, trigger),
			Structured: body,
			CreatedBy:  actor,
		}, now)
	}
	return t.ID
}

// findInFlightReindex returns the id of a reindex task already in
// flight for r, or empty when none. Workspace-scoped via memberships.
func (s *Server) findInFlightReindex(ctx context.Context, r repo.Repo, memberships []string) string {
	if s.tasks == nil {
		return ""
	}
	for _, status := range []string{task.StatusEnqueued, task.StatusClaimed, task.StatusInFlight} {
		rows, err := s.tasks.List(ctx, task.ListOptions{
			Origin: task.OriginEvent,
			Status: status,
		}, memberships)
		if err != nil {
			continue
		}
		for _, t := range rows {
			if matchesReindex(t, r.ID) {
				return t.ID
			}
		}
	}
	return ""
}

// matchesReindex decodes a task's payload and reports whether it
// targets the given repo_id. Tasks with non-JSON or missing handler
// fall through silently — they're not reindex tasks.
func matchesReindex(t task.Task, repoID string) bool {
	if len(t.Payload) == 0 {
		return false
	}
	var p reindexPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return false
	}
	return p.Handler == "reindex_repo" && p.RepoID == repoID
}

// appendRepoAuditRow writes a repo audit ledger row. Extra tags are
// appended after the canonical {kind, repo_id} pair so callers can
// add action sub-tags without recomputing the base set.
func (s *Server) appendRepoAuditRow(ctx context.Context, r repo.Repo, kind, actor string, payload map[string]any, now time.Time, extraTags ...string) {
	if s.ledger == nil {
		return
	}
	tags := make([]string, 0, 2+len(extraTags))
	tags = append(tags, kind, "repo_id:"+r.ID)
	tags = append(tags, extraTags...)
	body, _ := json.Marshal(payload)
	_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		Type:        ledger.TypeDecision,
		Tags:        tags,
		Content:     fmt.Sprintf("%s repo=%s", kind, r.ID),
		Structured:  body,
		CreatedBy:   actor,
	}, now)
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

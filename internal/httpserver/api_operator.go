// api_operator.go — operator-tier read verbs added under sty_ef248ab2
// (cli-primary order:04-followup). Each route POST /api/v1/<noun>/<verb>
// delegates to a typed client.Client method or directly to a store via
// the client.Client helpers.
//
// Verbs landed here (28):
//
//   story:     list, template-get, template-list, export-walk
//   task:      list
//   project:   get, list
//   workspace: get, list, member-list
//   changelog: get, list
//   agent:     list, search, ephemeral-summary
//   contract:  list, search
//   principle: get, search
//   document:  search
//   kv:        get, list, get-resolved
//   repo:      get, list, search, search-text, get-symbol-source,
//              get-file, get-outline
//
// The Register() method below is invoked from api.go's Register().

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/repo"
)

// registerOperatorRoutes attaches the operator-tier read routes to the
// supplied mux. Called from APIRegistrar.Register.
func (a *APIRegistrar) registerOperatorRoutes(mux *http.ServeMux) {
	// story
	mux.HandleFunc("POST /api/v1/story/list", a.handleStoryList)
	mux.HandleFunc("POST /api/v1/story/template-get", a.handleStoryTemplateGet)
	mux.HandleFunc("POST /api/v1/story/template-list", a.handleStoryTemplateList)
	mux.HandleFunc("POST /api/v1/story/export-walk", a.handleStoryExportWalk)

	// task
	mux.HandleFunc("POST /api/v1/task/list", a.handleTaskList)

	// project
	mux.HandleFunc("POST /api/v1/project/get", a.handleProjectGet)
	mux.HandleFunc("POST /api/v1/project/list", a.handleProjectList)

	// workspace
	mux.HandleFunc("POST /api/v1/workspace/get", a.handleWorkspaceGet)
	mux.HandleFunc("POST /api/v1/workspace/list", a.handleWorkspaceList)
	mux.HandleFunc("POST /api/v1/workspace/member-list", a.handleWorkspaceMemberList)

	// changelog
	mux.HandleFunc("POST /api/v1/changelog/get", a.handleChangelogGet)
	mux.HandleFunc("POST /api/v1/changelog/list", a.handleChangelogList)

	// document family — agent/contract/principle wrap document_list/search/get.
	mux.HandleFunc("POST /api/v1/document/search", a.handleDocumentSearch)
	mux.HandleFunc("POST /api/v1/agent/list", a.handleAgentList)
	mux.HandleFunc("POST /api/v1/agent/search", a.handleAgentSearch)
	mux.HandleFunc("POST /api/v1/agent/ephemeral-summary", a.handleAgentEphemeralSummary)
	mux.HandleFunc("POST /api/v1/contract/list", a.handleContractList)
	mux.HandleFunc("POST /api/v1/contract/search", a.handleContractSearch)
	mux.HandleFunc("POST /api/v1/principle/get", a.handlePrincipleGet)
	mux.HandleFunc("POST /api/v1/principle/search", a.handlePrincipleSearch)

	// kv
	mux.HandleFunc("POST /api/v1/kv/get", a.handleKVGet)
	mux.HandleFunc("POST /api/v1/kv/list", a.handleKVList)
	mux.HandleFunc("POST /api/v1/kv/get-resolved", a.handleKVGetResolved)

	// repo
	mux.HandleFunc("POST /api/v1/repo/get", a.handleRepoGet)
	mux.HandleFunc("POST /api/v1/repo/list", a.handleRepoList)
	mux.HandleFunc("POST /api/v1/repo/search", a.handleRepoSearch)
	mux.HandleFunc("POST /api/v1/repo/search-text", a.handleRepoSearchText)
	mux.HandleFunc("POST /api/v1/repo/get-symbol-source", a.handleRepoGetSymbolSource)
	mux.HandleFunc("POST /api/v1/repo/get-file", a.handleRepoGetFile)
	mux.HandleFunc("POST /api/v1/repo/get-outline", a.handleRepoGetOutline)
}

// ----- story -----

func (a *APIRegistrar) handleStoryList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
		Priority  string `json:"priority"`
		Tag       string `json:"tag"`
		Limit     int    `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ProjectID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "project_id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	resolvedProj, err := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	out, err := a.client.StoryList(r.Context(), cc, client.StoryListInput{
		ProjectID:   resolvedProj,
		Status:      req.Status,
		Priority:    req.Priority,
		Tag:         req.Tag,
		Limit:       req.Limit,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleStoryTemplateGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string `json:"category"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	out, err := a.client.StoryTemplateGet(r.Context(), cc, client.StoryTemplateGetInput{Category: req.Category})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleStoryTemplateList(w http.ResponseWriter, r *http.Request) {
	if err := decodeJSONBody(r, &struct{}{}); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	out, err := a.client.StoryTemplateList(r.Context(), cc)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleStoryExportWalk(w http.ResponseWriter, r *http.Request) {
	// Story export-walk renders the contract walk as paste-ready markdown.
	// The MCP handler delegates to story.RenderExportWalk; we mirror the
	// dispatch here without a typed client method since the rendering
	// itself is stateless once the story + walk are loaded.
	var req struct {
		StoryID string `json:"story_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.StoryID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "story_id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.StoryExportWalk(r.Context(), cc, client.StoryExportWalkInput{
		StoryID:     req.StoryID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// ----- task -----

func (a *APIRegistrar) handleTaskList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StoryID         string `json:"story_id"`
		Status          string `json:"status"`
		Kind            string `json:"kind"`
		Origin          string `json:"origin"`
		Priority        string `json:"priority"`
		ClaimedBy       string `json:"claimed_by"`
		Limit           int    `json:"limit"`
		IncludeArchived bool   `json:"include_archived"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.TaskList(r.Context(), cc, client.TaskListInput{
		StoryID:         req.StoryID,
		Status:          req.Status,
		Kind:            req.Kind,
		Origin:          req.Origin,
		Priority:        req.Priority,
		ClaimedBy:       req.ClaimedBy,
		Limit:           req.Limit,
		IncludeArchived: req.IncludeArchived,
		Memberships:     cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// ----- project -----

func (a *APIRegistrar) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.ProjectGet(r.Context(), cc, client.ProjectGetInput{
		ID:          req.ID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleProjectList(w http.ResponseWriter, r *http.Request) {
	if err := decodeJSONBody(r, &struct{}{}); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.ProjectList(r.Context(), cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// ----- workspace -----

func (a *APIRegistrar) handleWorkspaceGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	out, err := a.client.WorkspaceGet(r.Context(), cc, client.WorkspaceGetInput{ID: req.ID})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleWorkspaceList(w http.ResponseWriter, r *http.Request) {
	if err := decodeJSONBody(r, &struct{}{}); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	out, err := a.client.WorkspaceList(r.Context(), cc)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleWorkspaceMemberList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	out, err := a.client.WorkspaceMemberList(r.Context(), cc, client.WorkspaceMemberListInput{WorkspaceID: req.WorkspaceID})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// ----- changelog -----

func (a *APIRegistrar) handleChangelogGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.ChangelogGet(r.Context(), cc, client.ChangelogGetInput{
		ID:          req.ID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleChangelogList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
		Service   string `json:"service"`
		Limit     int    `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.ChangelogList(r.Context(), cc, client.ChangelogListInput{
		ProjectID:   req.ProjectID,
		Service:     req.Service,
		Limit:       req.Limit,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// ----- document family -----

// documentListReq is the shared list-shape for /document/list, /agent/list,
// /contract/list, /principle/list. The MCP wrappers (principle_list etc.)
// just pin `type`; we keep the wire field on the request body and let
// per-route shims set the type when they wrap.
type documentListReq struct {
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	ProjectID string   `json:"project_id"`
	Scope     string   `json:"scope"`
	Tags      []string `json:"tags"`
	Limit     int      `json:"limit"`
}

func (a *APIRegistrar) decodeDocumentListReq(r *http.Request) (documentListReq, error) {
	var req documentListReq
	err := decodeJSONBody(r, &req)
	return req, err
}

func (a *APIRegistrar) runDocumentList(w http.ResponseWriter, r *http.Request, pinnedType string) {
	req, err := a.decodeDocumentListReq(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if pinnedType != "" {
		req.Type = pinnedType
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	resolvedProj, _ := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), resolvedProj)
	opts := document.ListOptions{
		Type:      req.Type,
		ProjectID: resolvedProj,
		Scope:     req.Scope,
		Tags:      req.Tags,
		Limit:     req.Limit,
	}
	out, err := a.client.DocumentList(r.Context(), cc, client.DocumentListInput{
		Options:     opts,
		WorkspaceID: wsID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// documentSearchReq is the shared search-shape.
type documentSearchReq struct {
	Type            string   `json:"type"`
	Query           string   `json:"query"`
	Scope           string   `json:"scope"`
	ProjectID       string   `json:"project_id"`
	ContractBinding string   `json:"contract_binding"`
	Tags            []string `json:"tags"`
	TopK            int      `json:"top_k"`
}

func (a *APIRegistrar) runDocumentSearch(w http.ResponseWriter, r *http.Request, pinnedType string) {
	var req documentSearchReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if pinnedType != "" {
		req.Type = pinnedType
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.DocumentSearch(r.Context(), cc, client.DocumentSearchInput{
		Type:             req.Type,
		Query:            req.Query,
		Scope:            req.Scope,
		ProjectID:        req.ProjectID,
		ContractBinding:  req.ContractBinding,
		Tags:             req.Tags,
		TopK:             req.TopK,
		Memberships:      cc.Memberships,
		ResolveProjectID: req.ProjectID != "",
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleDocumentSearch(w http.ResponseWriter, r *http.Request) {
	a.runDocumentSearch(w, r, "")
}

func (a *APIRegistrar) handleAgentList(w http.ResponseWriter, r *http.Request) {
	a.runDocumentList(w, r, document.TypeAgent)
}

func (a *APIRegistrar) handleAgentSearch(w http.ResponseWriter, r *http.Request) {
	a.runDocumentSearch(w, r, document.TypeAgent)
}

func (a *APIRegistrar) handleContractList(w http.ResponseWriter, r *http.Request) {
	a.runDocumentList(w, r, document.TypeContract)
}

func (a *APIRegistrar) handleContractSearch(w http.ResponseWriter, r *http.Request) {
	a.runDocumentSearch(w, r, document.TypeContract)
}

func (a *APIRegistrar) handlePrincipleGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.PrincipleGet(r.Context(), cc, req.ID, req.Name, req.ProjectID, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handlePrincipleSearch(w http.ResponseWriter, r *http.Request) {
	a.runDocumentSearch(w, r, document.TypePrinciple)
}

// agent_ephemeral_summary aggregates active ephemeral agents per project.
// Returns {project_id → count} for the caller's accessible projects.
func (a *APIRegistrar) handleAgentEphemeralSummary(w http.ResponseWriter, r *http.Request) {
	if err := decodeJSONBody(r, &struct{}{}); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Documents == nil {
		writeAPIError(w, errors.New("document store not configured"))
		return
	}
	docs, err := stores.Documents.List(r.Context(), document.ListOptions{
		Type:  document.TypeAgent,
		Tags:  []string{"ephemeral"},
		Limit: 500,
	}, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	counts := map[string]int{}
	for _, d := range docs {
		if d.Status != document.StatusActive {
			continue
		}
		if d.ProjectID == nil || *d.ProjectID == "" {
			continue
		}
		counts[*d.ProjectID]++
	}
	writeAPIJSON(w, map[string]any{"counts": counts, "total": len(docs)})
}

// ----- kv -----

func (a *APIRegistrar) handleKVGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope       string `json:"scope"`
		Key         string `json:"key"`
		WorkspaceID string `json:"workspace_id"`
		ProjectID   string `json:"project_id"`
		UserID      string `json:"user_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	row, found, err := a.client.KVGet(r.Context(), cc, client.KVGetInput{
		Scope:       req.Scope,
		Key:         req.Key,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		UserID:      req.UserID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !found {
		writeAPIStatus(w, http.StatusNotFound, fmt.Sprintf("not_found: scope=%s key=%s", req.Scope, req.Key))
		return
	}
	writeAPIJSON(w, row)
}

func (a *APIRegistrar) handleKVList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope       string `json:"scope"`
		WorkspaceID string `json:"workspace_id"`
		ProjectID   string `json:"project_id"`
		UserID      string `json:"user_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	scope, rows, err := a.client.KVList(r.Context(), cc, client.KVListInput{
		Scope:       req.Scope,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		UserID:      req.UserID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{
		"scope": scope,
		"items": rows,
	})
}

func (a *APIRegistrar) handleKVGetResolved(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key         string `json:"key"`
		WorkspaceID string `json:"workspace_id"`
		ProjectID   string `json:"project_id"`
		UserID      string `json:"user_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	row, found, err := a.client.KVGetResolved(r.Context(), cc, client.KVGetResolvedInput{
		Key:         req.Key,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		UserID:      req.UserID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !found {
		writeAPIStatus(w, http.StatusNotFound, fmt.Sprintf("not_found: key=%s", req.Key))
		return
	}
	writeAPIJSON(w, row)
}

// ----- repo -----

func (a *APIRegistrar) handleRepoGet(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Repos == nil {
		writeAPIStatus(w, http.StatusBadRequest, "repo_get unavailable")
		return
	}
	var req struct {
		RepoID string `json:"repo_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.RepoID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "repo_id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	rr, err := stores.Repos.GetByID(r.Context(), req.RepoID, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, rr)
}

func (a *APIRegistrar) handleRepoList(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Repos == nil {
		writeAPIStatus(w, http.StatusBadRequest, "repo_list unavailable")
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	projectID, err := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	statusFilter := req.Status
	if statusFilter == "" {
		statusFilter = repo.StatusActive
	}
	rows, err := stores.Repos.List(r.Context(), projectID, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if statusFilter != "all" {
		filtered := make([]repo.Repo, 0, len(rows))
		for _, rr := range rows {
			if rr.Status == statusFilter {
				filtered = append(filtered, rr)
			}
		}
		rows = filtered
	}
	writeAPIJSON(w, map[string]any{
		"project_id": projectID,
		"status":     statusFilter,
		"repos":      rows,
	})
}

// resolveRepoForProxy is the shared boilerplate for repo proxy verbs.
// Returns the resolved repo row + a typed http-error on failure.
func (a *APIRegistrar) resolveRepoForProxy(r *http.Request, repoID string) (repo.Repo, error) {
	stores := a.client.Stores()
	if stores.Repos == nil {
		return repo.Repo{}, errors.New("repo verbs unavailable")
	}
	if repoID == "" {
		return repo.Repo{}, errors.New("repo_id required")
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	return stores.Repos.GetByID(r.Context(), repoID, cc.Memberships)
}

// appendRepoAuditRow mirrors mcpserver.appendRepoAuditRow.
func (a *APIRegistrar) appendRepoAuditRow(ctx context.Context, rr repo.Repo, kind, actor string, payload map[string]any, now time.Time, extraTags ...string) {
	stores := a.client.Stores()
	if stores.Ledger == nil {
		return
	}
	tags := make([]string, 0, 2+len(extraTags))
	tags = append(tags, kind, "repo_id:"+rr.ID)
	tags = append(tags, extraTags...)
	body, _ := json.Marshal(payload)
	_, _ = stores.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: rr.WorkspaceID,
		ProjectID:   rr.ProjectID,
		Type:        ledger.TypeDecision,
		Tags:        tags,
		Content:     kind + " repo=" + rr.ID,
		Structured:  body,
		CreatedBy:   actor,
	}, now)
}

// writeIndexerErr maps an indexer error to the documented envelope.
func writeIndexerErr(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, codeindex.ErrUnavailable) {
		body, _ := json.Marshal(map[string]any{
			"error":  "code_index_unavailable",
			"op":     op,
			"detail": err.Error(),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
		return
	}
	writeAPIStatus(w, http.StatusInternalServerError, op+": "+err.Error())
}

func (a *APIRegistrar) repoProxyCommon(w http.ResponseWriter, r *http.Request, repoID, action string, payload map[string]any) (repo.Repo, bool) {
	rr, err := a.resolveRepoForProxy(r, repoID)
	if err != nil {
		writeAPIError(w, err)
		return repo.Repo{}, false
	}
	id, _ := auth.UserFrom(r.Context())
	a.appendRepoAuditRow(r.Context(), rr, "kind:repo-query", id.UserID, payload, time.Now().UTC(), "action:"+action)
	return rr, true
}

func (a *APIRegistrar) writeIndexerRaw(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (a *APIRegistrar) handleRepoSearch(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Indexer == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "code_index_unavailable")
		return
	}
	var req struct {
		RepoID   string `json:"repo_id"`
		Query    string `json:"query"`
		Kind     string `json:"kind"`
		Language string `json:"language"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeAPIStatus(w, http.StatusBadRequest, "query required")
		return
	}
	rr, ok := a.repoProxyCommon(w, r, req.RepoID, "search", map[string]any{
		"query": req.Query, "kind": req.Kind, "language": req.Language,
	})
	if !ok {
		return
	}
	raw, err := stores.Indexer.SearchSymbols(r.Context(), rr.GitRemote, req.Query, req.Kind, req.Language)
	if err != nil {
		writeIndexerErr(w, "search", err)
		return
	}
	a.writeIndexerRaw(w, raw)
}

func (a *APIRegistrar) handleRepoSearchText(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Indexer == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "code_index_unavailable")
		return
	}
	var req struct {
		RepoID      string `json:"repo_id"`
		Query       string `json:"query"`
		FilePattern string `json:"file_pattern"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeAPIStatus(w, http.StatusBadRequest, "query required")
		return
	}
	rr, ok := a.repoProxyCommon(w, r, req.RepoID, "search_text", map[string]any{
		"query": req.Query, "file_pattern": req.FilePattern,
	})
	if !ok {
		return
	}
	raw, err := stores.Indexer.SearchText(r.Context(), rr.GitRemote, req.Query, req.FilePattern)
	if err != nil {
		writeIndexerErr(w, "search_text", err)
		return
	}
	a.writeIndexerRaw(w, raw)
}

func (a *APIRegistrar) handleRepoGetSymbolSource(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Indexer == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "code_index_unavailable")
		return
	}
	var req struct {
		RepoID   string `json:"repo_id"`
		SymbolID string `json:"symbol_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.SymbolID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "symbol_id required")
		return
	}
	rr, ok := a.repoProxyCommon(w, r, req.RepoID, "get_symbol_source", map[string]any{"symbol_id": req.SymbolID})
	if !ok {
		return
	}
	raw, err := stores.Indexer.GetSymbolSource(r.Context(), rr.GitRemote, req.SymbolID)
	if err != nil {
		writeIndexerErr(w, "get_symbol_source", err)
		return
	}
	a.writeIndexerRaw(w, raw)
}

func (a *APIRegistrar) handleRepoGetFile(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Indexer == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "code_index_unavailable")
		return
	}
	var req struct {
		RepoID string `json:"repo_id"`
		Path   string `json:"path"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Path == "" {
		writeAPIStatus(w, http.StatusBadRequest, "path required")
		return
	}
	rr, ok := a.repoProxyCommon(w, r, req.RepoID, "get_file", map[string]any{"path": req.Path})
	if !ok {
		return
	}
	raw, err := stores.Indexer.GetFileContent(r.Context(), rr.GitRemote, req.Path)
	if err != nil {
		writeIndexerErr(w, "get_file", err)
		return
	}
	a.writeIndexerRaw(w, raw)
}

func (a *APIRegistrar) handleRepoGetOutline(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.Indexer == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "code_index_unavailable")
		return
	}
	var req struct {
		RepoID string `json:"repo_id"`
		Path   string `json:"path"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Path == "" {
		writeAPIStatus(w, http.StatusBadRequest, "path required")
		return
	}
	rr, ok := a.repoProxyCommon(w, r, req.RepoID, "get_outline", map[string]any{"path": req.Path})
	if !ok {
		return
	}
	raw, err := stores.Indexer.GetFileOutline(r.Context(), rr.GitRemote, req.Path)
	if err != nil {
		writeIndexerErr(w, "get_outline", err)
		return
	}
	a.writeIndexerRaw(w, raw)
}

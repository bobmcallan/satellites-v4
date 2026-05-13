// api_operator_writes.go — operator-tier mutating verbs added under
// sty_f38bd573 (cli-primary order:05-followup) Tier A.
//
// Tier A scope (~30 verbs, straightforward CRUD):
//   story:     create, update, delete
//   project:   create, update, delete
//   workspace: create, member-add, member-update-role, member-remove
//   kv:        set, delete
//   changelog: add, update, delete
//   repo:      add
//   document:  create, update, delete
//   agent/contract/principle/reviewer/role/skill: create, update, delete
//
// Tier B (filed as follow-up): agent_compose, agent_apikey_*,
// project_seed_run, system_seed_run, portal_replicate,
// document_ingest_file — each has substantive MCP-helper code that
// needs a deeper extraction pass.
//
// session_register + ledger_dereference already have /api/v1 routes
// from the order:07a anchor and sty_ef248ab2 respectively; this story
// just un-stubs the CLI verbs that target them.

package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// registerOperatorWriteRoutes wires the Tier A mutating routes.
func (a *APIRegistrar) registerOperatorWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/story/add", a.handleStoryAdd)
	mux.HandleFunc("POST /api/v1/story/update", a.handleStoryUpdate)
	mux.HandleFunc("POST /api/v1/story/delete", a.handleStoryDelete)

	mux.HandleFunc("POST /api/v1/project/create", a.handleProjectCreate)
	mux.HandleFunc("POST /api/v1/project/update", a.handleProjectUpdate)
	mux.HandleFunc("POST /api/v1/project/delete", a.handleProjectDelete)

	mux.HandleFunc("POST /api/v1/workspace/add", a.handleWorkspaceAdd)
	mux.HandleFunc("POST /api/v1/workspace/member-add", a.handleWorkspaceMemberAdd)
	mux.HandleFunc("POST /api/v1/workspace/member-update-role", a.handleWorkspaceMemberUpdateRole)
	mux.HandleFunc("POST /api/v1/workspace/member-remove", a.handleWorkspaceMemberRemove)

	mux.HandleFunc("POST /api/v1/kv/set", a.handleKVSet)
	mux.HandleFunc("POST /api/v1/kv/delete", a.handleKVDelete)

	mux.HandleFunc("POST /api/v1/changelog/add", a.handleChangelogAdd)
	mux.HandleFunc("POST /api/v1/changelog/update", a.handleChangelogUpdate)
	mux.HandleFunc("POST /api/v1/changelog/delete", a.handleChangelogDelete)

	mux.HandleFunc("POST /api/v1/repo/add", a.handleRepoAdd)

	mux.HandleFunc("POST /api/v1/document/add", a.handleDocumentAdd)
	mux.HandleFunc("POST /api/v1/document/update", a.handleDocumentUpdate)
	mux.HandleFunc("POST /api/v1/document/delete", a.handleDocumentDelete)

	// Doc-family wrappers — same handlers with type pinned by the route.
	mux.HandleFunc("POST /api/v1/agent/add", a.handleAgentAdd)
	mux.HandleFunc("POST /api/v1/agent/update", a.handleAgentUpdate)
	mux.HandleFunc("POST /api/v1/agent/delete", a.handleAgentDelete)
	mux.HandleFunc("POST /api/v1/contract/add", a.handleContractAdd)
	mux.HandleFunc("POST /api/v1/contract/update", a.handleContractUpdate)
	mux.HandleFunc("POST /api/v1/contract/delete", a.handleContractDelete)
	mux.HandleFunc("POST /api/v1/principle/add", a.handlePrincipleAdd)
	mux.HandleFunc("POST /api/v1/principle/update", a.handlePrincipleUpdate)
	mux.HandleFunc("POST /api/v1/principle/delete", a.handlePrincipleDelete)
	mux.HandleFunc("POST /api/v1/reviewer/add", a.handleReviewerAdd)
	mux.HandleFunc("POST /api/v1/reviewer/update", a.handleReviewerUpdate)
	mux.HandleFunc("POST /api/v1/reviewer/delete", a.handleReviewerDelete)
	mux.HandleFunc("POST /api/v1/role/add", a.handleRoleAdd)
	mux.HandleFunc("POST /api/v1/role/update", a.handleRoleUpdate)
	mux.HandleFunc("POST /api/v1/role/delete", a.handleRoleDelete)
	mux.HandleFunc("POST /api/v1/skill/add", a.handleSkillAdd)
	mux.HandleFunc("POST /api/v1/skill/update", a.handleSkillUpdate)
	mux.HandleFunc("POST /api/v1/skill/delete", a.handleSkillDelete)
}

// ----- story -----

func (a *APIRegistrar) handleStoryAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID          string   `json:"project_id"`
		Title              string   `json:"title"`
		Description        string   `json:"description"`
		AcceptanceCriteria string   `json:"acceptance_criteria"`
		Priority           string   `json:"priority"`
		Category           string   `json:"category"`
		Tags               []string `json:"tags"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ProjectID == "" || req.Title == "" {
		writeAPIStatus(w, http.StatusBadRequest, "project_id and title required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Stories == nil {
		writeAPIError(w, errors.New("story store not configured"))
		return
	}
	resolvedID, err := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), resolvedID)
	candidate := story.Story{
		WorkspaceID:        wsID,
		ProjectID:          resolvedID,
		Title:              req.Title,
		Description:        req.Description,
		AcceptanceCriteria: req.AcceptanceCriteria,
		Priority:           ifEmpty(req.Priority, "medium"),
		Category:           ifEmpty(req.Category, "feature"),
		Tags:               req.Tags,
		CreatedBy:          cc.UserID,
	}
	st, err := stores.Stories.Create(r.Context(), candidate, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, st)
}

func (a *APIRegistrar) handleStoryUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID                 string    `json:"id"`
		Title              *string   `json:"title,omitempty"`
		Description        *string   `json:"description,omitempty"`
		AcceptanceCriteria *string   `json:"acceptance_criteria,omitempty"`
		Category           *string   `json:"category,omitempty"`
		Priority           *string   `json:"priority,omitempty"`
		Tags               *[]string `json:"tags,omitempty"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Stories == nil {
		writeAPIError(w, errors.New("story store not configured"))
		return
	}
	existing, err := stores.Stories.GetByID(r.Context(), req.ID, cc.Memberships)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "story not found")
		return
	}
	if _, err := a.client.ResolveProjectID(r.Context(), existing.ProjectID, "", cc, cc.Memberships); err != nil {
		writeAPIStatus(w, http.StatusNotFound, "story not found")
		return
	}
	fields := story.UpdateFields{
		Title:              req.Title,
		Description:        req.Description,
		AcceptanceCriteria: req.AcceptanceCriteria,
		Category:           req.Category,
		Priority:           req.Priority,
		Tags:               req.Tags,
	}
	updated, err := stores.Stories.Update(r.Context(), req.ID, fields, cc.UserID, time.Now().UTC(), cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, updated)
}

func (a *APIRegistrar) handleStoryDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Stories == nil {
		writeAPIError(w, errors.New("story store not configured"))
		return
	}
	updated, err := a.client.StoryUpdateStatus(r.Context(), cc, client.StoryUpdateStatusInput{
		ID:          req.ID,
		Status:      "cancelled",
		Memberships: cc.Memberships,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{"id": updated.ID, "status": updated.Status, "deleted": true})
}

// ----- project -----

func (a *APIRegistrar) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Name == "" {
		writeAPIStatus(w, http.StatusBadRequest, "name required")
		return
	}
	cc := a.clientCaller(r)
	if cc.UserID == "" {
		writeAPIStatus(w, http.StatusUnauthorized, "no caller identity")
		return
	}
	stores := a.client.Stores()
	if stores.Projects == nil {
		writeAPIError(w, errors.New("project store not configured"))
		return
	}
	wsID := a.client.ResolveCallerWorkspaceID(r.Context(), cc)
	p, err := stores.Projects.Create(r.Context(), cc.UserID, wsID, req.Name, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, p)
}

func (a *APIRegistrar) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		MCPURL *string `json:"mcp_url,omitempty"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Projects == nil {
		writeAPIError(w, errors.New("project store not configured"))
		return
	}
	existing, err := stores.Projects.GetByID(r.Context(), req.ID, cc.Memberships)
	if err != nil || existing.OwnerUserID != cc.UserID {
		writeAPIStatus(w, http.StatusNotFound, "project not found")
		return
	}
	now := time.Now().UTC()
	updated := existing
	if req.Name != "" && req.Name != existing.Name {
		next, rerr := stores.Projects.UpdateName(r.Context(), req.ID, req.Name, now)
		if rerr != nil {
			writeAPIError(w, rerr)
			return
		}
		updated = next
	}
	if req.MCPURL != nil && *req.MCPURL != updated.MCPURL {
		next, merr := stores.Projects.SetMCPURL(r.Context(), req.ID, *req.MCPURL, now)
		if merr != nil {
			writeAPIError(w, merr)
			return
		}
		updated = next
	}
	writeAPIJSON(w, updated)
}

func (a *APIRegistrar) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Projects == nil {
		writeAPIError(w, errors.New("project store not configured"))
		return
	}
	existing, err := stores.Projects.GetByID(r.Context(), req.ID, cc.Memberships)
	if err != nil || existing.OwnerUserID != cc.UserID {
		writeAPIStatus(w, http.StatusNotFound, "project not found")
		return
	}
	updated, err := stores.Projects.SetStatus(r.Context(), req.ID, project.StatusArchived, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, updated)
}

// ----- workspace -----

func (a *APIRegistrar) handleWorkspaceAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Name == "" {
		writeAPIStatus(w, http.StatusBadRequest, "name required")
		return
	}
	cc := a.clientCaller(r)
	if cc.UserID == "" {
		writeAPIStatus(w, http.StatusUnauthorized, "no caller identity")
		return
	}
	stores := a.client.Stores()
	if stores.Workspaces == nil {
		writeAPIError(w, errors.New("workspace store not configured"))
		return
	}
	ws, err := stores.Workspaces.Create(r.Context(), cc.UserID, req.Name, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, ws)
}

// requireWorkspaceAdmin checks the caller is an admin of the workspace.
func (a *APIRegistrar) requireWorkspaceAdmin(r *http.Request, workspaceID string) error {
	stores := a.client.Stores()
	if stores.Workspaces == nil {
		return errors.New("workspace store not configured")
	}
	cc := a.clientCaller(r)
	if cc.UserID == "" {
		return errors.New("no caller identity")
	}
	role, err := stores.Workspaces.GetRole(r.Context(), workspaceID, cc.UserID)
	if err != nil {
		return errors.New("workspace not found")
	}
	if role != workspace.RoleAdmin {
		return errors.New("admin role required")
	}
	return nil
}

func (a *APIRegistrar) workspaceAdminCount(r *http.Request, workspaceID string) (int, error) {
	stores := a.client.Stores()
	members, err := stores.Workspaces.ListMembers(r.Context(), workspaceID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range members {
		if m.Role == workspace.RoleAdmin {
			n++
		}
	}
	return n, nil
}

func (a *APIRegistrar) appendMembershipAudit(r *http.Request, workspaceID, kind, actor string, payload map[string]any) {
	stores := a.client.Stores()
	if stores.Ledger == nil || stores.DefaultProjectID == "" {
		return
	}
	payload["workspace_id"] = workspaceID
	payload["kind"] = kind
	body, _ := json.Marshal(payload)
	_, _ = stores.Ledger.Append(r.Context(), ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   stores.DefaultProjectID,
		Type:        ledger.TypeDecision,
		Tags:        []string{"kind:workspace." + kind},
		Content:     string(body),
		CreatedBy:   actor,
	}, time.Now().UTC())
}

func (a *APIRegistrar) handleWorkspaceMemberAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id"`
		Role        string `json:"role"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.WorkspaceID == "" || req.UserID == "" || req.Role == "" {
		writeAPIStatus(w, http.StatusBadRequest, "workspace_id, user_id, role required")
		return
	}
	if err := a.requireWorkspaceAdmin(r, req.WorkspaceID); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	stores := a.client.Stores()
	if err := stores.Workspaces.AddMember(r.Context(), req.WorkspaceID, req.UserID, req.Role, cc.UserID, time.Now().UTC()); err != nil {
		writeAPIError(w, err)
		return
	}
	a.appendMembershipAudit(r, req.WorkspaceID, "member_add", cc.UserID, map[string]any{
		"target_user_id": req.UserID,
		"role":           req.Role,
	})
	writeAPIJSON(w, map[string]any{
		"workspace_id": req.WorkspaceID,
		"user_id":      req.UserID,
		"role":         req.Role,
	})
}

func (a *APIRegistrar) handleWorkspaceMemberUpdateRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id"`
		Role        string `json:"role"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.WorkspaceID == "" || req.UserID == "" || req.Role == "" {
		writeAPIStatus(w, http.StatusBadRequest, "workspace_id, user_id, role required")
		return
	}
	if err := a.requireWorkspaceAdmin(r, req.WorkspaceID); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	stores := a.client.Stores()
	currentRole, err := stores.Workspaces.GetRole(r.Context(), req.WorkspaceID, req.UserID)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "member not found")
		return
	}
	if currentRole == workspace.RoleAdmin && req.Role != workspace.RoleAdmin {
		count, cerr := a.workspaceAdminCount(r, req.WorkspaceID)
		if cerr != nil {
			writeAPIError(w, cerr)
			return
		}
		if count <= 1 {
			writeAPIStatus(w, http.StatusBadRequest, "cannot downgrade the last admin")
			return
		}
	}
	if err := stores.Workspaces.UpdateRole(r.Context(), req.WorkspaceID, req.UserID, req.Role, time.Now().UTC()); err != nil {
		writeAPIError(w, err)
		return
	}
	a.appendMembershipAudit(r, req.WorkspaceID, "member_update_role", cc.UserID, map[string]any{
		"target_user_id": req.UserID,
		"previous_role":  currentRole,
		"new_role":       req.Role,
	})
	writeAPIJSON(w, map[string]any{
		"workspace_id": req.WorkspaceID,
		"user_id":      req.UserID,
		"role":         req.Role,
	})
}

func (a *APIRegistrar) handleWorkspaceMemberRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.WorkspaceID == "" || req.UserID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "workspace_id and user_id required")
		return
	}
	if err := a.requireWorkspaceAdmin(r, req.WorkspaceID); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	stores := a.client.Stores()
	currentRole, err := stores.Workspaces.GetRole(r.Context(), req.WorkspaceID, req.UserID)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "member not found")
		return
	}
	if currentRole == workspace.RoleAdmin {
		count, cerr := a.workspaceAdminCount(r, req.WorkspaceID)
		if cerr != nil {
			writeAPIError(w, cerr)
			return
		}
		if count <= 1 {
			writeAPIStatus(w, http.StatusBadRequest, "cannot remove the last admin")
			return
		}
	}
	if err := stores.Workspaces.RemoveMember(r.Context(), req.WorkspaceID, req.UserID); err != nil {
		writeAPIError(w, err)
		return
	}
	a.appendMembershipAudit(r, req.WorkspaceID, "member_remove", cc.UserID, map[string]any{
		"target_user_id": req.UserID,
		"previous_role":  currentRole,
	})
	writeAPIJSON(w, map[string]any{
		"workspace_id": req.WorkspaceID,
		"user_id":      req.UserID,
		"removed":      true,
	})
}

// ----- kv -----

// kvWriteEntry builds a KV ledger entry. Tombstone deletes carry the
// well-known kv tombstone tag.
func kvWriteEntry(args client.KVScopeArgs, key, value, actor string, tombstone bool) ledger.LedgerEntry {
	tags := []string{
		"scope:" + string(args.Scope),
		"key:" + key,
	}
	if args.Scope == ledger.KVScopeUser && args.UserID != "" {
		tags = append(tags, "user:"+args.UserID)
	}
	if tombstone {
		tags = append(tags, ledger.KVTombstoneTag)
	}
	entry := ledger.LedgerEntry{
		WorkspaceID: args.WorkspaceID,
		Type:        ledger.TypeKV,
		Tags:        tags,
		Content:     value,
		CreatedBy:   actor,
	}
	if args.Scope == ledger.KVScopeProject {
		entry.ProjectID = args.ProjectID
	}
	return entry
}

// kvCheckWriteAuth mirrors mcpserver.kvCheckWriteAuth's role gate.
func (a *APIRegistrar) kvCheckWriteAuth(r *http.Request, args client.KVScopeArgs) error {
	cc := a.clientCaller(r)
	stores := a.client.Stores()
	switch args.Scope {
	case ledger.KVScopeSystem:
		if !cc.GlobalAdmin {
			return errors.New("forbidden: scope=system requires role=global_admin")
		}
		return nil
	case ledger.KVScopeWorkspace:
		if cc.GlobalAdmin {
			return nil
		}
		if stores.Workspaces == nil {
			return errors.New("forbidden: workspace store unavailable")
		}
		role, err := stores.Workspaces.GetRole(r.Context(), args.WorkspaceID, cc.UserID)
		if err != nil || role != workspace.RoleAdmin {
			return errors.New("forbidden: scope=workspace requires role=workspace_admin")
		}
		return nil
	case ledger.KVScopeProject:
		if cc.GlobalAdmin {
			return nil
		}
		if stores.Projects == nil {
			return errors.New("forbidden: project store unavailable")
		}
		p, err := stores.Projects.GetByID(r.Context(), args.ProjectID, nil)
		if err == nil && p.OwnerUserID == cc.UserID {
			return nil
		}
		if stores.Workspaces != nil && args.WorkspaceID != "" {
			role, rerr := stores.Workspaces.GetRole(r.Context(), args.WorkspaceID, cc.UserID)
			if rerr == nil && role == workspace.RoleAdmin {
				return nil
			}
		}
		return errors.New("forbidden: scope=project requires role=project_owner_or_workspace_admin")
	case ledger.KVScopeUser:
		if cc.UserID == "" || cc.UserID != args.UserID {
			return errors.New("forbidden: scope=user is self-only (v1)")
		}
		return nil
	default:
		return errors.New("forbidden: unknown scope")
	}
}

func (a *APIRegistrar) handleKVSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope       string `json:"scope"`
		Key         string `json:"key"`
		Value       string `json:"value"`
		WorkspaceID string `json:"workspace_id"`
		ProjectID   string `json:"project_id"`
		UserID      string `json:"user_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Scope == "" || req.Key == "" {
		writeAPIStatus(w, http.StatusBadRequest, "scope and key required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	args, _, err := a.client.ResolveKVScopeArgs(r.Context(), req.Scope, req.WorkspaceID, req.ProjectID, req.UserID, cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.kvCheckWriteAuth(r, args); err != nil {
		writeAPIStatus(w, http.StatusForbidden, err.Error())
		return
	}
	stores := a.client.Stores()
	entry := kvWriteEntry(args, req.Key, req.Value, cc.UserID, false)
	row, err := stores.Ledger.Append(r.Context(), entry, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{
		"scope":    args.Scope,
		"key":      req.Key,
		"value":    req.Value,
		"entry_id": row.ID,
	})
}

func (a *APIRegistrar) handleKVDelete(w http.ResponseWriter, r *http.Request) {
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
	if req.Scope == "" || req.Key == "" {
		writeAPIStatus(w, http.StatusBadRequest, "scope and key required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	args, _, err := a.client.ResolveKVScopeArgs(r.Context(), req.Scope, req.WorkspaceID, req.ProjectID, req.UserID, cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.kvCheckWriteAuth(r, args); err != nil {
		writeAPIStatus(w, http.StatusForbidden, err.Error())
		return
	}
	stores := a.client.Stores()
	entry := kvWriteEntry(args, req.Key, "", cc.UserID, true)
	row, err := stores.Ledger.Append(r.Context(), entry, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{
		"scope":    args.Scope,
		"key":      req.Key,
		"entry_id": row.ID,
		"deleted":  true,
	})
}

// ----- changelog -----

func (a *APIRegistrar) handleChangelogAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID     string `json:"project_id"`
		Service       string `json:"service"`
		Content       string `json:"content"`
		VersionFrom   string `json:"version_from"`
		VersionTo     string `json:"version_to"`
		EffectiveDate string `json:"effective_date"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Service == "" || req.Content == "" {
		writeAPIStatus(w, http.StatusBadRequest, "service and content required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Changelog == nil {
		writeAPIError(w, errors.New("changelog store not configured"))
		return
	}
	projectID, err := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), projectID)
	now := time.Now().UTC()
	eff := now
	if req.EffectiveDate != "" {
		t, perr := time.Parse(time.RFC3339, req.EffectiveDate)
		if perr != nil {
			writeAPIStatus(w, http.StatusBadRequest, "effective_date must be RFC3339")
			return
		}
		eff = t
	}
	row := changelog.Changelog{
		WorkspaceID:   wsID,
		ProjectID:     projectID,
		Service:       req.Service,
		VersionFrom:   req.VersionFrom,
		VersionTo:     req.VersionTo,
		Content:       req.Content,
		EffectiveDate: eff,
		CreatedBy:     cc.UserID,
	}
	out, err := stores.Changelog.Create(r.Context(), row, now)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleChangelogUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string  `json:"id"`
		VersionFrom   *string `json:"version_from,omitempty"`
		VersionTo     *string `json:"version_to,omitempty"`
		Content       *string `json:"content,omitempty"`
		EffectiveDate *string `json:"effective_date,omitempty"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	current, err := stores.Changelog.GetByID(r.Context(), req.ID, cc.Memberships)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "changelog not found")
		return
	}
	if _, err := a.client.ResolveProjectID(r.Context(), current.ProjectID, "", cc, cc.Memberships); err != nil {
		writeAPIStatus(w, http.StatusNotFound, "changelog not found")
		return
	}
	fields := changelog.UpdateFields{
		VersionFrom: req.VersionFrom,
		VersionTo:   req.VersionTo,
		Content:     req.Content,
	}
	if req.EffectiveDate != nil {
		t, perr := time.Parse(time.RFC3339, *req.EffectiveDate)
		if perr != nil {
			writeAPIStatus(w, http.StatusBadRequest, "effective_date must be RFC3339")
			return
		}
		fields.EffectiveDate = &t
	}
	out, err := stores.Changelog.Update(r.Context(), req.ID, fields, time.Now().UTC(), cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleChangelogDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	current, err := stores.Changelog.GetByID(r.Context(), req.ID, cc.Memberships)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "changelog not found")
		return
	}
	if _, err := a.client.ResolveProjectID(r.Context(), current.ProjectID, "", cc, cc.Memberships); err != nil {
		writeAPIStatus(w, http.StatusNotFound, "changelog not found")
		return
	}
	if err := stores.Changelog.Delete(r.Context(), req.ID, cc.Memberships); err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{"id": req.ID, "deleted": true})
}

// ----- repo -----

func (a *APIRegistrar) handleRepoAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID     string `json:"project_id"`
		GitRemote     string `json:"git_remote"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.GitRemote == "" {
		writeAPIStatus(w, http.StatusBadRequest, "git_remote required")
		return
	}
	gitRemote, err := project.CanonicaliseGitRemote(req.GitRemote)
	if err != nil || gitRemote == "" {
		writeAPIStatus(w, http.StatusBadRequest, "git_remote_invalid")
		return
	}
	defaultBranch := req.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Repos == nil {
		writeAPIError(w, errors.New("repo store not configured"))
		return
	}
	projectID, err := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), projectID)
	now := time.Now().UTC()
	if existing, gerr := stores.Repos.GetByRemote(r.Context(), wsID, gitRemote); gerr == nil {
		writeAPIJSON(w, map[string]any{
			"repo_id":        existing.ID,
			"deduplicated":   true,
			"git_remote":     existing.GitRemote,
			"default_branch": existing.DefaultBranch,
		})
		return
	} else if !errors.Is(gerr, repo.ErrNotFound) {
		writeAPIError(w, gerr)
		return
	}
	created, err := stores.Repos.Create(r.Context(), repo.Repo{
		WorkspaceID:   wsID,
		ProjectID:     projectID,
		GitRemote:     gitRemote,
		DefaultBranch: defaultBranch,
		Status:        repo.StatusActive,
	}, now)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	a.appendRepoAuditRow(r.Context(), created, "kind:repo-added", cc.UserID, map[string]any{
		"git_remote":     created.GitRemote,
		"default_branch": created.DefaultBranch,
	}, now)
	writeAPIJSON(w, map[string]any{
		"repo_id":        created.ID,
		"deduplicated":   false,
		"git_remote":     created.GitRemote,
		"default_branch": created.DefaultBranch,
	})
}

// ----- document family -----

// runDocumentAdd handles document_add and the type-pinned wrappers
// (agent_add / contract_add / etc.). When pinnedType is non-empty,
// the caller's type field is ignored and pinnedType wins.
func (a *APIRegistrar) runDocumentAdd(w http.ResponseWriter, r *http.Request, pinnedType string) {
	var req struct {
		Type            string   `json:"type"`
		Scope           string   `json:"scope"`
		Name            string   `json:"name"`
		ProjectID       string   `json:"project_id"`
		Body            string   `json:"body"`
		Structured      string   `json:"structured"`
		ContractBinding string   `json:"contract_binding"`
		Tags            []string `json:"tags"`
		Status          string   `json:"status"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if pinnedType != "" {
		req.Type = pinnedType
	}
	if req.Type == "" || req.Scope == "" || req.Name == "" {
		writeAPIStatus(w, http.StatusBadRequest, "type, scope, name required")
		return
	}
	cc := a.clientCaller(r)
	if cc.UserID == "" {
		writeAPIStatus(w, http.StatusUnauthorized, "no caller identity")
		return
	}
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Documents == nil {
		writeAPIError(w, errors.New("document store not configured"))
		return
	}
	wsID := a.client.ResolveCallerWorkspaceID(r.Context(), cc)
	doc := document.Document{
		WorkspaceID: wsID,
		Type:        req.Type,
		Scope:       req.Scope,
		Name:        req.Name,
		Body:        req.Body,
		Tags:        req.Tags,
		Status:      ifEmpty(req.Status, document.StatusActive),
		CreatedBy:   cc.UserID,
		UpdatedBy:   cc.UserID,
	}
	switch req.Scope {
	case document.ScopeProject:
		resolvedID, perr := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
		if perr != nil {
			writeAPIError(w, perr)
			return
		}
		doc.ProjectID = document.StringPtr(resolvedID)
		if cascade := a.client.ResolveProjectWorkspaceID(r.Context(), resolvedID); cascade != "" {
			doc.WorkspaceID = cascade
		}
	case document.ScopeSystem:
		if req.ProjectID != "" {
			writeAPIStatus(w, http.StatusBadRequest, "scope=system does not accept project_id")
			return
		}
		doc.WorkspaceID = ""
	}
	if req.ContractBinding != "" {
		doc.ContractBinding = document.StringPtr(req.ContractBinding)
	}
	if req.Structured != "" {
		if !json.Valid([]byte(req.Structured)) {
			writeAPIStatus(w, http.StatusBadRequest, "structured must be valid JSON")
			return
		}
		doc.Structured = []byte(req.Structured)
	}
	created, err := stores.Documents.Create(r.Context(), doc, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, created)
}

func (a *APIRegistrar) runDocumentUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID              string    `json:"id"`
		Body            *string   `json:"body,omitempty"`
		Structured      *string   `json:"structured,omitempty"`
		Tags            *[]string `json:"tags,omitempty"`
		Status          *string   `json:"status,omitempty"`
		ContractBinding *string   `json:"contract_binding,omitempty"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	fields := document.UpdateFields{
		Body:            req.Body,
		Tags:            req.Tags,
		Status:          req.Status,
		ContractBinding: req.ContractBinding,
	}
	if req.Structured != nil {
		if *req.Structured != "" && !json.Valid([]byte(*req.Structured)) {
			writeAPIStatus(w, http.StatusBadRequest, "structured must be valid JSON")
			return
		}
		buf := []byte(*req.Structured)
		fields.Structured = &buf
	}
	memberships := cc.Memberships
	if existing, gerr := stores.Documents.GetByID(r.Context(), req.ID, nil); gerr == nil && existing.Scope == document.ScopeSystem {
		memberships = nil
	}
	updated, err := stores.Documents.Update(r.Context(), req.ID, fields, cc.UserID, time.Now().UTC(), memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, updated)
}

func (a *APIRegistrar) runDocumentDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "id required")
		return
	}
	mode := document.DeleteMode(req.Mode)
	if mode == "" {
		mode = document.DeleteArchive
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	memberships := cc.Memberships
	if existing, gerr := stores.Documents.GetByID(r.Context(), req.ID, nil); gerr == nil && existing.Scope == document.ScopeSystem {
		memberships = nil
	}
	if err := stores.Documents.Delete(r.Context(), req.ID, mode, memberships); err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{"id": req.ID, "mode": string(mode), "deleted": true})
}

// Document family — pinned-type wrappers.
func (a *APIRegistrar) handleDocumentAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, "")
}
func (a *APIRegistrar) handleDocumentUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handleDocumentDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

func (a *APIRegistrar) handleAgentAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, document.TypeAgent)
}
func (a *APIRegistrar) handleAgentUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

func (a *APIRegistrar) handleContractAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, document.TypeContract)
}
func (a *APIRegistrar) handleContractUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handleContractDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

func (a *APIRegistrar) handlePrincipleAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, document.TypePrinciple)
}
func (a *APIRegistrar) handlePrincipleUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handlePrincipleDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

func (a *APIRegistrar) handleReviewerAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, document.TypeReviewer)
}
func (a *APIRegistrar) handleReviewerUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handleReviewerDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

func (a *APIRegistrar) handleRoleAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, document.TypeRole)
}
func (a *APIRegistrar) handleRoleUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handleRoleDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

func (a *APIRegistrar) handleSkillAdd(w http.ResponseWriter, r *http.Request) {
	a.runDocumentAdd(w, r, document.TypeSkill)
}
func (a *APIRegistrar) handleSkillUpdate(w http.ResponseWriter, r *http.Request) {
	a.runDocumentUpdate(w, r)
}
func (a *APIRegistrar) handleSkillDelete(w http.ResponseWriter, r *http.Request) {
	a.runDocumentDelete(w, r)
}

// ifEmpty returns fallback when s is empty, otherwise s. Used for default
// values without overwriting caller-supplied non-empty input.
func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// _ silences unused import warnings until a verb in this file uses auth
// directly. Removed when handleSystemSeedRun lands in the Tier B follow-up.
var _ = auth.CallerIdentity{}

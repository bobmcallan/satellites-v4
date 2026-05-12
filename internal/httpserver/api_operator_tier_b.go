// api_operator_tier_b.go — Tier B operator-tier mutating verbs added
// under sty_0b419d98. Each has substantive business logic the Tier A
// CRUD slice deferred.
//
// Tier B scope (6 verbs):
//   agent_apikey_create, agent_apikey_list, agent_apikey_delete
//   project_seed_run, system_seed_run
//   document_ingest_file
//
// Deferred to a follow-up: portal_replicate (chromedp + vocabulary
// plumbing). agent_compose was originally Tier B; covered here too
// since the business logic is just document.Create with skill_refs
// validation.

package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// registerOperatorTierBRoutes wires the Tier B mutating routes.
func (a *APIRegistrar) registerOperatorTierBRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/agent/apikey-create", a.handleAgentAPIKeyCreate)
	mux.HandleFunc("POST /api/v1/agent/apikey-list", a.handleAgentAPIKeyList)
	mux.HandleFunc("POST /api/v1/agent/apikey-delete", a.handleAgentAPIKeyDelete)
	mux.HandleFunc("POST /api/v1/agent/compose", a.handleAgentCompose)
	mux.HandleFunc("POST /api/v1/project/seed-run", a.handleProjectSeedRun)
	mux.HandleFunc("POST /api/v1/system/seed-run", a.handleSystemSeedRun)
	mux.HandleFunc("POST /api/v1/document/ingest-file", a.handleDocumentIngestFile)
}

// ----- agent_apikey -----

func (a *APIRegistrar) handleAgentAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.APIKeys == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "agent_apikey: store not configured")
		return
	}
	var req struct {
		Name      string `json:"name"`
		ProjectID string `json:"project_id"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Name == "" {
		writeAPIStatus(w, http.StatusBadRequest, "name is required")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeAPIStatus(w, http.StatusBadRequest, "expires_at must be RFC3339")
			return
		}
		tt := t.UTC()
		expiresAt = &tt
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	workspaceID := ""
	if req.ProjectID != "" {
		workspaceID = a.client.ResolveProjectWorkspaceID(r.Context(), req.ProjectID)
		if workspaceID == "" {
			writeAPIStatus(w, http.StatusNotFound, "project_not_found")
			return
		}
		if !cc.GlobalAdmin && !apiKeyWorkspaceInMemberships(workspaceID, cc.Memberships) {
			writeAPIStatus(w, http.StatusForbidden, "caller is not a member of the project's workspace")
			return
		}
	} else {
		workspaceID = a.client.ResolveCallerWorkspaceID(r.Context(), cc)
	}
	cleartext, salt, err := auth.GenerateAPIKey()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	now := time.Now().UTC()
	row := auth.APIKey{
		ID:          auth.NewAPIKeyID(),
		WorkspaceID: workspaceID,
		ProjectID:   req.ProjectID,
		OwnerUserID: cc.UserID,
		Name:        req.Name,
		Prefix:      auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:     auth.HashAPIKey(salt, cleartext),
		KeySalt:     salt,
		Status:      auth.APIKeyStatusActive,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
	}
	if err := stores.APIKeys.Create(r.Context(), row); err != nil {
		writeAPIError(w, err)
		return
	}
	auditPayload, _ := json.Marshal(map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"prefix":        row.Prefix,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"actor":         cc.UserID,
	})
	if stores.Ledger != nil {
		_, _ = stores.Ledger.Append(r.Context(), ledger.LedgerEntry{
			WorkspaceID: workspaceID,
			ProjectID:   req.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-apikey-created", "apikey:" + row.ID},
			Content:     "agent api-key minted",
			Structured:  auditPayload,
			CreatedBy:   cc.UserID,
		}, now)
	}
	writeAPIJSON(w, map[string]any{
		"id":            row.ID,
		"key":           cleartext,
		"prefix":        row.Prefix,
		"name":          row.Name,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"status":        row.Status,
		"expires_at":    row.ExpiresAt,
		"created_at":    row.CreatedAt,
	})
}

func (a *APIRegistrar) handleAgentAPIKeyList(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.APIKeys == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "agent_apikey: store not configured")
		return
	}
	var req struct {
		ProjectID       string `json:"project_id"`
		IncludeArchived bool   `json:"include_archived"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	owner := cc.UserID
	if cc.GlobalAdmin {
		owner = ""
	}
	rows, err := stores.APIKeys.List(r.Context(), owner, req.ProjectID, req.IncludeArchived)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, k := range rows {
		out = append(out, map[string]any{
			"id":            k.ID,
			"workspace_id":  k.WorkspaceID,
			"project_id":    k.ProjectID,
			"owner_user_id": k.OwnerUserID,
			"name":          k.Name,
			"prefix":        k.Prefix,
			"status":        k.Status,
			"expires_at":    k.ExpiresAt,
			"last_used_at":  k.LastUsedAt,
			"created_at":    k.CreatedAt,
		})
	}
	writeAPIJSON(w, map[string]any{
		"items":      out,
		"count":      len(out),
		"project_id": req.ProjectID,
	})
}

func (a *APIRegistrar) handleAgentAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	stores := a.client.Stores()
	if stores.APIKeys == nil {
		writeAPIStatus(w, http.StatusServiceUnavailable, "agent_apikey: store not configured")
		return
	}
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
	row, err := stores.APIKeys.Get(r.Context(), req.ID)
	if err != nil {
		if errors.Is(err, auth.ErrAPIKeyNotFound) {
			writeAPIStatus(w, http.StatusNotFound, "agent_apikey_not_found")
			return
		}
		writeAPIError(w, err)
		return
	}
	if row.OwnerUserID != cc.UserID && !cc.GlobalAdmin {
		writeAPIStatus(w, http.StatusForbidden, "caller does not own the api-key")
		return
	}
	if row.Status == auth.APIKeyStatusArchived {
		writeAPIJSON(w, map[string]any{"id": row.ID, "status": row.Status})
		return
	}
	if err := stores.APIKeys.Delete(r.Context(), req.ID); err != nil {
		writeAPIError(w, err)
		return
	}
	now := time.Now().UTC()
	auditPayload, _ := json.Marshal(map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"prefix":        row.Prefix,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"actor":         cc.UserID,
	})
	if stores.Ledger != nil {
		_, _ = stores.Ledger.Append(r.Context(), ledger.LedgerEntry{
			WorkspaceID: row.WorkspaceID,
			ProjectID:   row.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-apikey-archived", "apikey:" + row.ID, "actor:" + cc.UserID},
			Content:     "agent api-key archived",
			Structured:  auditPayload,
			CreatedBy:   cc.UserID,
		}, now)
	}
	writeAPIJSON(w, map[string]any{"id": row.ID, "status": auth.APIKeyStatusArchived})
}

// apiKeyWorkspaceInMemberships is the membership-check helper. Mirrors
// the package-private one in api_operator.go's neighbours.
func apiKeyWorkspaceInMemberships(ws string, mems []string) bool {
	for _, m := range mems {
		if m == ws {
			return true
		}
	}
	return false
}

// ----- agent_compose -----

var hookPatternPrefixes = []string{
	"Read:", "Edit:", "Write:", "MultiEdit:", "NotebookEdit:",
	"Glob", "Grep", "TodoWrite", "Task", "ToolSearch",
	"AskUserQuestion", "BashOutput", "KillShell",
	"Bash:", "mcp__",
}

func isRecognisedPattern(p string) bool {
	if p == "" {
		return false
	}
	for _, pref := range hookPatternPrefixes {
		if strings.HasPrefix(p, pref) {
			return true
		}
	}
	return false
}

func (a *APIRegistrar) handleAgentCompose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name               string   `json:"name"`
		ProjectID          string   `json:"project_id"`
		Body               string   `json:"body"`
		SkillRefs          []string `json:"skill_refs"`
		PermissionPatterns []string `json:"permission_patterns"`
		Ephemeral          bool     `json:"ephemeral"`
		StoryID            string   `json:"story_id"`
		Reason             string   `json:"reason"`
		Tags               []string `json:"tags"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Name == "" {
		writeAPIStatus(w, http.StatusBadRequest, "name required")
		return
	}
	if req.Ephemeral && req.StoryID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "story_id is required when ephemeral=true")
		return
	}
	for i, p := range req.PermissionPatterns {
		if !isRecognisedPattern(p) {
			writeAPIStatus(w, http.StatusBadRequest, "unknown_permission_pattern at index "+itoa(i)+": "+p)
			return
		}
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Documents == nil {
		writeAPIError(w, errors.New("document store not configured"))
		return
	}
	// Validate skill_refs resolve to active type=skill documents.
	for i, sid := range req.SkillRefs {
		d, err := stores.Documents.GetByID(r.Context(), sid, nil)
		if err != nil {
			writeAPIStatus(w, http.StatusBadRequest, "unknown_skill_ref at index "+itoa(i)+": "+sid)
			return
		}
		if d.Type != document.TypeSkill || d.Status != document.StatusActive {
			writeAPIStatus(w, http.StatusBadRequest, "skill_ref at index "+itoa(i)+" is not an active skill: "+sid)
			return
		}
	}
	now := time.Now().UTC()
	scope := document.ScopeProject
	if req.ProjectID == "" {
		scope = document.ScopeSystem
	}
	resolvedProj := ""
	wsID := ""
	if scope == document.ScopeProject {
		var perr error
		resolvedProj, perr = a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
		if perr != nil {
			writeAPIError(w, perr)
			return
		}
		wsID = a.client.ResolveProjectWorkspaceID(r.Context(), resolvedProj)
	}
	tags := append([]string{}, req.Tags...)
	if req.Ephemeral {
		tags = append(tags, "ephemeral", "story:"+req.StoryID)
	}
	structured, _ := json.Marshal(map[string]any{
		"skill_refs":          req.SkillRefs,
		"permission_patterns": req.PermissionPatterns,
		"ephemeral":           req.Ephemeral,
		"story_id":            req.StoryID,
		"reason":              req.Reason,
	})
	doc := document.Document{
		WorkspaceID: wsID,
		Type:        document.TypeAgent,
		Scope:       scope,
		Name:        req.Name,
		Body:        req.Body,
		Tags:        tags,
		Structured:  structured,
		Status:      document.StatusActive,
		CreatedBy:   cc.UserID,
		UpdatedBy:   cc.UserID,
	}
	if scope == document.ScopeProject {
		doc.ProjectID = document.StringPtr(resolvedProj)
	}
	created, err := stores.Documents.Create(r.Context(), doc, now)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if stores.Ledger != nil {
		auditPayload, _ := json.Marshal(map[string]any{
			"agent_id":            created.ID,
			"name":                created.Name,
			"skill_refs":          req.SkillRefs,
			"permission_patterns": req.PermissionPatterns,
			"ephemeral":           req.Ephemeral,
			"story_id":            req.StoryID,
			"actor":               cc.UserID,
		})
		ledgerWsID := wsID
		_, _ = stores.Ledger.Append(r.Context(), ledger.LedgerEntry{
			WorkspaceID: ledgerWsID,
			ProjectID:   resolvedProj,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-compose", "agent:" + created.ID},
			Content:     "agent composed",
			Structured:  auditPayload,
			CreatedBy:   cc.UserID,
		}, now)
	}
	writeAPIJSON(w, created)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+(i%10))) + digits
		i /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// ----- project_seed_run -----

func (a *APIRegistrar) handleProjectSeedRun(w http.ResponseWriter, r *http.Request) {
	cc := a.clientCaller(r)
	if !cc.GlobalAdmin {
		writeAPIStatus(w, http.StatusForbidden, "forbidden")
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ProjectID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "project_id required")
		return
	}
	stores := a.client.Stores()
	if stores.Projects == nil {
		writeAPIError(w, errors.New("project store not wired"))
		return
	}
	proj, err := stores.Projects.GetByID(r.Context(), req.ProjectID, nil)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "project not found")
		return
	}
	now := time.Now().UTC()
	summary, sumErr := configseed.RunProject(r.Context(),
		stores.Documents,
		configseed.ResolveSeedDir(),
		req.ProjectID, proj.WorkspaceID, cc.UserID, now)
	result := map[string]any{
		"project_id": req.ProjectID,
		"loaded":     summary.Loaded,
		"created":    summary.Created,
		"updated":    summary.Updated,
		"skipped":    summary.Skipped,
		"errors":     summary.Errors,
		"started_at": now,
	}
	if stores.Ledger != nil {
		structured, _ := json.Marshal(result)
		row, _ := stores.Ledger.Append(r.Context(), ledger.LedgerEntry{
			WorkspaceID: proj.WorkspaceID,
			ProjectID:   req.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:project-seed-run"},
			Content:     "project seed run",
			Structured:  structured,
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			CreatedBy:   cc.UserID,
		}, now)
		result["ledger_id"] = row.ID
	}
	if sumErr != nil {
		result["error"] = sumErr.Error()
	}
	writeAPIJSON(w, result)
}

// ----- system_seed_run -----

func (a *APIRegistrar) handleSystemSeedRun(w http.ResponseWriter, r *http.Request) {
	cc := a.clientCaller(r)
	if !cc.GlobalAdmin {
		writeAPIStatus(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := decodeJSONBody(r, &struct{}{}); err != nil {
		writeAPIError(w, err)
		return
	}
	stores := a.client.Stores()
	now := time.Now().UTC()
	workspaceID := ""
	if stores.Workspaces != nil {
		if list, err := stores.Workspaces.ListByMember(r.Context(), "system"); err == nil && len(list) > 0 {
			workspaceID = list[0].ID
		}
	}
	summary, sumErr := configseed.RunAll(r.Context(),
		stores.Documents,
		configseed.ResolveSeedDir(),
		configseed.ResolveHelpDir(),
		workspaceID, cc.UserID, now)
	result := map[string]any{
		"loaded":     summary.Loaded,
		"created":    summary.Created,
		"updated":    summary.Updated,
		"skipped":    summary.Skipped,
		"errors":     summary.Errors,
		"started_at": now,
	}
	if stores.Ledger != nil {
		structured, _ := json.Marshal(result)
		row, _ := stores.Ledger.Append(r.Context(), ledger.LedgerEntry{
			WorkspaceID: workspaceID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:system-seed-run"},
			Content:     "system seed run",
			Structured:  structured,
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			CreatedBy:   cc.UserID,
		}, now)
		result["ledger_id"] = row.ID
	}
	if sumErr != nil {
		result["error"] = sumErr.Error()
	}
	writeAPIJSON(w, result)
}

// ----- document_ingest_file -----

func (a *APIRegistrar) handleDocumentIngestFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path      string `json:"path"`
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Path == "" {
		writeAPIStatus(w, http.StatusBadRequest, "path required")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Documents == nil {
		writeAPIError(w, errors.New("document store not configured"))
		return
	}
	resolvedID, err := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), resolvedID)
	if wsID == "" {
		wsID = a.client.ResolveCallerWorkspaceID(r.Context(), cc)
	}
	res, err := document.IngestFile(r.Context(), stores.Documents, stores.Logger, wsID, resolvedID, stores.DocsDir, req.Path, time.Now().UTC())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, map[string]any{
		"id":         res.Document.ID,
		"project_id": resolvedID,
		"name":       res.Document.Name,
		"version":    res.Document.Version,
		"changed":    res.Changed,
		"created":    res.Created,
	})
}

// api.go — HTTP API surface for satellites-server.
//
// Each route POST /api/v1/<noun>/<verb> decodes its JSON body into a
// per-verb wire shape, threads the AuthMiddleware-resolved caller
// through *client.Client typed methods, and renders the typed
// output as JSON. The resolution helpers (memberships, project_id,
// workspace_id) live on *client.Client (sty_068a6c46 Slice A);
// the wire layer is thin.
//
// Ships under sty_068a6c46 (cli-primary order:07a-layer-3). Parity
// with the existing /mcp surface is asserted by the integration test
// in tests/api/api_integration_test.go (AC-4).

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// APIRegistrar attaches POST /api/v1/<noun>/<verb> handlers to a
// mux. Constructed once at boot in cmd/satellites-server; satisfies
// the httpserver.RouteRegistrar interface so the existing
// New(...registrars...) wiring picks it up.
type APIRegistrar struct {
	client *client.Client
}

// NewAPIRegistrar binds the registrar to a constructed typed client.
// Caller constructs the client.Client with the same Deps the MCP
// server uses (story DocStore/ProjectStore/...).
func NewAPIRegistrar(c *client.Client) *APIRegistrar {
	return &APIRegistrar{client: c}
}

// Register attaches the 20 verb routes to the supplied mux. The mux
// is wrapped by auth.AuthMiddleware at the cmd/satellites-server
// layer; routes here assume the caller is resolved on the request
// context.
func (a *APIRegistrar) Register(mux *http.ServeMux) {
	// Read verbs.
	mux.HandleFunc("POST /api/v1/satellites/info", a.handleSatellitesInfo)
	mux.HandleFunc("POST /api/v1/session/whoami", a.handleSessionWhoami)
	mux.HandleFunc("POST /api/v1/session/register", a.handleSessionRegister)
	mux.HandleFunc("POST /api/v1/ledger/get", a.handleLedgerGet)
	mux.HandleFunc("POST /api/v1/ledger/list", a.handleLedgerList)
	mux.HandleFunc("POST /api/v1/ledger/search", a.handleLedgerSearch)
	mux.HandleFunc("POST /api/v1/ledger/recall", a.handleLedgerRecall)
	mux.HandleFunc("POST /api/v1/ledger/append", a.handleLedgerAppend)
	mux.HandleFunc("POST /api/v1/ledger/dereference", a.handleLedgerDereference)
	mux.HandleFunc("POST /api/v1/document/get", a.handleDocumentGet)
	mux.HandleFunc("POST /api/v1/document/list", a.handleDocumentList)
	mux.HandleFunc("POST /api/v1/task/get", a.handleTaskGet)
	mux.HandleFunc("POST /api/v1/task/walk", a.handleTaskWalk)
	mux.HandleFunc("POST /api/v1/task/claim", a.handleTaskClaim)
	mux.HandleFunc("POST /api/v1/task/update", a.handleTaskUpdate)
	mux.HandleFunc("POST /api/v1/task/add", a.handleTaskAdd)
	mux.HandleFunc("POST /api/v1/task/plan", a.handleTaskPlan)
	mux.HandleFunc("POST /api/v1/story/get", a.handleStoryGet)
	mux.HandleFunc("POST /api/v1/story/update-status", a.handleStoryUpdateStatus)
	mux.HandleFunc("POST /api/v1/story/field-set", a.handleStoryFieldSet)
	mux.HandleFunc("POST /api/v1/project/set", a.handleProjectSet)

	// Operator-tier read routes (sty_ef248ab2).
	a.registerOperatorRoutes(mux)
	// Operator-tier write routes Tier A (sty_f38bd573).
	a.registerOperatorWriteRoutes(mux)
}

// clientCaller builds the typed client.Caller from the AuthMiddleware-
// resolved identity. Memberships are left nil — handlers that need
// them call a.client.ResolveCallerMemberships explicitly.
func (a *APIRegistrar) clientCaller(r *http.Request) client.Caller {
	id, _ := auth.UserFrom(r.Context())
	return client.Caller{
		UserID:      id.UserID,
		Email:       id.Email,
		GlobalAdmin: id.GlobalAdmin,
	}
}

// decodeJSONBody decodes the request body into v. An empty body is
// permitted (verbs with no params accept an empty {} or no body at
// all). A non-empty body that fails to decode returns an error.
func decodeJSONBody(r *http.Request, v any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// resolveScopedAndMemberships threads the per-request resolution chain:
// caller-from-ctx → memberships → resolved project_id (when
// requestedProject is non-empty) → workspace_id. Returns the resolved
// project + workspace + caller (with memberships populated).
//
// The HTTP API path passes "" for scopedProjectID — URL-based project
// scoping is an /mcp-only concern.
func (a *APIRegistrar) resolveScopedAndMemberships(r *http.Request, requestedProject string) (resolvedProjectID, workspaceID string, cc client.Caller, err error) {
	cc = a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	if requestedProject != "" || true {
		resolvedProjectID, err = a.client.ResolveProjectID(r.Context(), requestedProject, "", cc, cc.Memberships)
		if err != nil {
			return "", "", cc, err
		}
		workspaceID = a.client.ResolveProjectWorkspaceID(r.Context(), resolvedProjectID)
	}
	return resolvedProjectID, workspaceID, cc, nil
}

// ----- handlers -----

func (a *APIRegistrar) handleSatellitesInfo(w http.ResponseWriter, r *http.Request) {
	cc := a.clientCaller(r)
	out, err := a.client.SatellitesInfo(r.Context(), cc, client.SatellitesInfoInput{})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleSessionWhoami(w http.ResponseWriter, r *http.Request) {
	cc := a.clientCaller(r)
	out, err := a.client.SessionWhoami(r.Context(), cc, client.SessionWhoamiInput{})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleSessionRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
		SessionID string `json:"session_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	resolvedProj, _ := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), resolvedProj)
	out, err := a.client.SessionRegister(r.Context(), cc, client.SessionRegisterInput{
		ProjectID:   resolvedProj,
		WorkspaceID: wsID,
		SessionID:   req.SessionID,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleLedgerGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.LedgerGet(r.Context(), cc, client.LedgerGetInput{
		ID:          req.ID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleLedgerList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID           string   `json:"project_id"`
		StoryID             string   `json:"story_id"`
		Type                string   `json:"type"`
		Tags                []string `json:"tags"`
		Durability          string   `json:"durability"`
		SourceType          string   `json:"source_type"`
		Status              string   `json:"status"`
		Sensitive           string   `json:"sensitive"`
		IncludeDereferenced bool     `json:"include_dereferenced"`
		Limit               int      `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	projectID, _, cc, err := a.resolveScopedAndMemberships(r, req.ProjectID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	opts := ledger.ListOptions{
		StoryID:       req.StoryID,
		Type:          req.Type,
		Tags:          req.Tags,
		Durability:    req.Durability,
		SourceType:    req.SourceType,
		Status:        req.Status,
		IncludeDerefd: req.IncludeDereferenced,
		Limit:         req.Limit,
	}
	if req.Sensitive == "true" {
		v := true
		opts.Sensitive = &v
	} else if req.Sensitive == "false" {
		v := false
		opts.Sensitive = &v
	}
	out, err := a.client.LedgerList(r.Context(), cc, client.LedgerListInput{
		ResolvedProjectID: projectID,
		Options:           opts,
		Memberships:       cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleLedgerSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string   `json:"project_id"`
		Query     string   `json:"query"`
		Tags      []string `json:"tags"`
		Limit     int      `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	projectID, _, cc, err := a.resolveScopedAndMemberships(r, req.ProjectID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	searchOpts := ledger.SearchOptions{Query: req.Query, TopK: req.Limit}
	searchOpts.Tags = req.Tags
	searchOpts.Limit = req.Limit
	out, err := a.client.LedgerSearch(r.Context(), cc, client.LedgerSearchInput{
		ResolvedProjectID: projectID,
		Memberships:       cc.Memberships,
		Options:           searchOpts,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleLedgerRecall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RootID string `json:"root_id"`
		Limit  int    `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.LedgerRecall(r.Context(), cc, client.LedgerRecallInput{
		RootID:      req.RootID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleLedgerAppend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID  string          `json:"project_id"`
		StoryID    string          `json:"story_id"`
		Type       string          `json:"type"`
		Content    string          `json:"content"`
		Tags       []string        `json:"tags"`
		Durability string          `json:"durability"`
		SourceType string          `json:"source_type"`
		Sensitive  bool            `json:"sensitive"`
		Structured json.RawMessage `json:"structured"`
		ExpiresAt  string          `json:"expires_at"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Type == "" {
		writeAPIError(w, errors.New("type required"))
		return
	}
	projectID, wsID, cc, err := a.resolveScopedAndMemberships(r, req.ProjectID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	in := client.LedgerAppendInput{
		ResolvedProjectID: projectID,
		WorkspaceID:       wsID,
		StoryID:           req.StoryID,
		EventType:         req.Type,
		Content:           req.Content,
		Tags:              req.Tags,
		Durability:        req.Durability,
		SourceType:        req.SourceType,
		Sensitive:         req.Sensitive,
		Structured:        req.Structured,
		Now:               time.Now().UTC(),
	}
	if req.ExpiresAt != "" {
		t, perr := time.Parse(time.RFC3339, req.ExpiresAt)
		if perr != nil {
			writeAPIError(w, errors.New("expires_at must be RFC3339"))
			return
		}
		in.ExpiresAt = &t
	}
	if cc.GlobalAdmin && wsID != "" && !client.WorkspaceInMemberships(wsID, cc.Memberships) {
		in.ImpersonatingAsWorkspace = wsID
	}
	out, err := a.client.LedgerAppend(r.Context(), cc, in)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleLedgerDereference(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.LedgerDereference(r.Context(), cc, client.LedgerDereferenceInput{
		ID:          req.ID,
		Reason:      req.Reason,
		Memberships: cc.Memberships,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleDocumentGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	resolvedProj, _ := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	wsID := a.client.ResolveProjectWorkspaceID(r.Context(), resolvedProj)
	out, err := a.client.DocumentGet(r.Context(), cc, client.DocumentGetInput{
		ID:                req.ID,
		Name:              req.Name,
		Type:              req.Type,
		WorkspaceID:       wsID,
		ResolvedProjectID: resolvedProj,
		Memberships:       cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleDocumentList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type        string `json:"type"`
		Status      string `json:"status"`
		ProjectID   string `json:"project_id"`
		WorkspaceID string `json:"workspace_id"`
		Scope       string `json:"scope"`
		Limit       int    `json:"limit"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	resolvedProj, _ := a.client.ResolveProjectID(r.Context(), req.ProjectID, "", cc, cc.Memberships)
	wsID := req.WorkspaceID
	if wsID == "" {
		wsID = a.client.ResolveProjectWorkspaceID(r.Context(), resolvedProj)
	}
	opts := document.ListOptions{
		Type:      req.Type,
		ProjectID: resolvedProj,
		Scope:     req.Scope,
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

func (a *APIRegistrar) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.TaskGet(r.Context(), cc, client.TaskGetInput{
		ID:          req.ID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleTaskWalk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StoryID string `json:"story_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.TaskWalk(r.Context(), cc, client.TaskWalkInput{
		StoryID:     req.StoryID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID    string `json:"worker_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	workspaceIDs := cc.Memberships
	if req.WorkspaceID != "" {
		workspaceIDs = []string{req.WorkspaceID}
	}
	out, err := a.client.TaskClaim(r.Context(), cc, client.TaskClaimInput{
		WorkerID:     req.WorkerID,
		WorkspaceIDs: workspaceIDs,
		Memberships:  cc.Memberships,
		Now:          time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleTaskUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID                string   `json:"id"`
		Status            string   `json:"status"`
		Outcome           string   `json:"outcome"`
		EvidenceLedgerIDs []string `json:"evidence_ledger_ids"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.TaskUpdate(r.Context(), cc, client.TaskUpdateInput{
		ID:                req.ID,
		Status:            req.Status,
		Outcome:           req.Outcome,
		EvidenceLedgerIDs: req.EvidenceLedgerIDs,
		Memberships:       cc.Memberships,
		Now:               time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleTaskAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID  string `json:"agent_id"`
		Prompt   string `json:"prompt"`
		StoryID  string `json:"story_id"`
		Kind     string `json:"kind"`
		Action   string `json:"action"`
		Priority string `json:"priority"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	resolve := client.TaskAddResolveDeps{
		CallerActiveProjectID: func(ctx context.Context, c client.Caller) string {
			id, _ := a.client.ResolveProjectID(ctx, "", "", c, c.Memberships)
			return id
		},
		ResolveStoryProjectID: func(ctx context.Context, storyID string, memberships []string) string {
			if storyID == "" {
				return ""
			}
			s := a.client.Stores().Stories
			if s == nil {
				return ""
			}
			st, err := s.GetByID(ctx, storyID, memberships)
			if err != nil {
				return ""
			}
			return st.ProjectID
		},
		ResolveProjectWorkspaceID: func(ctx context.Context, projectID string) string {
			return a.client.ResolveProjectWorkspaceID(ctx, projectID)
		},
	}
	out, err := a.client.TaskAdd(r.Context(), cc, client.TaskAddInput{
		AgentID:     req.AgentID,
		Prompt:      req.Prompt,
		StoryID:     req.StoryID,
		Kind:        req.Kind,
		Action:      req.Action,
		Priority:    req.Priority,
		Memberships: cc.Memberships,
		Resolve:     resolve,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleTaskPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Origin           string          `json:"origin"`
		WorkspaceID      string          `json:"workspace_id"`
		ProjectID        string          `json:"project_id"`
		Kind             string          `json:"kind"`
		AgentID          string          `json:"agent_id"`
		ParentTaskID     string          `json:"parent_task_id"`
		PriorTaskID      string          `json:"prior_task_id"`
		Priority         string          `json:"priority"`
		Trigger          json.RawMessage `json:"trigger"`
		ExpectedDuration string          `json:"expected_duration"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	projectID := req.ProjectID
	if projectID == "" {
		projectID, _ = a.client.ResolveProjectID(r.Context(), "", "", cc, cc.Memberships)
	}
	wsID := req.WorkspaceID
	if wsID == "" {
		wsID = a.client.ResolveProjectWorkspaceID(r.Context(), projectID)
	}
	in := client.TaskPlanInput{
		Origin:       req.Origin,
		WorkspaceID:  wsID,
		ProjectID:    projectID,
		Kind:         req.Kind,
		AgentID:      req.AgentID,
		ParentTaskID: req.ParentTaskID,
		PriorTaskID:  req.PriorTaskID,
		Priority:     req.Priority,
		Trigger:      []byte(req.Trigger),
		Memberships:  cc.Memberships,
		Now:          time.Now().UTC(),
	}
	if req.ExpectedDuration != "" {
		d, derr := time.ParseDuration(req.ExpectedDuration)
		if derr != nil {
			writeAPIError(w, errors.New("expected_duration must be a Go duration string"))
			return
		}
		in.ExpectedDuration = d
	}
	out, err := a.client.TaskPlan(r.Context(), cc, in)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleStoryGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.StoryGet(r.Context(), cc, client.StoryGetInput{
		ID:          req.ID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleStoryUpdateStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.StoryUpdateStatus(r.Context(), cc, client.StoryUpdateStatusInput{
		ID:          req.ID,
		Status:      req.Status,
		Memberships: cc.Memberships,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleStoryFieldSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Field string `json:"field"`
		Value string `json:"value"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.StoryFieldSet(r.Context(), cc, client.StoryFieldSetInput{
		ID:          req.ID,
		Field:       req.Field,
		Value:       req.Value,
		Memberships: cc.Memberships,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

func (a *APIRegistrar) handleProjectSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoURL   string `json:"repo_url"`
		SessionID string `json:"session_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	out, err := a.client.ProjectSet(r.Context(), cc, client.ProjectSetInput{
		RepoURL:   req.RepoURL,
		SessionID: req.SessionID,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// Compile-time guard that APIRegistrar satisfies RouteRegistrar.
var _ RouteRegistrar = (*APIRegistrar)(nil)

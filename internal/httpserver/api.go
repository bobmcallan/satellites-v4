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
//
// Every /api/v1/* registration flows through a.handle() which wraps
// the handler with the dispatched-tool-call telemetry middleware
// (sty_090f6183). The middleware is a no-op when the request lacks
// the X-Satellites-Dispatched-Task-ID header.
func (a *APIRegistrar) Register(mux *http.ServeMux) {
	// Read verbs.
	a.handle(mux, "POST /api/v1/satellites/info", a.handleSatellitesInfo)
	a.handle(mux, "POST /api/v1/satellites/init", a.handleSatellitesInit)
	a.handle(mux, "POST /api/v1/substrate/audit", a.handleSubstrateAudit)
	a.handle(mux, "POST /api/v1/system/version", a.handleSystemVersion)
	a.handle(mux, "POST /api/v1/session/whoami", a.handleSessionWhoami)
	a.handle(mux, "POST /api/v1/session/register", a.handleSessionRegister)
	a.handle(mux, "POST /api/v1/ledger/get", a.handleLedgerGet)
	a.handle(mux, "POST /api/v1/ledger/list", a.handleLedgerList)
	a.handle(mux, "POST /api/v1/ledger/search", a.handleLedgerSearch)
	a.handle(mux, "POST /api/v1/ledger/recall", a.handleLedgerRecall)
	a.handle(mux, "POST /api/v1/ledger/append", a.handleLedgerAppend)
	a.handle(mux, "POST /api/v1/ledger/dereference", a.handleLedgerDereference)
	a.handle(mux, "POST /api/v1/document/get", a.handleDocumentGet)
	a.handle(mux, "POST /api/v1/document/list", a.handleDocumentList)
	a.handle(mux, "POST /api/v1/task/get", a.handleTaskGet)
	a.handle(mux, "POST /api/v1/task/walk", a.handleTaskWalk)
	a.handle(mux, "POST /api/v1/task/claim", a.handleTaskClaim)
	a.handle(mux, "POST /api/v1/task/update", a.handleTaskUpdate)
	a.handle(mux, "POST /api/v1/task/add", a.handleTaskAdd)
	// sty_8c17b89d: task_log routes — append + list are thin
	// forwarders; stream is the SSE long-lived-connection carve-out.
	a.handle(mux, "POST /api/v1/task/log/append", a.handleTaskLogAppend)
	a.handle(mux, "POST /api/v1/task/log/list", a.handleTaskLogList)
	mux.HandleFunc("GET /api/v1/task/log/stream", a.handleTaskLogStream)
	a.handle(mux, "POST /api/v1/story/get", a.handleStoryGet)
	// /api/v1/story/update-status + /api/v1/story/field-set routes removed
	// in sty_4db0e025 slice D1 — both folded into /api/v1/story/update
	// (handleStoryUpdate accepts status + fields). pr_story_terminal_gate
	// is preserved via client.StoryUpdate → MemoryStore.UpdateStatus.
	a.handle(mux, "POST /api/v1/project/set", a.handleProjectSet)

	// Operator-tier read routes (sty_ef248ab2).
	a.registerOperatorRoutes(mux)
	// Operator-tier write routes Tier A (sty_f38bd573).
	a.registerOperatorWriteRoutes(mux)
	// Operator-tier Tier B mutates (sty_0b419d98). portal_replicate
	// remains deferred.
	a.registerOperatorTierBRoutes(mux)
}

// dispatchedTaskIDHeader names the inbound HTTP header dispatched
// agents stamp on every /api/v1/* request so the satellites-server
// can publish KindToolCallStart / KindToolCallEnd telemetry against
// the originating task_log stream (sty_090f6183).
const dispatchedTaskIDHeader = "X-Satellites-Dispatched-Task-ID"

// handle registers a /api/v1/* HandleFunc wrapped with the
// dispatched-tool-call telemetry middleware. The middleware extracts
// the tool name from the URL path (e.g. POST /api/v1/task/claim →
// "task_claim") and emits KindToolCallStart before the handler runs
// and KindToolCallEnd after — both only when the inbound request
// carries the dispatched-task-id header. The middleware delegates
// emission to client.Client.PublishTaskLog so the wire layer never
// imports internal/tasklog directly (pr_mcp_cli_shared_path).
//
// sty_056b68f6: every handler is ALSO wrapped with the verb-allowlist
// gate (`gateHTTPVerbs`). The gate is in-process: it reads
// CallerIdentity off ctx, calls auth.AssertVerbAllowed, and short-
// circuits with the JSON envelope on rejection. Project-scoped keys
// / OAuth / session callers pass through as a no-op. The wrapping
// order is `gate(telemetry(handler))` so a denied call never emits a
// KindToolCallStart row.
func (a *APIRegistrar) handle(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	toolName := deriveToolName(pattern)
	mux.HandleFunc(pattern, gateHTTPVerbs(toolName, a.withDispatchedToolCallTelemetry(toolName, h)))
}

// gateHTTPVerbs wraps next with the verb-allowlist gate. The gate
// reads CallerIdentity.AllowedVerbs off ctx; nil means the caller is
// un-narrowed (project-scoped key / OAuth / session) and the
// wrapper passes through. Task-scoped callers run through
// auth.AssertVerbAllowed; out-of-set verbs return HTTP 403 + the
// wire envelope `{"error":"verb_not_in_allowlist","verb":"<v>","allowed":[…]}`.
// sty_056b68f6.
func gateHTTPVerbs(verb string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if denied := auth.AssertVerbAllowed(r.Context(), verb); denied != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(denied.Body()))
			return
		}
		next.ServeHTTP(w, r)
	}
}

// deriveToolName turns "POST /api/v1/task/log/append" into
// "task_log_append" so the telemetry payload mirrors the verb name
// the cliremote.Call surface uses.
func deriveToolName(pattern string) string {
	// Strip method prefix.
	p := pattern
	if i := strings.Index(p, " "); i >= 0 {
		p = p[i+1:]
	}
	p = strings.TrimPrefix(p, "/api/v1/")
	p = strings.Trim(p, "/")
	return strings.ReplaceAll(p, "/", "_")
}

// withDispatchedToolCallTelemetry wraps next with the
// KindToolCallStart / KindToolCallEnd emit pair. Returns next
// unwrapped when the inbound request has no
// dispatchedTaskIDHeader — the production cost on operator-tier
// API hits is exactly one header lookup.
func (a *APIRegistrar) withDispatchedToolCallTelemetry(toolName string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := strings.TrimSpace(r.Header.Get(dispatchedTaskIDHeader))
		if taskID == "" {
			next.ServeHTTP(w, r)
			return
		}
		a.client.PublishTaskLog(r.Context(), taskID, client.TaskLogKindToolCallStart, map[string]any{
			"tool_name":    toolName,
			"args_summary": "",
		})
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)
		errMsg := ""
		if rec.status >= 400 {
			errMsg = http.StatusText(rec.status)
		}
		a.client.PublishTaskLog(r.Context(), taskID, client.TaskLogKindToolCallEnd, map[string]any{
			"tool_name":   toolName,
			"duration_ms": duration.Milliseconds(),
			"error":       errMsg,
		})
	}
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
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	id, _ := auth.UserFrom(r.Context())
	out, err := a.client.SatellitesInfo(r.Context(), cc, client.SatellitesInfoInput{
		SessionID: r.Header.Get("Mcp-Session-Id"),
		AuthKind:  id.Source,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// handleSystemVersion forwards to the typed SystemVersion method on
// *client.Client (sty_64e69db8). The typed surface owns the manifest
// fetch + TTL cache; this handler only decodes the request, threads
// the caller, and writes the JSON payload.
func (a *APIRegistrar) handleSystemVersion(w http.ResponseWriter, r *http.Request) {
	cc := a.clientCaller(r)
	out, err := a.client.SystemVersion(r.Context(), cc, client.SystemVersionInput{})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// handleSubstrateAudit forwards to the typed SubstrateAudit method
// (sty_2f0db922). The typed surface owns substrate_auditor resolution
// + task-mint; this handler decodes optional project_id / workspace_id
// args, threads the caller's resolution chain, and writes the JSON
// payload.
func (a *APIRegistrar) handleSubstrateAudit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID   string `json:"project_id"`
		WorkspaceID string `json:"workspace_id"`
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
	out, err := a.client.SubstrateAudit(r.Context(), cc, client.SubstrateAuditInput{
		ProjectID:   req.ProjectID,
		WorkspaceID: req.WorkspaceID,
		Memberships: cc.Memberships,
		Resolve:     resolve,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// handleSatellitesInit forwards to the typed SatellitesInit method
// (sty_796b8fe1). The typed surface owns manifest projection + state
// machine; this handler decodes optional caller args (current_version,
// os, arch) and writes the JSON payload.
func (a *APIRegistrar) handleSatellitesInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentVersion string `json:"current_version"`
		OS             string `json:"os"`
		Arch           string `json:"arch"`
		AgentName      string `json:"agent_name"`
		ProjectID      string `json:"project_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	// HTTP callers can opt into the apikey-mint flow by supplying
	// project_id explicitly (the MCP path resolves it from the
	// session's project_set state). sty_6b1e207a slice B.
	resolvedProjectID := strings.TrimSpace(req.ProjectID)
	out, err := a.client.SatellitesInit(r.Context(), cc, client.SatellitesInitInput{
		CurrentVersion:    req.CurrentVersion,
		OS:                req.OS,
		Arch:              req.Arch,
		AgentName:         req.AgentName,
		ResolvedProjectID: resolvedProjectID,
		Memberships:       cc.Memberships,
	})
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
	ctx := client.WithOriginVerb(r.Context(), "ledger_list")
	out, err := a.client.LedgerList(ctx, cc, client.LedgerListInput{
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
	ctx := client.WithOriginVerb(r.Context(), "document_get")
	out, err := a.client.DocumentGet(ctx, cc, client.DocumentGetInput{
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
	ctx := client.WithOriginVerb(r.Context(), "document_list")
	out, err := a.client.DocumentList(ctx, cc, client.DocumentListInput{
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
	ctx := client.WithOriginVerb(r.Context(), "task_walk")
	out, err := a.client.TaskWalk(ctx, cc, client.TaskWalkInput{
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
		PriorTaskID       *string  `json:"prior_task_id"`
		ParentTaskID      *string  `json:"parent_task_id"`
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
		PriorTaskID:       req.PriorTaskID,
		ParentTaskID:      req.ParentTaskID,
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
		AgentID      string `json:"agent_id"`
		Prompt       string `json:"prompt"`
		StoryID      string `json:"story_id"`
		Kind         string `json:"kind"`
		Action       string `json:"action"`
		Priority     string `json:"priority"`
		PriorTaskID  string `json:"prior_task_id"`
		ParentTaskID string `json:"parent_task_id"`
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
		AgentID:      req.AgentID,
		Prompt:       req.Prompt,
		StoryID:      req.StoryID,
		Kind:         req.Kind,
		Action:       req.Action,
		Priority:     req.Priority,
		PriorTaskID:  req.PriorTaskID,
		ParentTaskID: req.ParentTaskID,
		Memberships:  cc.Memberships,
		Resolve:      resolve,
		Now:          time.Now().UTC(),
	})
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
	ctx := client.WithOriginVerb(r.Context(), "story_get")
	out, err := a.client.StoryGet(ctx, cc, client.StoryGetInput{
		ID:          req.ID,
		Memberships: cc.Memberships,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}

// handleStoryUpdateStatus + handleStoryFieldSet HTTP handlers were
// removed in sty_4db0e025 slice D1 — both folded into handleStoryUpdate
// (now living in api_operator_writes.go), which routes through
// client.StoryUpdate. The consolidated client method preserves
// pr_story_terminal_gate by calling MemoryStore.UpdateStatus when a
// status transition is requested.

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
	ctx := client.WithOriginVerb(r.Context(), "project_set")
	out, err := a.client.ProjectSet(ctx, cc, client.ProjectSetInput{
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

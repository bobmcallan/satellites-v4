// Package mcpserver exposes the satellites MCP surface over Streamable HTTP.
// v4 currently registers: satellites_info, document_ingest_file, document_get,
// project_create/get/list, ledger_append/list, story_create/get/list/update_status,
// workspace_create/get/list. Subsequent epics add more.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// Server bundles the mcp-go MCPServer + StreamableHTTPServer with the
// satellites-specific dependencies needed by the tools.
type Server struct {
	cfg              *config.Config
	logger           arbor.ILogger
	startedAt        time.Time
	mcp              *mcpserver.MCPServer
	streamable       *mcpserver.StreamableHTTPServer
	docs             document.Store
	docsDir          string
	projects         project.Store
	defaultProjectID string
	ledger           ledger.Store
	stories          story.Store
	workspaces       workspace.Store
}

// Deps bundles the optional per-tool dependencies passed through to
// handlers. A nil store field disables the associated verbs.
type Deps struct {
	DocStore         document.Store
	DocsDir          string
	ProjectStore     project.Store
	DefaultProjectID string
	LedgerStore      ledger.Store
	StoryStore       story.Store
	WorkspaceStore   workspace.Store
}

// New constructs the MCP server with the satellites_info tool registered.
// Stateless mode is required because Fly rolling deploys move clients
// between machines (see memory note project_mcp_stateless).
func New(cfg *config.Config, logger arbor.ILogger, startedAt time.Time, deps Deps) *Server {
	s := &Server{
		cfg:              cfg,
		logger:           logger,
		startedAt:        startedAt,
		docs:             deps.DocStore,
		docsDir:          deps.DocsDir,
		projects:         deps.ProjectStore,
		defaultProjectID: deps.DefaultProjectID,
		ledger:           deps.LedgerStore,
		stories:          deps.StoryStore,
		workspaces:       deps.WorkspaceStore,
	}

	s.mcp = mcpserver.NewMCPServer(
		"satellites",
		config.Version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions("Satellites v4 — walking skeleton."),
	)

	infoTool := mcpgo.NewTool("satellites_info",
		mcpgo.WithDescription("Return the satellites server's version metadata and the calling user's identity."),
	)
	s.mcp.AddTool(infoTool, s.handleInfo)

	if s.docs != nil {
		ingestTool := mcpgo.NewTool("document_ingest_file",
			mcpgo.WithDescription("Ingest a file from the server's DOCS_DIR into the document store. Path is repo-relative; server reads the file and upserts by (project_id, filename). If project_id is omitted, defaults to the caller's first owned project or the system default."),
			mcpgo.WithString("path",
				mcpgo.Required(),
				mcpgo.Description("Repo-relative path inside DOCS_DIR."),
			),
			mcpgo.WithString("project_id",
				mcpgo.Description("Optional project scope. Defaults to caller's first owned project or the system default."),
			),
		)
		s.mcp.AddTool(ingestTool, s.handleDocumentIngestFile)

		getTool := mcpgo.NewTool("document_get",
			mcpgo.WithDescription("Return the stored document body by (project_id, filename)."),
			mcpgo.WithString("filename",
				mcpgo.Required(),
				mcpgo.Description("Document filename (e.g. architecture.md)."),
			),
			mcpgo.WithString("project_id",
				mcpgo.Description("Optional project scope. Defaults to caller's first owned project or the system default."),
			),
		)
		s.mcp.AddTool(getTool, s.handleDocumentGet)
	}

	if s.projects != nil {
		createTool := mcpgo.NewTool("project_create",
			mcpgo.WithDescription("Create a new project owned by the caller."),
			mcpgo.WithString("name",
				mcpgo.Required(),
				mcpgo.Description("Project display name."),
			),
		)
		s.mcp.AddTool(createTool, s.handleProjectCreate)

		getProjTool := mcpgo.NewTool("project_get",
			mcpgo.WithDescription("Return a project the caller owns. Cross-owner access returns not-found."),
			mcpgo.WithString("id",
				mcpgo.Required(),
				mcpgo.Description("Project id (proj_<8hex>)."),
			),
		)
		s.mcp.AddTool(getProjTool, s.handleProjectGet)

		listProjTool := mcpgo.NewTool("project_list",
			mcpgo.WithDescription("List the caller's projects, newest-first."),
		)
		s.mcp.AddTool(listProjTool, s.handleProjectList)
	}

	if s.ledger != nil {
		appendTool := mcpgo.NewTool("ledger_append",
			mcpgo.WithDescription("Append an event row to the project's ledger. Caller must own the project."),
			mcpgo.WithString("project_id",
				mcpgo.Required(),
				mcpgo.Description("Project scope (caller must own it, or it must be the system default)."),
			),
			mcpgo.WithString("type",
				mcpgo.Required(),
				mcpgo.Description("Event type, e.g. 'story.status_change'."),
			),
			mcpgo.WithString("content",
				mcpgo.Description("Event content / free-form payload."),
			),
		)
		s.mcp.AddTool(appendTool, s.handleLedgerAppend)

		listLedgerTool := mcpgo.NewTool("ledger_list",
			mcpgo.WithDescription("List ledger entries for a project, newest-first. Caller must own the project."),
			mcpgo.WithString("project_id",
				mcpgo.Required(),
				mcpgo.Description("Project scope."),
			),
			mcpgo.WithString("type",
				mcpgo.Description("Optional type filter."),
			),
			mcpgo.WithNumber("limit",
				mcpgo.Description("Max entries to return (default 100, max 500)."),
			),
		)
		s.mcp.AddTool(listLedgerTool, s.handleLedgerList)
	}

	if s.stories != nil {
		createStoryTool := mcpgo.NewTool("story_create",
			mcpgo.WithDescription("Create a new story in a project the caller owns."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project scope.")),
			mcpgo.WithString("title", mcpgo.Required(), mcpgo.Description("Short story title.")),
			mcpgo.WithString("description", mcpgo.Description("Full description.")),
			mcpgo.WithString("acceptance_criteria", mcpgo.Description("What done looks like.")),
			mcpgo.WithString("priority", mcpgo.Description("critical | high | medium | low")),
			mcpgo.WithString("category", mcpgo.Description("feature | bug | improvement | infrastructure | documentation")),
			mcpgo.WithArray("tags", mcpgo.Description("Free-form tags (e.g. epic:v4-stories)."),
				mcpgo.Items(map[string]any{"type": "string"})),
		)
		s.mcp.AddTool(createStoryTool, s.handleStoryCreate)

		getStoryTool := mcpgo.NewTool("story_get",
			mcpgo.WithDescription("Return a story by id. Cross-project access returns not-found."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id (sty_<8hex>).")),
		)
		s.mcp.AddTool(getStoryTool, s.handleStoryGet)

		listStoryTool := mcpgo.NewTool("story_list",
			mcpgo.WithDescription("List stories in a project. Supports status, priority, and tag filters."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project scope.")),
			mcpgo.WithString("status", mcpgo.Description("Status filter.")),
			mcpgo.WithString("priority", mcpgo.Description("Priority filter.")),
			mcpgo.WithString("tag", mcpgo.Description("Tag filter (e.g. epic:v4-stories).")),
			mcpgo.WithNumber("limit", mcpgo.Description("Max stories (default 100, max 500).")),
		)
		s.mcp.AddTool(listStoryTool, s.handleStoryList)

		updateStatusTool := mcpgo.NewTool("story_update_status",
			mcpgo.WithDescription("Transition a story to a new status. Emits a story.status_change ledger row. Valid transitions: backlog→ready→in_progress→done, or ←→cancelled from any non-terminal."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id.")),
			mcpgo.WithString("status", mcpgo.Required(), mcpgo.Description("Target status: ready | in_progress | done | cancelled.")),
		)
		s.mcp.AddTool(updateStatusTool, s.handleStoryUpdateStatus)
	}

	if s.workspaces != nil {
		createWsTool := mcpgo.NewTool("workspace_create",
			mcpgo.WithDescription("Create a new workspace and add the caller as admin. The caller must be authenticated."),
			mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Workspace display name.")),
		)
		s.mcp.AddTool(createWsTool, s.handleWorkspaceCreate)

		getWsTool := mcpgo.NewTool("workspace_get",
			mcpgo.WithDescription("Return a workspace the caller is a member of. Non-member access returns not-found."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Workspace id (wksp_<8hex>).")),
		)
		s.mcp.AddTool(getWsTool, s.handleWorkspaceGet)

		listWsTool := mcpgo.NewTool("workspace_list",
			mcpgo.WithDescription("List the caller's member workspaces, newest-first."),
		)
		s.mcp.AddTool(listWsTool, s.handleWorkspaceList)
	}

	s.streamable = mcpserver.NewStreamableHTTPServer(s.mcp,
		mcpserver.WithStateLess(true),
	)
	return s
}

// ServeHTTP implements http.Handler. AuthMiddleware is responsible for
// establishing the user context before this handler runs.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.streamable.ServeHTTP(w, r)
}

// handleInfo is the satellites_info tool implementation.
func (s *Server) handleInfo(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	var userEmail string
	if u, ok := UserFrom(ctx); ok {
		userEmail = u.Email
	}
	payload := map[string]any{
		"version":    config.Version,
		"build":      config.Build,
		"commit":     config.GitCommit,
		"user_email": userEmail,
		"started_at": s.startedAt.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "satellites_info").
		Str("user_email", userEmail).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// resolveProjectID picks the document-operation project scope for the
// caller. Rules: (1) if req supplies project_id, the caller must own that
// project or it must be the system default; cross-project access returns
// an error. (2) otherwise, fall back to the caller's first owned project.
// (3) otherwise, fall back to the system default.
func (s *Server) resolveProjectID(ctx context.Context, requested string, caller CallerIdentity, memberships []string) (string, error) {
	if requested != "" {
		if requested == s.defaultProjectID {
			return requested, nil
		}
		p, err := s.projectsSafe().GetByID(ctx, requested, memberships)
		if err != nil {
			return "", errors.New("project not found or access denied")
		}
		if p.OwnerUserID != caller.UserID {
			return "", errors.New("project not found or access denied")
		}
		return requested, nil
	}
	if s.projects != nil && caller.UserID != "" {
		list, err := s.projects.ListByOwner(ctx, caller.UserID, memberships)
		if err == nil && len(list) > 0 {
			return list[0].ID, nil
		}
	}
	if s.defaultProjectID != "" {
		return s.defaultProjectID, nil
	}
	return "", errors.New("no project context available")
}

// projectsSafe returns the project store, or a zero-value implementation
// when the server was constructed without one. The MCP tool registrations
// already gate project_* on non-nil projects; this is a safety net for
// document_* callers that somehow arrive with a requested project_id when
// projects are disabled.
func (s *Server) projectsSafe() project.Store {
	if s.projects != nil {
		return s.projects
	}
	return project.NewMemoryStore()
}

// ensureCallerWorkspaces returns the caller's member-workspace ids, minting
// a default workspace on first sight via workspace.EnsureDefault (matches
// the OnUserCreated hook for human logins, and covers synthetic callers
// like API keys that didn't flow through the auth bootstrap path). Returns
// nil when the workspace store is disabled (pre-tenant mode). Empty slice
// only when the caller is unauthenticated.
func (s *Server) ensureCallerWorkspaces(ctx context.Context, caller CallerIdentity) []string {
	if s.workspaces == nil {
		return nil
	}
	if caller.UserID == "" {
		return []string{}
	}
	list, err := s.workspaces.ListByMember(ctx, caller.UserID)
	if err != nil {
		return []string{}
	}
	if len(list) == 0 {
		if _, err := workspace.EnsureDefault(ctx, s.workspaces, s.logger, caller.UserID, time.Now().UTC()); err == nil {
			list, _ = s.workspaces.ListByMember(ctx, caller.UserID)
		}
	}
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, w.ID)
	}
	return out
}

// resolveCallerWorkspaceID returns the caller's default workspace id, or
// empty when the caller is unauthenticated or the workspace store is off.
// Write paths use this to stamp workspace_id on new rows.
func (s *Server) resolveCallerWorkspaceID(ctx context.Context, caller CallerIdentity) string {
	ids := s.ensureCallerWorkspaces(ctx, caller)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// resolveCallerMemberships returns the caller's memberships slice as the
// store reads expect: nil when the workspace store is disabled (pre-tenant
// behaviour), empty slice when the caller has no membership yet (deny-all),
// non-empty workspace ids otherwise. See docs/architecture.md §8.
func (s *Server) resolveCallerMemberships(ctx context.Context, caller CallerIdentity) []string {
	return s.ensureCallerWorkspaces(ctx, caller)
}

// resolveProjectWorkspaceID returns the workspace_id of the given project,
// or empty when the project has none yet (legacy path before backfill).
// This helper reads with a nil memberships filter because it's used on the
// write path to cascade workspace_id onto children; the caller-facing read
// scoping is applied by the handler that called resolveProjectID first.
func (s *Server) resolveProjectWorkspaceID(ctx context.Context, projectID string) string {
	if s.projects == nil || projectID == "" {
		return ""
	}
	p, err := s.projects.GetByID(ctx, projectID, nil)
	if err != nil {
		return ""
	}
	return p.WorkspaceID
}

func (s *Server) handleDocumentIngestFile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	path, err := req.RequireString("path")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	projectID := req.GetString("project_id", "")
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	if wsID == "" {
		wsID = s.resolveCallerWorkspaceID(ctx, caller)
	}
	res, err := document.IngestFile(ctx, s.docs, s.logger, wsID, resolvedID, s.docsDir, path, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	payload := map[string]any{
		"id":         res.Document.ID,
		"project_id": res.Document.ProjectID,
		"filename":   res.Document.Filename,
		"version":    res.Document.Version,
		"changed":    res.Changed,
		"created":    res.Created,
	}
	body, _ := json.Marshal(payload)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_ingest_file").
		Str("project_id", resolvedID).
		Str("filename", res.Document.Filename).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	filename, err := req.RequireString("filename")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	projectID := req.GetString("project_id", "")
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	doc, err := s.docs.GetByFilename(ctx, resolvedID, filename, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(doc)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_get").
		Str("project_id", resolvedID).
		Str("filename", filename).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveCallerWorkspaceID(ctx, caller)
	p, err := s.projects.Create(ctx, caller.UserID, wsID, name, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(p)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_create").
		Str("project_id", p.ID).
		Str("owner_user_id", p.OwnerUserID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	p, err := s.projects.GetByID(ctx, id, memberships)
	if err != nil || p.OwnerUserID != caller.UserID {
		return mcpgo.NewToolResultError("project not found"), nil
	}
	body, _ := json.Marshal(p)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_get").
		Str("project_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	list, err := s.projects.ListByOwner(ctx, caller.UserID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(list)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_list").
		Str("owner_user_id", caller.UserID).
		Int("count", len(list)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerAppend(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	eventType, err := req.RequireString("type")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	content := req.GetString("content", "")
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	e, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: wsID,
		ProjectID:   resolvedID,
		Type:        eventType,
		Content:     content,
		Actor:       caller.UserID,
	}, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(e)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "ledger_append").
		Str("project_id", resolvedID).
		Str("event_type", eventType).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleStoryCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	tagsRaw := req.GetStringSlice("tags", nil)
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	st, err := s.stories.Create(ctx, story.Story{
		WorkspaceID:        wsID,
		ProjectID:          resolvedID,
		Title:              title,
		Description:        req.GetString("description", ""),
		AcceptanceCriteria: req.GetString("acceptance_criteria", ""),
		Priority:           req.GetString("priority", "medium"),
		Category:           req.GetString("category", "feature"),
		Tags:               tagsRaw,
		CreatedBy:          caller.UserID,
	}, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(st)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_create").
		Str("project_id", resolvedID).
		Str("story_id", st.ID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleStoryGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	st, err := s.stories.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	// Owner check is project-scoped: the caller must own the story's project.
	if _, err := s.resolveProjectID(ctx, st.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	body, _ := json.Marshal(st)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_get").
		Str("story_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleStoryList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	opts := story.ListOptions{
		Status:   req.GetString("status", ""),
		Priority: req.GetString("priority", ""),
		Tag:      req.GetString("tag", ""),
		Limit:    int(req.GetFloat("limit", 0)),
	}
	list, err := s.stories.List(ctx, resolvedID, opts, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(list)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_list").
		Str("project_id", resolvedID).
		Int("count", len(list)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleStoryUpdateStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	status, err := req.RequireString("status")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	existing, err := s.stories.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, existing.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	updated, err := s.stories.UpdateStatus(ctx, id, status, caller.UserID, time.Now().UTC(), memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_update_status").
		Str("story_id", id).
		Str("new_status", status).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	w, err := s.workspaces.Create(ctx, caller.UserID, name, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(w)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_create").
		Str("workspace_id", w.ID).
		Str("owner_user_id", w.OwnerUserID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	is, err := s.workspaces.IsMember(ctx, id, caller.UserID)
	if err != nil || !is {
		return mcpgo.NewToolResultError("workspace not found"), nil
	}
	w, err := s.workspaces.GetByID(ctx, id)
	if err != nil {
		return mcpgo.NewToolResultError("workspace not found"), nil
	}
	body, _ := json.Marshal(w)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_get").
		Str("workspace_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	list, err := s.workspaces.ListByMember(ctx, caller.UserID)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(list)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_list").
		Str("user_id", caller.UserID).
		Int("count", len(list)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	opts := ledger.ListOptions{
		Type:  req.GetString("type", ""),
		Limit: int(req.GetFloat("limit", 0)),
	}
	entries, err := s.ledger.List(ctx, resolvedID, opts, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(entries)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "ledger_list").
		Str("project_id", resolvedID).
		Str("type_filter", opts.Type).
		Int("count", len(entries)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

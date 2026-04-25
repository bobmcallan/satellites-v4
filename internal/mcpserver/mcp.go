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
	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/jcodemunch"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/reviewer"
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
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
	contracts        contract.Store
	sessions         session.Store
	reviewer         reviewer.Reviewer
	grants           rolegrant.Store
	tasks            task.Store
	repos            repo.Store
	jcodemunch       jcodemunch.Client
	nowFunc          func() time.Time
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
	ContractStore    contract.Store
	SessionStore     session.Store
	Reviewer         reviewer.Reviewer
	// RoleGrantStore is optional; nil disables the agent_role_* MCP
	// verbs and forces the grant middleware into pass-through mode even
	// when Config.GrantsEnforced is true. Story_1efbfc48.
	RoleGrantStore rolegrant.Store
	// TaskStore is optional; nil disables the task_* MCP verbs.
	// Story_a8fee0cc.
	TaskStore task.Store
	// RepoStore is optional; nil disables the repo_* MCP verbs.
	// Story_970ddfa1.
	RepoStore repo.Store
	// JcodemunchClient is the proxy used by the repo_* search/get verbs.
	// Nil falls back to jcodemunch.NewStub(), which returns a structured
	// "jcodemunch_unavailable" error for every call. Production wires a
	// real HTTP/MCP adapter.
	JcodemunchClient jcodemunch.Client
	// NowFunc is the optional clock source for handlers. Tests inject a
	// frozen clock so session-staleness fixtures stay deterministic
	// (story_3ae6621b). Production callers leave it nil and the server
	// falls back to time.Now().UTC().
	NowFunc func() time.Time
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
		contracts:        deps.ContractStore,
		sessions:         deps.SessionStore,
		reviewer:         deps.Reviewer,
		grants:           deps.RoleGrantStore,
		tasks:            deps.TaskStore,
		repos:            deps.RepoStore,
		jcodemunch:       deps.JcodemunchClient,
		nowFunc:          deps.NowFunc,
	}
	if s.reviewer == nil {
		s.reviewer = reviewer.AcceptAll{}
	}
	if s.jcodemunch == nil {
		s.jcodemunch = jcodemunch.NewStub()
	}

	serverOpts := []mcpserver.ServerOption{
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions("Satellites v4 — walking skeleton."),
	}
	// Grant middleware is always installed; it's a pass-through unless
	// Config.GrantsEnforced is true AND the RoleGrantStore is wired.
	serverOpts = append(serverOpts, mcpserver.WithToolHandlerMiddleware(s.grantMiddleware()))

	s.mcp = mcpserver.NewMCPServer(
		"satellites",
		config.Version,
		serverOpts...,
	)

	infoTool := mcpgo.NewTool("satellites_info",
		mcpgo.WithDescription("Return the satellites server's version metadata and the calling user's identity."),
	)
	s.mcp.AddTool(infoTool, s.handleInfo)

	if s.docs != nil {
		ingestTool := mcpgo.NewTool("document_ingest_file",
			mcpgo.WithDescription("Ingest a file from the server's DOCS_DIR into the document store. Path is repo-relative; server reads the file and upserts by (project_id, name). If project_id is omitted, defaults to the caller's first owned project or the system default."),
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
			mcpgo.WithDescription("Return a stored document by id (preferred) or by (project_id, name). When both are supplied, id wins."),
			mcpgo.WithString("id",
				mcpgo.Description("Document id (doc_<8hex>). When supplied, name + project_id are ignored."),
			),
			mcpgo.WithString("name",
				mcpgo.Description("Document name. Used only when id is omitted."),
			),
			mcpgo.WithString("project_id",
				mcpgo.Description("Optional project scope for name-keyed lookups. Defaults to caller's first owned project or the system default."),
			),
		)
		s.mcp.AddTool(getTool, s.handleDocumentGet)

		createTool := mcpgo.NewTool("document_create",
			mcpgo.WithDescription("Create a new document. Workspace is resolved from the caller; project_id is required when scope=project and forbidden when scope=system."),
			mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("artifact | contract | skill | principle | reviewer")),
			mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("system | project")),
			mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Document name.")),
			mcpgo.WithString("project_id", mcpgo.Description("Project scope. Required when scope=project; rejected when scope=system.")),
			mcpgo.WithString("body", mcpgo.Description("Markdown body.")),
			mcpgo.WithString("structured", mcpgo.Description("Type-specific JSON payload (raw JSON string).")),
			mcpgo.WithString("contract_binding", mcpgo.Description("Document id of an active type=contract row. Required for type=skill or type=reviewer; forbidden otherwise.")),
			mcpgo.WithArray("tags", mcpgo.Description("Free-form tags."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("status", mcpgo.Description("active (default) | archived")),
		)
		s.mcp.AddTool(createTool, s.handleDocumentCreate)

		updateTool := mcpgo.NewTool("document_update",
			mcpgo.WithDescription("Patch the mutable fields of a document. Immutable fields (id, workspace_id, project_id, type, scope, name) are rejected."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Document id (doc_<8hex>).")),
			mcpgo.WithString("body", mcpgo.Description("Markdown body.")),
			mcpgo.WithString("structured", mcpgo.Description("Type-specific JSON payload (raw JSON string).")),
			mcpgo.WithArray("tags", mcpgo.Description("Replace the tag set."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("status", mcpgo.Description("active | archived")),
			mcpgo.WithString("contract_binding", mcpgo.Description("Document id of an active type=contract row.")),
		)
		s.mcp.AddTool(updateTool, s.handleDocumentUpdate)

		listTool := mcpgo.NewTool("document_list",
			mcpgo.WithDescription("List documents in the caller's workspaces, filtered by type/scope/tags/contract_binding/project_id. Workspace scoping is enforced at the handler."),
			mcpgo.WithString("type", mcpgo.Description("Filter by type.")),
			mcpgo.WithString("scope", mcpgo.Description("Filter by scope.")),
			mcpgo.WithString("project_id", mcpgo.Description("Filter by project. Defaults to all visible projects.")),
			mcpgo.WithString("contract_binding", mcpgo.Description("Filter by contract_binding (skill/reviewer rows bound to a contract id).")),
			mcpgo.WithArray("tags", mcpgo.Description("Filter by tags (any-of)."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithNumber("limit", mcpgo.Description("Max rows to return (server caps at 500).")),
		)
		s.mcp.AddTool(listTool, s.handleDocumentList)

		deleteTool := mcpgo.NewTool("document_delete",
			mcpgo.WithDescription("Archive (default) or hard-delete a document."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Document id.")),
			mcpgo.WithString("mode", mcpgo.Description("archive (default) | hard")),
		)
		s.mcp.AddTool(deleteTool, s.handleDocumentDelete)

		s.registerDocumentWrappers()

		searchTool := mcpgo.NewTool("document_search",
			mcpgo.WithDescription("Search documents in the caller's workspaces. Combines structured filters (type/scope/tags/contract_binding/project_id) with a case-insensitive substring match on name + body when query is supplied. Empty query + at least one filter returns an updated_at DESC list. Workspace scoping is enforced at the handler."),
			mcpgo.WithString("query", mcpgo.Description("Free-text query; case-insensitive substring on name + body.")),
			mcpgo.WithString("type", mcpgo.Description("Filter by type.")),
			mcpgo.WithString("scope", mcpgo.Description("Filter by scope.")),
			mcpgo.WithString("project_id", mcpgo.Description("Filter by project.")),
			mcpgo.WithString("contract_binding", mcpgo.Description("Filter by contract_binding.")),
			mcpgo.WithArray("tags", mcpgo.Description("Filter by tags (any-of)."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithNumber("top_k", mcpgo.Description("Max rows to return (default 20, capped at 100).")),
		)
		s.mcp.AddTool(searchTool, s.handleDocumentSearch)
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
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project scope.")),
			mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("Event type per architecture.md §6 enum (plan|action_claim|artifact|evidence|decision|close-request|verdict|workflow-claim|kv); other strings are wrapped as Type=decision with the original value preserved as a kind:<value> tag.")),
			mcpgo.WithString("content", mcpgo.Description("Event content / free-form markdown.")),
			mcpgo.WithString("story_id", mcpgo.Description("Optional story FK.")),
			mcpgo.WithString("contract_id", mcpgo.Description("Optional contract FK.")),
			mcpgo.WithArray("tags", mcpgo.Description("Free-form tags."), mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("structured", mcpgo.Description("Type-specific JSON payload (raw JSON string).")),
			mcpgo.WithString("durability", mcpgo.Description("ephemeral | pipeline | durable (default).")),
			mcpgo.WithString("expires_at", mcpgo.Description("RFC3339 timestamp; required when durability=ephemeral.")),
			mcpgo.WithString("source_type", mcpgo.Description("manifest | feedback | agent (default) | user | system | migration.")),
			mcpgo.WithBoolean("sensitive", mcpgo.Description("Marks the row as sensitive — visible only to its author.")),
		)
		s.mcp.AddTool(appendTool, s.handleLedgerAppend)

		listLedgerTool := mcpgo.NewTool("ledger_list",
			mcpgo.WithDescription("List ledger entries for a project, newest-first. Caller must own the project. Default excludes status=dereferenced unless overridden via status or include_dereferenced."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project scope.")),
			mcpgo.WithString("type", mcpgo.Description("Filter by type (architecture.md §6 enum).")),
			mcpgo.WithString("story_id", mcpgo.Description("Filter by story FK.")),
			mcpgo.WithString("contract_id", mcpgo.Description("Filter by contract FK.")),
			mcpgo.WithArray("tags", mcpgo.Description("Filter by tags (any-of)."), mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("durability", mcpgo.Description("Filter by durability.")),
			mcpgo.WithString("source_type", mcpgo.Description("Filter by source_type.")),
			mcpgo.WithString("status", mcpgo.Description("Filter by status (active | archived | dereferenced).")),
			mcpgo.WithBoolean("sensitive", mcpgo.Description("Filter by sensitive flag.")),
			mcpgo.WithBoolean("include_dereferenced", mcpgo.Description("Include dereferenced rows in the default-status branch.")),
			mcpgo.WithNumber("limit", mcpgo.Description("Max entries to return (default 100, max 500).")),
		)
		s.mcp.AddTool(listLedgerTool, s.handleLedgerList)

		getLedgerTool := mcpgo.NewTool("ledger_get",
			mcpgo.WithDescription("Return a ledger row by id. Workspace-membership enforced."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Ledger entry id (ldg_<8hex>).")),
		)
		s.mcp.AddTool(getLedgerTool, s.handleLedgerGet)

		searchLedgerTool := mcpgo.NewTool("ledger_search",
			mcpgo.WithDescription("Search ledger rows. Combines structured filters with a case-insensitive substring match on content when query is supplied. Empty query + filter returns updated_at DESC."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project scope.")),
			mcpgo.WithString("query", mcpgo.Description("Free-text query.")),
			mcpgo.WithString("type", mcpgo.Description("Filter by type.")),
			mcpgo.WithString("story_id", mcpgo.Description("Filter by story FK.")),
			mcpgo.WithString("contract_id", mcpgo.Description("Filter by contract FK.")),
			mcpgo.WithArray("tags", mcpgo.Description("Filter by tags (any-of)."), mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("durability", mcpgo.Description("Filter by durability.")),
			mcpgo.WithString("source_type", mcpgo.Description("Filter by source_type.")),
			mcpgo.WithString("status", mcpgo.Description("Filter by status.")),
			mcpgo.WithBoolean("include_dereferenced", mcpgo.Description("Include dereferenced rows.")),
			mcpgo.WithNumber("top_k", mcpgo.Description("Max rows (default 20, capped 100).")),
		)
		s.mcp.AddTool(searchLedgerTool, s.handleLedgerSearch)

		recallLedgerTool := mcpgo.NewTool("ledger_recall",
			mcpgo.WithDescription("Return the chain of ledger rows tagged recall_root:<root_id> plus the root row, ordered by created_at ASC. Used by contract claim/resume to load prior evidence."),
			mcpgo.WithString("root_id", mcpgo.Required(), mcpgo.Description("Root ledger entry id.")),
		)
		s.mcp.AddTool(recallLedgerTool, s.handleLedgerRecall)

		dereferenceLedgerTool := mcpgo.NewTool("ledger_dereference",
			mcpgo.WithDescription("Soft-retire a ledger row by flipping its status to 'dereferenced' and writing a kind:dereference audit row. The original row stays in the chain for audit; default queries hide it. Hard delete is not exposed (pr_root_cause)."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Ledger entry id to dereference.")),
			mcpgo.WithString("reason", mcpgo.Required(), mcpgo.Description("Why this row is being dereferenced. Recorded as the audit row's content.")),
		)
		s.mcp.AddTool(dereferenceLedgerTool, s.handleLedgerDereference)
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

	if s.contracts != nil && s.stories != nil && s.ledger != nil && s.docs != nil && s.projects != nil {
		specGetTool := mcpgo.NewTool("project_workflow_spec_get",
			mcpgo.WithDescription("Return the project's workflow_spec (ordered list of contract_name slots with min/max counts). Returns the default spec when none has been set."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project id.")),
		)
		s.mcp.AddTool(specGetTool, s.handleProjectWorkflowSpecGet)

		specSetTool := mcpgo.NewTool("project_workflow_spec_set",
			mcpgo.WithDescription("Persist a new workflow_spec for a project. Writes a kind:kv ledger row tagged key:workflow_spec; older rows remain in the audit chain (KVProjection reads the latest). Caller must own the project."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project id.")),
			mcpgo.WithString("slots", mcpgo.Required(), mcpgo.Description("JSON array of {contract_name, required, min_count, max_count, source} entries.")),
		)
		s.mcp.AddTool(specSetTool, s.handleProjectWorkflowSpecSet)

		workflowClaimTool := mcpgo.NewTool("story_workflow_claim",
			mcpgo.WithDescription("Lock a workflow shape for a story. Validates proposed_contracts against the project's workflow_spec, resolves each contract_name to a document{type=contract}, creates one contract_instance per slot (all status=ready), and writes a kind:workflow-claim ledger row. Idempotent: re-calling with an existing workflow returns the existing CIs."),
			mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story id.")),
			mcpgo.WithArray("proposed_contracts", mcpgo.Description("Ordered list of contract_name slots. When omitted, the project's workflow_spec is expanded using each required slot's min_count."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("claim_markdown", mcpgo.Description("Agent's workflow-shape rationale.")),
		)
		s.mcp.AddTool(workflowClaimTool, s.handleStoryWorkflowClaim)

		contractNextTool := mcpgo.NewTool("story_contract_next",
			mcpgo.WithDescription("Return the lowest-sequence contract_instance with status=ready for a story, plus any document{type=skill} rows whose contract_binding matches the contract's id. Read-only — does NOT claim."),
			mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story id.")),
		)
		s.mcp.AddTool(contractNextTool, s.handleStoryContractNext)

		if s.sessions != nil {
			claimTool := mcpgo.NewTool("story_contract_claim",
				mcpgo.WithDescription("Claim a contract instance — runs the process-order gate, verifies the session is registered + not stale, writes action-claim and optional plan ledger rows, and transitions the CI to claimed. Same-session re-claim is an amend (prior rows dereferenced; amended=true)."),
				mcpgo.WithString("contract_instance_id", mcpgo.Required(), mcpgo.Description("Contract instance id.")),
				mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Claude Code harness chat UUID — must be registered in the session registry.")),
				mcpgo.WithArray("permissions_claim", mcpgo.Description("Tool:pattern strings the agent needs during this claim."),
					mcpgo.Items(map[string]any{"type": "string"})),
				mcpgo.WithArray("skills_used", mcpgo.Description("Skill IDs / names the agent is applying (informational)."),
					mcpgo.Items(map[string]any{"type": "string"})),
				mcpgo.WithString("plan_markdown", mcpgo.Description("Optional plan markdown. Written as a kind:plan ledger row and stamped on the CI's PlanLedgerID.")),
			)
			s.mcp.AddTool(claimTool, s.handleStoryContractClaim)

			closeTool := mcpgo.NewTool("story_contract_close",
				mcpgo.WithDescription("Close a contract instance: writes a phase:close kind:close-request row, optional kind:evidence row, flips CI to passed, rolls the story to done when every required CI is terminal. On preplan close, proposed_workflow is validated against the project spec and written as a kind:workflow-claim row."),
				mcpgo.WithString("contract_instance_id", mcpgo.Required(), mcpgo.Description("Contract instance id.")),
				mcpgo.WithString("close_markdown", mcpgo.Description("Close summary markdown.")),
				mcpgo.WithString("evidence_markdown", mcpgo.Description("Optional evidence markdown; writes a kind:evidence row when non-empty.")),
				mcpgo.WithArray("evidence_ledger_ids", mcpgo.Description("IDs of prior evidence rows referenced from the close."),
					mcpgo.Items(map[string]any{"type": "string"})),
				mcpgo.WithString("plan_markdown", mcpgo.Description("Optional plan markdown — used when the CI was claimed without a plan (deferred plan path).")),
				mcpgo.WithArray("proposed_workflow", mcpgo.Description("Preplan-only: list of contract_names forming the remainder of the workflow."),
					mcpgo.Items(map[string]any{"type": "string"})),
			)
			s.mcp.AddTool(closeTool, s.handleStoryContractClose)

			respondTool := mcpgo.NewTool("story_contract_respond",
				mcpgo.WithDescription("Write a kind:review-response ledger row addressing the latest unresolved review-question on a CI. Reviewer re-invocation happens on the next close."),
				mcpgo.WithString("contract_instance_id", mcpgo.Required(), mcpgo.Description("Contract instance id.")),
				mcpgo.WithString("response_markdown", mcpgo.Required(), mcpgo.Description("Agent's response markdown.")),
			)
			s.mcp.AddTool(respondTool, s.handleStoryContractRespond)

			resumeTool := mcpgo.NewTool("story_contract_resume",
				mcpgo.WithDescription("Resume a CI. When the CI is claimed, rebinds the session. When the CI is passed, reopens it: flips it back to claimed, dereferences its prior plan + action-claim rows, and flips downstream required CIs back to ready. Enforces per-CI + per-story resume caps (SATELLITES_MAX_RESUMES_PER_CI / _PER_STORY)."),
				mcpgo.WithString("contract_instance_id", mcpgo.Required(), mcpgo.Description("Contract instance id.")),
				mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session to bind onto the CI.")),
				mcpgo.WithString("reason", mcpgo.Required(), mcpgo.Description("Human-readable reason written to the resume row.")),
			)
			s.mcp.AddTool(resumeTool, s.handleStoryContractResume)

			whoamiTool := mcpgo.NewTool("session_whoami",
				mcpgo.WithDescription("Return the caller's session registry row for the given session_id. Returns a structured session_not_registered error when the session is not in the registry."),
				mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session id to inspect.")),
			)
			s.mcp.AddTool(whoamiTool, s.handleSessionWhoami)

			registerTool := mcpgo.NewTool("session_register",
				mcpgo.WithDescription("Upsert a session row keyed by (caller_user_id, session_id). Called by the SessionStart hook + by tests. LastSeenAt is set to now."),
				mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session id to register.")),
				mcpgo.WithString("source", mcpgo.Description("Source string (session_start | enforce_hook | apikey). Defaults to session_start.")),
			)
			s.mcp.AddTool(registerTool, s.handleSessionRegister)
		}
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

		addMemberTool := mcpgo.NewTool("workspace_member_add",
			mcpgo.WithDescription("Add a user to a workspace at the given role. Caller must be an admin of the workspace."),
			mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.Description("Workspace id.")),
			mcpgo.WithString("user_id", mcpgo.Required(), mcpgo.Description("User id to add.")),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("admin | member | reviewer | viewer")),
		)
		s.mcp.AddTool(addMemberTool, s.handleWorkspaceMemberAdd)

		listMemberTool := mcpgo.NewTool("workspace_member_list",
			mcpgo.WithDescription("List members of a workspace. Caller must be a member (any role)."),
			mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.Description("Workspace id.")),
		)
		s.mcp.AddTool(listMemberTool, s.handleWorkspaceMemberList)

		updateRoleTool := mcpgo.NewTool("workspace_member_update_role",
			mcpgo.WithDescription("Change an existing member's role. Caller must be an admin. Downgrading the last admin is rejected."),
			mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.Description("Workspace id.")),
			mcpgo.WithString("user_id", mcpgo.Required(), mcpgo.Description("Target user id.")),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("New role.")),
		)
		s.mcp.AddTool(updateRoleTool, s.handleWorkspaceMemberUpdateRole)

		removeMemberTool := mcpgo.NewTool("workspace_member_remove",
			mcpgo.WithDescription("Remove a member from a workspace. Caller must be an admin. Removing the last admin is rejected."),
			mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.Description("Workspace id.")),
			mcpgo.WithString("user_id", mcpgo.Required(), mcpgo.Description("User id to remove.")),
		)
		s.mcp.AddTool(removeMemberTool, s.handleWorkspaceMemberRemove)
	}

	if s.grants != nil {
		claimTool := mcpgo.NewTool("agent_role_claim",
			mcpgo.WithDescription("Claim a role-grant binding a grantee (session/task/worker) to a role under an agent document. Validates role in agent.permitted_roles and that agent.tool_ceiling covers role.allowed_mcp_verbs. Writes a kind:role-grant,event:claimed ledger row. When provider_override=\"mechanical\" OR no agent-document resolves for the role, falls through to the deterministic mechanical runner and tags resulting ledger rows provider:mechanical (story_548ab5a5)."),
			mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.Description("Workspace scope for the grant.")),
			mcpgo.WithString("role_id", mcpgo.Required(), mcpgo.Description("Role document id (type=role).")),
			mcpgo.WithString("agent_id", mcpgo.Description("Agent document id (type=agent). Optional when provider_override=\"mechanical\" or when the server should auto-resolve.")),
			mcpgo.WithString("grantee_kind", mcpgo.Required(), mcpgo.Description("session | task | worker")),
			mcpgo.WithString("grantee_id", mcpgo.Required(), mcpgo.Description("Stable id for the grantee (chat UUID for session, task id for task, worker id for worker).")),
			mcpgo.WithString("project_id", mcpgo.Description("Optional project scope for the grant.")),
			mcpgo.WithString("provider_override", mcpgo.Description("Skip the provider chain when set to \"mechanical\"; routes to the deterministic runner directly.")),
		)
		s.mcp.AddTool(claimTool, s.handleAgentRoleClaim)

		releaseTool := mcpgo.NewTool("agent_role_release",
			mcpgo.WithDescription("Release an active grant. Idempotent: a second call on a released grant returns the released row and writes a redundant-release ledger entry without mutating status."),
			mcpgo.WithString("grant_id", mcpgo.Required(), mcpgo.Description("Grant id (grant_<8hex>).")),
			mcpgo.WithString("reason", mcpgo.Description("Free-form release reason (e.g. task_close, session_end).")),
		)
		s.mcp.AddTool(releaseTool, s.handleAgentRoleRelease)

		listTool := mcpgo.NewTool("agent_role_list",
			mcpgo.WithDescription("List role-grants matching the supplied filters. Workspace-scoped."),
			mcpgo.WithString("role_id", mcpgo.Description("Filter by role id.")),
			mcpgo.WithString("agent_id", mcpgo.Description("Filter by agent id.")),
			mcpgo.WithString("grantee_kind", mcpgo.Description("Filter by grantee kind.")),
			mcpgo.WithString("grantee_id", mcpgo.Description("Filter by grantee id.")),
			mcpgo.WithString("status", mcpgo.Description("active | released")),
			mcpgo.WithNumber("limit", mcpgo.Description("Max rows to return.")),
		)
		s.mcp.AddTool(listTool, s.handleAgentRoleList)
	}

	if s.tasks != nil {
		enqueueTool := mcpgo.NewTool("task_enqueue",
			mcpgo.WithDescription("Enqueue a new task. Writes a kind:task-enqueued ledger row. Returns {task_id, ledger_root_id}. Story_a8fee0cc."),
			mcpgo.WithString("origin", mcpgo.Required(), mcpgo.Description("story_stage | scheduled | story_producing | free_preplan | event")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Workspace scope. Defaults to caller's first membership.")),
			mcpgo.WithString("project_id", mcpgo.Description("Optional project scope.")),
			mcpgo.WithString("priority", mcpgo.Description("critical | high | medium (default) | low")),
			mcpgo.WithString("trigger", mcpgo.Description("Free-form JSON trigger payload.")),
			mcpgo.WithString("payload", mcpgo.Description("Free-form JSON task payload (contract_instance_id, story_id, ...).")),
			mcpgo.WithString("expected_duration", mcpgo.Description("Optional Go duration string (e.g. \"30s\") used by claim-expiry watchdog.")),
		)
		s.mcp.AddTool(enqueueTool, s.handleTaskEnqueue)

		getTaskTool := mcpgo.NewTool("task_get",
			mcpgo.WithDescription("Return a task by id. Workspace-scoped."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id.")),
		)
		s.mcp.AddTool(getTaskTool, s.handleTaskGet)

		listTaskTool := mcpgo.NewTool("task_list",
			mcpgo.WithDescription("List tasks matching filters. Workspace-scoped."),
			mcpgo.WithString("origin", mcpgo.Description("Filter by origin.")),
			mcpgo.WithString("status", mcpgo.Description("Filter by status.")),
			mcpgo.WithString("priority", mcpgo.Description("Filter by priority.")),
			mcpgo.WithString("claimed_by", mcpgo.Description("Filter by claimed_by worker id.")),
			mcpgo.WithNumber("limit", mcpgo.Description("Max rows to return.")),
		)
		s.mcp.AddTool(listTaskTool, s.handleTaskList)

		claimTaskTool := mcpgo.NewTool("task_claim",
			mcpgo.WithDescription("Atomic claim: picks highest-priority oldest-queued task from the worker's workspace(s). Returns null when queue is empty. Writes a kind:task-claimed ledger row."),
			mcpgo.WithString("worker_id", mcpgo.Description("Worker id. Defaults to the caller's user id.")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Narrow to one workspace. Defaults to all caller memberships.")),
		)
		s.mcp.AddTool(claimTaskTool, s.handleTaskClaim)

		closeTaskTool := mcpgo.NewTool("task_close",
			mcpgo.WithDescription("Close a task with outcome (success|failure|timeout). Writes a kind:task-closed ledger row. When origin=story_stage and outcome=success, enqueues the parent story's next ready CI as a follow-up task (stage hand-off). When worker_id is supplied and does not match the task's current ClaimedBy, the close is rejected with stale_claim (story_b4513c8c)."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id.")),
			mcpgo.WithString("outcome", mcpgo.Required(), mcpgo.Description("success | failure | timeout")),
			mcpgo.WithString("worker_id", mcpgo.Description("Optional worker id; when supplied, the handler rejects the close if the task has been reclaimed to a different worker since claim time.")),
		)
		s.mcp.AddTool(closeTaskTool, s.handleTaskClose)
	}

	if s.repos != nil {
		addRepoTool := mcpgo.NewTool("repo_add",
			mcpgo.WithDescription("Register a git remote on the caller's project. Dedups on (workspace, git_remote); enqueues a reindex task. Returns {repo_id, task_id, deduplicated}. Story_970ddfa1."),
			mcpgo.WithString("git_remote", mcpgo.Required(), mcpgo.Description("Git remote URL (e.g. git@github.com:owner/repo.git).")),
			mcpgo.WithString("default_branch", mcpgo.Description("Default branch (default: main).")),
			mcpgo.WithString("project_id", mcpgo.Description("Project scope. Defaults to caller's first owned project.")),
		)
		s.mcp.AddTool(addRepoTool, s.handleRepoAdd)

		getRepoTool := mcpgo.NewTool("repo_get",
			mcpgo.WithDescription("Return a repo by id. Workspace-scoped — cross-workspace returns not-found."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
		)
		s.mcp.AddTool(getRepoTool, s.handleRepoGet)

		listRepoTool := mcpgo.NewTool("repo_list",
			mcpgo.WithDescription("List repos in a project. Defaults to caller's workspaces and status=active. Pass status=archived for archived rows or status=all for both."),
			mcpgo.WithString("project_id", mcpgo.Description("Project scope. Defaults to caller's first owned project.")),
			mcpgo.WithString("status", mcpgo.Description("active (default) | archived | all")),
		)
		s.mcp.AddTool(listRepoTool, s.handleRepoList)

		scanRepoTool := mcpgo.NewTool("repo_scan",
			mcpgo.WithDescription("Enqueue a reindex task. Idempotent — returns the in-flight task_id when one already exists for the repo."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
		)
		s.mcp.AddTool(scanRepoTool, s.handleRepoScan)

		searchTool := mcpgo.NewTool("repo_search",
			mcpgo.WithDescription("Symbol search via jcodemunch. Writes a kind:repo-query audit row. Returns the jcodemunch payload as JSON. jcodemunch outage → structured `jcodemunch_unavailable` error."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Search query.")),
			mcpgo.WithString("kind", mcpgo.Description("Optional symbol kind filter.")),
			mcpgo.WithString("language", mcpgo.Description("Optional language filter.")),
		)
		s.mcp.AddTool(searchTool, s.handleRepoSearch)

		searchTextTool := mcpgo.NewTool("repo_search_text",
			mcpgo.WithDescription("Full-text search via jcodemunch. Writes a kind:repo-query audit row."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Search query.")),
			mcpgo.WithString("file_pattern", mcpgo.Description("Optional file glob.")),
		)
		s.mcp.AddTool(searchTextTool, s.handleRepoSearchText)

		symbolSourceTool := mcpgo.NewTool("repo_get_symbol_source",
			mcpgo.WithDescription("Source of one symbol via jcodemunch."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("symbol_id", mcpgo.Required(), mcpgo.Description("Jcodemunch symbol id.")),
		)
		s.mcp.AddTool(symbolSourceTool, s.handleRepoGetSymbolSource)

		fileTool := mcpgo.NewTool("repo_get_file",
			mcpgo.WithDescription("Raw file content via jcodemunch."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Repo-relative file path.")),
		)
		s.mcp.AddTool(fileTool, s.handleRepoGetFile)

		outlineTool := mcpgo.NewTool("repo_get_outline",
			mcpgo.WithDescription("File outline (symbols + nesting) via jcodemunch."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Repo-relative file path.")),
		)
		s.mcp.AddTool(outlineTool, s.handleRepoGetOutline)
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

// nowUTC returns the server's clock reading. Production calls fall
// through to time.Now().UTC(); tests inject Deps.NowFunc to freeze the
// clock at a fixture timestamp so session-staleness checks remain
// deterministic (story_3ae6621b).
func (s *Server) nowUTC() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now().UTC()
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
		"project_id": resolvedID,
		"name":       res.Document.Name,
		"version":    res.Document.Version,
		"changed":    res.Changed,
		"created":    res.Created,
	}
	body, _ := json.Marshal(payload)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_ingest_file").
		Str("project_id", resolvedID).
		Str("name", res.Document.Name).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id := req.GetString("id", "")
	if id != "" {
		doc, err := s.docs.GetByID(ctx, id, memberships)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		body, _ := json.Marshal(doc)
		s.logger.Info().
			Str("method", "tools/call").
			Str("tool", "document_get").
			Str("id", id).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Msg("mcp tool call")
		return mcpgo.NewToolResultText(string(body)), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError("either id or name is required"), nil
	}
	projectID := req.GetString("project_id", "")
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	doc, err := s.docs.GetByName(ctx, resolvedID, name, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(doc)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_get").
		Str("project_id", resolvedID).
		Str("name", name).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// immutableUpdateFields are the document keys that document_update must
// reject if the caller supplies them. The Store interface's UpdateFields
// only carries the mutable subset, so the only place to enforce this is
// the handler.
var immutableUpdateFields = []string{"workspace_id", "project_id", "type", "scope", "name", "id"}

func (s *Server) handleDocumentCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	docType, err := req.RequireString("type")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	scope, err := req.RequireString("scope")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	wsID := s.resolveCallerWorkspaceID(ctx, caller)
	requestedProject := req.GetString("project_id", "")

	doc := document.Document{
		WorkspaceID: wsID,
		Type:        docType,
		Scope:       scope,
		Name:        name,
		Body:        req.GetString("body", ""),
		Tags:        req.GetStringSlice("tags", nil),
		Status:      req.GetString("status", document.StatusActive),
		CreatedBy:   caller.UserID,
		UpdatedBy:   caller.UserID,
	}

	switch scope {
	case document.ScopeProject:
		resolvedID, err := s.resolveProjectID(ctx, requestedProject, caller, memberships)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		doc.ProjectID = document.StringPtr(resolvedID)
		if cascade := s.resolveProjectWorkspaceID(ctx, resolvedID); cascade != "" {
			doc.WorkspaceID = cascade
		}
	case document.ScopeSystem:
		if requestedProject != "" {
			return mcpgo.NewToolResultError("scope=system does not accept project_id"), nil
		}
	}
	if binding := req.GetString("contract_binding", ""); binding != "" {
		doc.ContractBinding = document.StringPtr(binding)
	}
	if structured := req.GetString("structured", ""); structured != "" {
		if !json.Valid([]byte(structured)) {
			return mcpgo.NewToolResultError("structured must be valid JSON"), nil
		}
		doc.Structured = []byte(structured)
	}

	created, err := s.docs.Create(ctx, doc, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(created)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_create").
		Str("doc_id", created.ID).
		Str("type", created.Type).
		Str("scope", created.Scope).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	args := req.GetArguments()
	for _, k := range immutableUpdateFields {
		if k == "id" {
			continue
		}
		if _, ok := args[k]; ok {
			return mcpgo.NewToolResultError("immutable field rejected: " + k), nil
		}
	}
	fields := document.UpdateFields{}
	if v, ok := args["body"]; ok {
		s, _ := v.(string)
		fields.Body = &s
	}
	if v, ok := args["structured"]; ok {
		s, _ := v.(string)
		if s != "" && !json.Valid([]byte(s)) {
			return mcpgo.NewToolResultError("structured must be valid JSON"), nil
		}
		buf := []byte(s)
		fields.Structured = &buf
	}
	if _, ok := args["tags"]; ok {
		tags := req.GetStringSlice("tags", nil)
		fields.Tags = &tags
	}
	if v, ok := args["status"]; ok {
		s, _ := v.(string)
		fields.Status = &s
	}
	if v, ok := args["contract_binding"]; ok {
		s, _ := v.(string)
		fields.ContractBinding = &s
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	updated, err := s.docs.Update(ctx, id, fields, caller.UserID, time.Now().UTC(), memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_update").
		Str("doc_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	opts := document.ListOptions{
		Type:            req.GetString("type", ""),
		Scope:           req.GetString("scope", ""),
		ContractBinding: req.GetString("contract_binding", ""),
		ProjectID:       req.GetString("project_id", ""),
		Tags:            req.GetStringSlice("tags", nil),
		Limit:           int(req.GetFloat("limit", 0)),
	}
	if opts.Limit > 500 {
		opts.Limit = 500
	}
	rows, err := s.docs.List(ctx, opts, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_list").
		Str("type", opts.Type).
		Str("scope", opts.Scope).
		Int("count", len(rows)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	opts := document.SearchOptions{
		ListOptions: document.ListOptions{
			Type:            req.GetString("type", ""),
			Scope:           req.GetString("scope", ""),
			ContractBinding: req.GetString("contract_binding", ""),
			ProjectID:       req.GetString("project_id", ""),
			Tags:            req.GetStringSlice("tags", nil),
		},
		Query: req.GetString("query", ""),
		TopK:  int(req.GetFloat("top_k", 0)),
	}
	rows, err := s.docs.Search(ctx, opts, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_search").
		Str("query", opts.Query).
		Str("type", opts.Type).
		Int("count", len(rows)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	mode := document.DeleteMode(req.GetString("mode", string(document.DeleteArchive)))
	memberships := s.resolveCallerMemberships(ctx, caller)
	if err := s.docs.Delete(ctx, id, mode, memberships); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{"id": id, "mode": string(mode), "deleted": true})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_delete").
		Str("doc_id", id).
		Str("mode", string(mode)).
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
	entryType, classifiedTags := classifyLedgerEvent(eventType)
	tags := append([]string{}, classifiedTags...)
	tags = append(tags, req.GetStringSlice("tags", nil)...)

	entry := ledger.LedgerEntry{
		WorkspaceID: wsID,
		ProjectID:   resolvedID,
		StoryID:     ledger.StringPtr(req.GetString("story_id", "")),
		ContractID:  ledger.StringPtr(req.GetString("contract_id", "")),
		Type:        entryType,
		Tags:        tags,
		Content:     content,
		Durability:  req.GetString("durability", ""),
		SourceType:  req.GetString("source_type", ""),
		Sensitive:   req.GetBool("sensitive", false),
		CreatedBy:   caller.UserID,
	}
	if structured := req.GetString("structured", ""); structured != "" {
		if !json.Valid([]byte(structured)) {
			return mcpgo.NewToolResultError("structured must be valid JSON"), nil
		}
		entry.Structured = []byte(structured)
	}
	if expires := req.GetString("expires_at", ""); expires != "" {
		t, err := time.Parse(time.RFC3339, expires)
		if err != nil {
			return mcpgo.NewToolResultError("expires_at must be RFC3339"), nil
		}
		entry.ExpiresAt = &t
	}

	e, err := s.ledger.Append(ctx, entry, time.Now().UTC())
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

func (s *Server) handleLedgerGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	e, err := s.ledger.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(e)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_get").Str("id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
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
	opts := ledger.SearchOptions{
		ListOptions: buildLedgerListOptions(req),
		Query:       req.GetString("query", ""),
		TopK:        int(req.GetFloat("top_k", 0)),
	}
	rows, err := s.ledger.Search(ctx, resolvedID, opts, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_search").Str("project_id", resolvedID).Str("query", opts.Query).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerRecall(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	rootID, err := req.RequireString("root_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	rows, err := s.ledger.Recall(ctx, rootID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_recall").Str("root_id", rootID).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerDereference(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	reason, err := req.RequireString("reason")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	audit, err := s.ledger.Dereference(ctx, id, reason, caller.UserID, time.Now().UTC(), memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(audit)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_dereference").Str("id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// buildLedgerListOptions translates a CallToolRequest into ListOptions.
// Shared by handleLedgerList and handleLedgerSearch so the filter
// surface is identical.
func buildLedgerListOptions(req mcpgo.CallToolRequest) ledger.ListOptions {
	opts := ledger.ListOptions{
		Type:          req.GetString("type", ""),
		StoryID:       req.GetString("story_id", ""),
		ContractID:    req.GetString("contract_id", ""),
		Tags:          req.GetStringSlice("tags", nil),
		Durability:    req.GetString("durability", ""),
		SourceType:    req.GetString("source_type", ""),
		Status:        req.GetString("status", ""),
		IncludeDerefd: req.GetBool("include_dereferenced", false),
		Limit:         int(req.GetFloat("limit", 0)),
	}
	args := req.GetArguments()
	if v, ok := args["sensitive"]; ok {
		if b, ok := v.(bool); ok {
			opts.Sensitive = &b
		}
	}
	return opts
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

// requireWorkspaceAdmin asserts the caller is an admin of the given
// workspace. Returns a user-friendly error on mismatch.
func (s *Server) requireWorkspaceAdmin(ctx context.Context, caller CallerIdentity, workspaceID string) error {
	if caller.UserID == "" {
		return errors.New("no caller identity")
	}
	role, err := s.workspaces.GetRole(ctx, workspaceID, caller.UserID)
	if err != nil {
		return errors.New("workspace not found")
	}
	if role != workspace.RoleAdmin {
		return errors.New("admin role required")
	}
	return nil
}

// adminCount returns the number of admin members on a workspace. Used for
// the last-admin guard on downgrades and removals.
func (s *Server) adminCount(ctx context.Context, workspaceID string) (int, error) {
	members, err := s.workspaces.ListMembers(ctx, workspaceID)
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

// appendMembershipAudit writes a ledger row recording a membership
// mutation. Scoped to the system default project + the target workspace.
// Safe to no-op when defaults aren't wired (tests).
func (s *Server) appendMembershipAudit(ctx context.Context, workspaceID, kind, actor string, payload map[string]any) {
	if s.ledger == nil || s.defaultProjectID == "" {
		return
	}
	payload["workspace_id"] = workspaceID
	payload["kind"] = kind
	body, _ := json.Marshal(payload)
	_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   s.defaultProjectID,
		Type:        ledger.TypeDecision,
		Tags:        []string{"kind:workspace." + kind},
		Content:     string(body),
		CreatedBy:   actor,
	}, time.Now().UTC())
}

// classifyLedgerEvent maps a caller-supplied event-type string into the
// §6 enum. When the caller's value is one of the lifecycle types
// (plan/action_claim/etc.) it passes through. Otherwise the event is
// recorded as a generic decision with the original event-type preserved
// as a `kind:<value>` tag — keeping the §6 enum closed without
// forcing scripts that emitted v3-style domain events to be rewritten.
func classifyLedgerEvent(eventType string) (string, []string) {
	switch eventType {
	case ledger.TypePlan, ledger.TypeActionClaim, ledger.TypeArtifact,
		ledger.TypeEvidence, ledger.TypeDecision, ledger.TypeCloseRequest,
		ledger.TypeVerdict, ledger.TypeWorkflowClaim, ledger.TypeKV:
		return eventType, nil
	default:
		return ledger.TypeDecision, []string{"kind:" + eventType}
	}
}

func (s *Server) handleWorkspaceMemberAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	workspaceID, err := req.RequireString("workspace_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	userID, err := req.RequireString("user_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.requireWorkspaceAdmin(ctx, caller, workspaceID); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.workspaces.AddMember(ctx, workspaceID, userID, role, caller.UserID, time.Now().UTC()); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.appendMembershipAudit(ctx, workspaceID, "member_add", caller.UserID, map[string]any{
		"target_user_id": userID,
		"role":           role,
	})
	body, _ := json.Marshal(map[string]any{
		"workspace_id": workspaceID,
		"user_id":      userID,
		"role":         role,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_member_add").
		Str("workspace_id", workspaceID).
		Str("user_id", userID).
		Str("role", role).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceMemberList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	workspaceID, err := req.RequireString("workspace_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	is, err := s.workspaces.IsMember(ctx, workspaceID, caller.UserID)
	if err != nil || !is {
		return mcpgo.NewToolResultError("workspace not found"), nil
	}
	members, err := s.workspaces.ListMembers(ctx, workspaceID)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(members)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_member_list").
		Str("workspace_id", workspaceID).
		Int("count", len(members)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceMemberUpdateRole(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	workspaceID, err := req.RequireString("workspace_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	userID, err := req.RequireString("user_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	newRole, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.requireWorkspaceAdmin(ctx, caller, workspaceID); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	currentRole, err := s.workspaces.GetRole(ctx, workspaceID, userID)
	if err != nil {
		return mcpgo.NewToolResultError("member not found"), nil
	}
	if currentRole == workspace.RoleAdmin && newRole != workspace.RoleAdmin {
		count, err := s.adminCount(ctx, workspaceID)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if count <= 1 {
			return mcpgo.NewToolResultError("cannot downgrade the last admin"), nil
		}
	}
	if err := s.workspaces.UpdateRole(ctx, workspaceID, userID, newRole, time.Now().UTC()); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.appendMembershipAudit(ctx, workspaceID, "member_update_role", caller.UserID, map[string]any{
		"target_user_id": userID,
		"previous_role":  currentRole,
		"new_role":       newRole,
	})
	body, _ := json.Marshal(map[string]any{
		"workspace_id": workspaceID,
		"user_id":      userID,
		"role":         newRole,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_member_update_role").
		Str("workspace_id", workspaceID).
		Str("user_id", userID).
		Str("role", newRole).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceMemberRemove(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	workspaceID, err := req.RequireString("workspace_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	userID, err := req.RequireString("user_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.requireWorkspaceAdmin(ctx, caller, workspaceID); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	currentRole, err := s.workspaces.GetRole(ctx, workspaceID, userID)
	if err != nil {
		return mcpgo.NewToolResultError("member not found"), nil
	}
	if currentRole == workspace.RoleAdmin {
		count, err := s.adminCount(ctx, workspaceID)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if count <= 1 {
			return mcpgo.NewToolResultError("cannot remove the last admin"), nil
		}
	}
	if err := s.workspaces.RemoveMember(ctx, workspaceID, userID); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.appendMembershipAudit(ctx, workspaceID, "member_remove", caller.UserID, map[string]any{
		"target_user_id": userID,
		"previous_role":  currentRole,
	})
	body, _ := json.Marshal(map[string]any{
		"workspace_id": workspaceID,
		"user_id":      userID,
		"removed":      true,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "workspace_member_remove").
		Str("workspace_id", workspaceID).
		Str("user_id", userID).
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
	opts := buildLedgerListOptions(req)
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

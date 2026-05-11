// Package mcpserver exposes the satellites MCP surface over Streamable HTTP.
// v4 currently registers: satellites_info, document_ingest_file, document_get,
// project_create/get/list, ledger_append/list, story_create/get/list/update_status,
// workspace_create/get/list. Subsequent epics add more.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/portalreplicate"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
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
	sessions         session.Store
	tasks            task.Store
	repos            repo.Store
	changelog        changelog.Store
	apiKeys          auth.APIKeyStore
	indexer          codeindex.Indexer
	replicateVocab   *portalreplicate.Vocabulary
	replicateRunner  func(ctx context.Context, opts portalreplicate.RunOptions, actions []portalreplicate.Action) ([]portalreplicate.Result, portalreplicate.Summary, error)
	nowFunc          func() time.Time
	audit            *auditLogger
}

// HandshakeFallbackInstructions is the literal MCP server-instructions
// string used when the agent-process artifact resolver returns empty —
// i.e. the seed hasn't run, the doc store isn't wired, or no project
// context narrows the lookup. Kept verbatim from pre-sty_e1ab884d so
// out-of-band MCP clients that grep for it during integration testing
// continue to match.
const HandshakeFallbackInstructions = "Satellites v4 — walking skeleton."

// cli returns the typed business surface used by handlers extracted
// under cli-primary order:02. Rebuilds Deps on every call so test
// fixtures that mutate Server store fields *after* New() (the
// orchestratorFixture pattern that sets f.server.tasks late) reach
// typed methods with a fresh snapshot. story_66c02002 + sty_df1cb227.
func (s *Server) cli() *client.Client {
	return client.New(client.Deps{
		Documents:  s.docs,
		Projects:   s.projects,
		Ledger:     s.ledger,
		Stories:    s.stories,
		Workspaces: s.workspaces,
		Sessions:   s.sessions,
		Tasks:      s.tasks,
		Repos:      s.repos,
		Changelog:  s.changelog,
		StartedAt:  s.startedAt,
	})
}

// resolveHandshakeInstructions returns the MCP server-instructions
// string emitted at handshake. Sourced from the agent-process
// resolver chain (sty_e1ab884d): project-scope override (not yet
// wired here — needs URL-scoped project context), then system-scope
// `default_agent_process`, then HandshakeFallbackInstructions.
//
// docs may be nil during early-boot tests; the helper returns the
// fallback in that case so the server stays bootable.
func resolveHandshakeInstructions(docs document.Store) string {
	if body := agentprocess.Resolve(context.Background(), docs, "", nil); body != "" {
		return body
	}
	return HandshakeFallbackInstructions
}

// Deps bundles the optional per-tool dependencies passed through to
// handlers. A nil store field disables the associated verbs.
type Deps struct {
	// AuditReadTTL is the durability TTL applied to read-classified
	// audit rows. Zero falls back to 720h (30 days). Mutations land
	// durable and ignore this knob. Sty_1493c077.
	AuditReadTTL     time.Duration
	DocStore         document.Store
	DocsDir          string
	ProjectStore     project.Store
	DefaultProjectID string
	LedgerStore      ledger.Store
	StoryStore       story.Store
	WorkspaceStore   workspace.Store
	SessionStore     session.Store
	// TaskStore is optional; nil disables the task_* MCP verbs.
	// Story_a8fee0cc.
	TaskStore task.Store
	// RepoStore is optional; nil disables the repo_* MCP verbs.
	// Story_970ddfa1.
	RepoStore repo.Store
	// ChangelogStore is optional; nil disables the changelog_* MCP verbs
	// and the project-page changelog panel renders empty (sty_12af0bdc).
	ChangelogStore changelog.Store
	// APIKeyStore is optional; nil disables the agent_apikey_* MCP
	// verbs and the AuthMiddleware store-backed Bearer fall-through.
	// story_3191fbfc.
	APIKeyStore auth.APIKeyStore
	// Indexer is the satellites-native code indexer used by repo_*
	// search/get verbs and the reindex worker. Nil falls back to
	// codeindex.NewStub() which returns a structured
	// "code_index_unavailable" error for every call — useful for unit
	// tests. Production wires codeindex.NewLocalIndexer(workdir).
	// Story_75a371c7 replaced the prior jcodemunch proxy with this
	// satellites-internal package.
	Indexer codeindex.Indexer
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
		sessions:         deps.SessionStore,
		tasks:            deps.TaskStore,
		repos:            deps.RepoStore,
		changelog:        deps.ChangelogStore,
		apiKeys:          deps.APIKeyStore,
		indexer:          deps.Indexer,
		nowFunc:          deps.NowFunc,
	}
	if s.indexer == nil {
		s.indexer = codeindex.NewStub()
	}
	// sty_66c02002 + sty_df1cb227: typed business surface. cli() builds
	// the per-call client.Deps snapshot — no eager init here, so test
	// fixtures that wire stores after New() (the orchestratorFixture
	// pattern that sets f.server.tasks late) see them at typed-method
	// dispatch.

	// sty_1493c077: per-call audit logger. Wraps every tool handler via
	// mcp-go's middleware seam; writes one ledger row per call tagged
	// kind:mcp-call. Reads land ephemeral, mutations durable. Disabled
	// when no ledger store is wired (early-test fixtures).
	if s.ledger != nil {
		s.audit = newAuditLogger(s.ledger, s.tasks, s.projects, s.logger,
			deps.AuditReadTTL, s.defaultProjectID, s.nowFunc)
	}

	// sty_e1ab884d: handshake instructions are sourced from the
	// agent-process artifact. Resolution chain: project-scope override
	// (when this server boots into a project context — not yet wired) →
	// system-scope `default_agent_process` artifact (seeded at boot via
	// agentprocess.SeedSystemDefault). The literal "walking skeleton"
	// tagline is the back-compat fallback for boots where the seed
	// hasn't run (early-test fixtures) or the doc store is unwired.
	serverOpts := []mcpserver.ServerOption{
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions(resolveHandshakeInstructions(s.docs)),
	}
	if s.audit != nil {
		serverOpts = append(serverOpts, mcpserver.WithToolHandlerMiddleware(s.audit.middleware))
	}
	s.mcp = mcpserver.NewMCPServer(
		"satellites",
		config.Version,
		serverOpts...,
	)

	infoTool := mcpgo.NewTool("satellites_info",
		mcpgo.WithDescription("Return the satellites server's version metadata and the calling user's identity."),
	)
	s.mcp.AddTool(infoTool, s.handleInfo)

	// Per cli-primary order:07c (sty_0881d04b): the two surviving
	// advisory verbs alongside satellites_info. order:07d will
	// delete every other tool; these three remain.
	helpTool := mcpgo.NewTool("satellites_help",
		mcpgo.WithDescription("Return the bin/satellites-client CLI catalogue (noun groups + verbs + flags + persistent-flag list + exit-code map) as structured JSON. IDE agents call this once at session boot to discover the surface without paying the per-MCP-tool token cost."),
	)
	s.mcp.AddTool(helpTool, s.handleSatellitesHelp)

	execTool := mcpgo.NewTool("satellites_exec",
		mcpgo.WithDescription("Proxy a bin/satellites-client invocation. Spawns the CLI server-side with the supplied argv + optional stdin; returns stdout, stderr, exit_code. Caller's bearer is forwarded via SATELLITES_TOKEN env. Output is bounded by SATELLITES_EXEC_PAYLOAD_CAP (default 1 MiB); wall clock by SATELLITES_EXEC_TIMEOUT (default 30s)."),
		mcpgo.WithArray("argv", mcpgo.Required(),
			mcpgo.Description("CLI argument vector, e.g. [\"task\", \"get\", \"task_xxx\"]."),
			mcpgo.Items(map[string]any{"type": "string"})),
		mcpgo.WithString("stdin",
			mcpgo.Description("Optional stdin body forwarded to the subprocess.")),
	)
	s.mcp.AddTool(execTool, s.handleSatellitesExec)

	if s.docs != nil {
		ingestTool := mcpgo.NewTool("document_ingest_file",
			mcpgo.WithDescription("Ingest a file from the server's docs volume (SATELLITES_DOCS_DIR) into the document store. Path is repo-relative; server reads the file and upserts by (project_id, name). If project_id is omitted, defaults to the caller's first owned project or the system default."),
			mcpgo.WithString("path",
				mcpgo.Required(),
				mcpgo.Description("Repo-relative path inside SATELLITES_DOCS_DIR."),
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
			mcpgo.WithDescription("Create a new document. Workspace is resolved from the caller; project_id is required when scope=project and forbidden when scope=system. type=configuration (story_d371f155) accepts scope=project or scope=system (story_764726d3 — configseed ships a system-default Configuration operators can clone) and requires a structured payload of shape {\"contract_refs\":[...],\"skill_refs\":[...],\"principle_refs\":[...]} whose ids must resolve to active documents of the matching type in the same workspace."),
			mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("artifact | contract | skill | principle | reviewer | agent | role | configuration")),
			mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("system | project | workspace (workspace only valid for type=role)")),
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
			mcpgo.WithDescription("Create a new project owned by the caller. Code-backed projects: follow with repo_add to register the git remote on the resulting project. The (workspace, git_remote) binding lives on the per-project repo row, not on the project itself."),
			mcpgo.WithString("name",
				mcpgo.Required(),
				mcpgo.Description("Project display name."),
			),
		)
		s.mcp.AddTool(createTool, s.handleProjectCreate)

		getProjTool := mcpgo.NewTool("project_get",
			mcpgo.WithDescription("Return the orientation bundle for a project the caller owns: project row, mcp_url + mcp_config (paste-ready client snippets that scope an MCP client to this project via ?project_id=), intent_body, and active principles. Cross-owner access returns not-found."),
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

		updateProjTool := mcpgo.NewTool("project_update",
			mcpgo.WithDescription("Update a project's name and/or mcp_url. Owner-only. The git remote binding is managed by repo_add — call that to add or replace the project's tracked repo."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Project id (proj_<8hex>).")),
			mcpgo.WithString("name", mcpgo.Description("New display name. Empty to leave unchanged.")),
			mcpgo.WithString("mcp_url", mcpgo.Description("Explicit MCP connection URL. Empty string clears the override and falls back to the derived form. Absent leaves unchanged.")),
		)
		s.mcp.AddTool(updateProjTool, s.handleProjectUpdate)

		deleteProjTool := mcpgo.NewTool("project_delete",
			mcpgo.WithDescription("Archive a project (soft delete — flips status to archived, rows are not physically removed). Owner-only."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Project id (proj_<8hex>).")),
		)
		s.mcp.AddTool(deleteProjTool, s.handleProjectDelete)

		// project_set — sty_4db7c3a3 + sty_31d51494 layer 2. The agent's
		// first call after a user prompt fires when the prompt references
		// substrate primitives (story, task, contract, agent) or asks
		// about the project. Resolves the existing project by canonical
		// git_remote, auto-registers the session row keyed by the
		// Mcp-Session-Id header (no body arg required), stamps
		// active_project_id, and returns the orientation bundle —
		// project + intent_body + principles[] — in one roundtrip.
		// Idempotent. Never creates a project.
		setProjTool := mcpgo.NewTool("project_set",
			mcpgo.WithDescription("Bootstrap call after a user prompt fires when the prompt references substrate primitives (story id, task id, contract name, agent name) or asks about the project. Binds the caller's session to the project that owns the given git remote URL. Auto-registers the session row keyed by the Mcp-Session-Id header — no explicit session_register required. Returns the orientation bundle: {project_id, status: \"resolved\", mcp_url, intent_body, principles[]}. When no project matches, returns {status: \"no_project_for_remote\", repo_url_canonical}. Subsequent project-scoped verbs may default to the bound project. Idempotent."),
			mcpgo.WithString("repo_url", mcpgo.Required(), mcpgo.Description("Git remote URL — accepts ssh, https, or git:// forms. Normalised server-side via the same canonicaliser project_create uses. Typically `git remote get-url origin` from the working directory.")),
			mcpgo.WithString("session_id", mcpgo.Description("Optional explicit session id. Streamable HTTP callers should let the Mcp-Session-Id header carry the id; this arg is for stdio/test callers that can't set the header.")),
		)
		s.mcp.AddTool(setProjTool, s.handleProjectSet)
	}

	if s.ledger != nil {
		appendTool := mcpgo.NewTool("ledger_append",
			mcpgo.WithDescription("Append an event row to the project's ledger. Caller must own the project."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project scope.")),
			mcpgo.WithString("type", mcpgo.Required(), mcpgo.Description("Event type per architecture.md §6 enum (plan|action_claim|artifact|evidence|decision|close-request|verdict|workflow-claim|kv); other strings are wrapped as Type=decision with the original value preserved as a kind:<value> tag.")),
			mcpgo.WithString("content", mcpgo.Description("Event content / free-form markdown.")),
			mcpgo.WithString("story_id", mcpgo.Description("Optional story FK.")),
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

		updateStoryTool := mcpgo.NewTool("story_update",
			mcpgo.WithDescription("Update a story's mutable non-status fields. Pass only the fields you want to change; omitted fields are left untouched. Tags replace wholesale (V3 parity) — pass an empty array to clear. Status transitions go through story_update_status."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id (sty_<8hex>).")),
			mcpgo.WithString("title", mcpgo.Description("New title.")),
			mcpgo.WithString("description", mcpgo.Description("New description.")),
			mcpgo.WithString("acceptance_criteria", mcpgo.Description("New acceptance criteria.")),
			mcpgo.WithString("category", mcpgo.Description("feature | bug | improvement | infrastructure | documentation")),
			mcpgo.WithString("priority", mcpgo.Description("critical | high | medium | low")),
			mcpgo.WithArray("tags", mcpgo.Description("Tags/labels (replaces existing tags). Empty array clears."),
				mcpgo.Items(map[string]any{"type": "string"})),
		)
		s.mcp.AddTool(updateStoryTool, s.handleStoryUpdate)

		getStoryTool := mcpgo.NewTool("story_get",
			mcpgo.WithDescription("Return the orientation bundle for a story: row body/status/fields/tags, owning project, recent ledger evidence, the resolved agent_process instruction markdown, and the category template. Workspace-scoped; cross-project access returns not-found."),
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
			mcpgo.WithDescription("Transition a story to a new status. Emits a story.status_change ledger row. Valid transitions: backlog→ready→in_progress→done, or ←→cancelled from any non-terminal. The story's category template (if registered) gates the transition — failed structured hooks are returned as a natural-language explanation."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id.")),
			mcpgo.WithString("status", mcpgo.Required(), mcpgo.Description("Target status: ready | in_progress | done | cancelled.")),
		)
		s.mcp.AddTool(updateStatusTool, s.handleStoryUpdateStatus)

		fieldSetTool := mcpgo.NewTool("story_field_set",
			mcpgo.WithDescription("Set a single template-defined field on a story (e.g. repro, fix_commit, root_cause). The field must be declared by the story's category template; unknown fields are rejected. Pass an empty value to clear a field."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id (sty_<8hex>).")),
			mcpgo.WithString("field", mcpgo.Required(), mcpgo.Description("Field name as declared by the category template.")),
			mcpgo.WithString("value", mcpgo.Description("Field value. Empty string clears the field.")),
		)
		s.mcp.AddTool(fieldSetTool, s.handleStoryFieldSet)

		templateGetTool := mcpgo.NewTool("story_template_get",
			mcpgo.WithDescription("Return the parsed story template for a given category. Convenience over document_get with type=story_template."),
			mcpgo.WithString("category", mcpgo.Required(), mcpgo.Description("Category name: bug | feature | improvement | infrastructure | documentation.")),
		)
		s.mcp.AddTool(templateGetTool, s.handleStoryTemplateGet)

		templateListTool := mcpgo.NewTool("story_template_list",
			mcpgo.WithDescription("List every registered story template. Convenience over document_list with type=story_template."),
		)
		s.mcp.AddTool(templateListTool, s.handleStoryTemplateList)
	}

	if s.stories != nil && s.ledger != nil && s.docs != nil && s.projects != nil {
		// project_workflow_spec_get / _set were removed by
		// epic:configuration-over-code-mandate (story_af79cf95). The
		// substrate no longer enforces a per-project workflow shape; the
		// orchestrator composes per-story plans and the reviewer
		// (story_reviewer, Gemini-backed) approves them via the
		// plan-approval loop (now agent-authored via task_add).

		// Unified KV verbs (story_3d392258). Single family taking a
		// `scope` arg covering the four tiers from epic:kv-scopes.
		// Per-scope role gates land in story_eb17cb16.
		kvGetTool := mcpgo.NewTool("kv_get",
			mcpgo.WithDescription("Read a KV value at the named scope. scope=system|workspace|project|user. Returns {key, value, scope, updated_at, updated_by, entry_id} or not_found. Scope-strict: does not walk the resolution chain (see kv_get_resolved in story_405b7221)."),
			mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("KV scope: system|workspace|project|user.")),
			mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("KV key.")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Required for scope=workspace and scope=user.")),
			mcpgo.WithString("project_id", mcpgo.Description("Required for scope=project.")),
			mcpgo.WithString("user_id", mcpgo.Description("scope=user only. Defaults to the authenticated caller.")),
		)
		s.mcp.AddTool(kvGetTool, s.handleKVGet)

		kvSetTool := mcpgo.NewTool("kv_set",
			mcpgo.WithDescription("Write a KV value at the named scope. Appends a Type=kv ledger row tagged scope:<scope> + key:<name> (+ user:<id> for scope=user). scope=system requires global_admin; finer per-scope role gates land in story_eb17cb16."),
			mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("KV scope: system|workspace|project|user.")),
			mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("KV key.")),
			mcpgo.WithString("value", mcpgo.Required(), mcpgo.Description("KV value (string).")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Required for scope=workspace and scope=user.")),
			mcpgo.WithString("project_id", mcpgo.Description("Required for scope=project.")),
			mcpgo.WithString("user_id", mcpgo.Description("scope=user only. Defaults to the authenticated caller.")),
		)
		s.mcp.AddTool(kvSetTool, s.handleKVSet)

		kvDeleteTool := mcpgo.NewTool("kv_delete",
			mcpgo.WithDescription("Delete a KV value at the named scope. Appends a tombstone row (kind:tombstone tag + empty Content) — the projection then suppresses the key. Append-only ledger; the prior values stay in the audit chain. scope=system requires global_admin."),
			mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("KV scope: system|workspace|project|user.")),
			mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("KV key.")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Required for scope=workspace and scope=user.")),
			mcpgo.WithString("project_id", mcpgo.Description("Required for scope=project.")),
			mcpgo.WithString("user_id", mcpgo.Description("scope=user only. Defaults to the authenticated caller.")),
		)
		s.mcp.AddTool(kvDeleteTool, s.handleKVDelete)

		kvGetResolvedTool := mcpgo.NewTool("kv_get_resolved",
			mcpgo.WithDescription("Resolve a KV key by walking system → user → project → workspace and returning the first hit. system always wins; otherwise lowest-tier wins (user > project > workspace). Missing identifiers skip the corresponding tier — system-only callers may omit all FKs. Returns {key, value, resolved_scope, ...} on hit or not_found. Read path; no auth gate beyond workspace membership. story_405b7221."),
			mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("KV key to resolve.")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Optional. Required to read workspace, project, or user tiers.")),
			mcpgo.WithString("project_id", mcpgo.Description("Optional. Required to read the project tier.")),
			mcpgo.WithString("user_id", mcpgo.Description("Optional. Defaults to the authenticated caller. Required (or defaulted) to read the user tier.")),
		)
		s.mcp.AddTool(kvGetResolvedTool, s.handleKVGetResolved)

		kvListTool := mcpgo.NewTool("kv_list",
			mcpgo.WithDescription("List all KV values at the named scope. Returns {scope, count, items[]} sorted by key. Tombstoned keys are excluded."),
			mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("KV scope: system|workspace|project|user.")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Required for scope=workspace and scope=user.")),
			mcpgo.WithString("project_id", mcpgo.Description("Required for scope=project.")),
			mcpgo.WithString("user_id", mcpgo.Description("scope=user only. Defaults to the authenticated caller.")),
		)
		s.mcp.AddTool(kvListTool, s.handleKVList)

		agentComposeTool := mcpgo.NewTool("agent_compose",
			mcpgo.WithDescription("Create a type=agent document carrying explicit skill_refs + permission_patterns. When ephemeral=true the agent is scoped to story_id and the project_status sweeper archives it after SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS once the story reaches a terminal state. Writes a kind:agent-compose ledger row capturing {agent_id, name, skill_refs, permission_patterns, story_id, ephemeral, reason} in Structured. story_b19260d8."),
			mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Agent document name. Must be unique within scope.")),
			mcpgo.WithString("project_id", mcpgo.Description("Project for the agent. Defaults to the owning story's project when story_id is supplied; otherwise scope=system.")),
			mcpgo.WithArray("skill_refs", mcpgo.Description("Document ids of active type=skill rows the agent pulls."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithArray("permission_patterns", mcpgo.Description("Action_claim patterns this agent grants when allocated to a CI (e.g. Edit:internal/portal/**)."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithBoolean("ephemeral", mcpgo.Description("When true, the agent is story-scoped and the sweeper archives it on story completion. Requires story_id.")),
			mcpgo.WithString("story_id", mcpgo.Description("Owning story id. Required when ephemeral=true.")),
			mcpgo.WithString("reason", mcpgo.Description("Orchestrator's rationale; recorded on the kind:agent-compose ledger row + agent body.")),
		)
		s.mcp.AddTool(agentComposeTool, s.handleAgentCompose)

		agentSummaryTool := mcpgo.NewTool("agent_ephemeral_summary",
			mcpgo.WithDescription("Per-project hint surface (story_b19260d8 AC #7) — returns the count of active ephemeral type=agent documents and groups them by their sorted skill_refs so operators can spot promotion candidates: 'N agents created with skills X+Y → promote to canonical?'. Optional project_id; omit for an all-projects summary."),
			mcpgo.WithString("project_id", mcpgo.Description("Project to scope the summary to. Omit for all visible projects.")),
		)
		s.mcp.AddTool(agentSummaryTool, s.handleAgentEphemeralSummary)

		// sty_3191fbfc: agent api-key mint/list/delete. Disabled when
		// no APIKeyStore is wired (early-test fixtures); production
		// wires SurrealAgentAPIKeyStore in cmd/satellites/main.go.
		if s.apiKeys != nil {
			apiKeyCreateTool := mcpgo.NewTool("agent_apikey_create",
				mcpgo.WithDescription("Mint a new agent api-key. Returns the cleartext `key` once — subsequent agent_apikey_list calls return only metadata. The cleartext is hashed (SHA-256 with a per-row salt) at rest. The caller becomes the owner; AuthMiddleware Bearer requests carrying the cleartext resolve as the owner identity. story_3191fbfc."),
				mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Operator-friendly label, e.g. 'agent-laptop' or 'sty_ccb35588-dogfood'.")),
				mcpgo.WithString("project_id", mcpgo.Description("Optional project scope. When set, the caller must be a member of the project's workspace. Future enforcement may scope the resulting Bearer to project-level operations only.")),
				mcpgo.WithString("expires_at", mcpgo.Description("Optional RFC3339 expiry. When set, AuthMiddleware rejects the key after this instant. Omit for keys that never expire.")),
			)
			s.mcp.AddTool(apiKeyCreateTool, s.handleAgentAPIKeyCreate)

			apiKeyListTool := mcpgo.NewTool("agent_apikey_list",
				mcpgo.WithDescription("List the caller's agent api-keys. Global admins see every key. The cleartext key is NEVER returned; only id, prefix, name, owner, project, status, last_used_at, expires_at, and created_at. story_3191fbfc."),
				mcpgo.WithString("project_id", mcpgo.Description("Optional project filter. Empty = all projects the caller owns keys for.")),
				mcpgo.WithBoolean("include_archived", mcpgo.Description("Include status=archived rows. Default false — only active keys are returned.")),
			)
			s.mcp.AddTool(apiKeyListTool, s.handleAgentAPIKeyList)

			apiKeyDeleteTool := mcpgo.NewTool("agent_apikey_delete",
				mcpgo.WithDescription("Soft-delete an agent api-key by flipping its status to archived. The row remains queryable by id so audit ledger rows referencing apikey:<id> still resolve. Cross-owner deletes are rejected unless the caller is a global admin. Writes a kind:agent-apikey-archived ledger row. story_3191fbfc."),
				mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("api-key id (apk_<8hex>) to archive.")),
			)
			s.mcp.AddTool(apiKeyDeleteTool, s.handleAgentAPIKeyDelete)
		}

		if s.sessions != nil {
			// task_add (sty_a427368d): mint one task at status=published
			// for the given agent. Auto-mints a thin ad-hoc story when
			// story_id is omitted. Mints exactly one task — review
			// pairing, when a contract requires it, is authored by the
			// reviewer's contract prose via task_add(prior_task_id=…).
			taskAddTool := mcpgo.NewTool("task_add",
				mcpgo.WithDescription("Mint one task at status=published for the given agent. When story_id is omitted, auto-mints a thin ad-hoc story so every task is anchored to a story. Mints exactly one task — review pairing, when a contract requires it, is authored by the reviewer's contract prose via task_add(prior_task_id=…). Capability check: when action is shaped contract:<name>, the agent's delivers (kind=work) or reviews (kind=review) list must contain it. Returns {task_id, story_id, story_minted, status, agent_id}."),
				mcpgo.WithString("agent_id", mcpgo.Required(), mcpgo.Description("Document id of the agent that should execute this task.")),
				mcpgo.WithString("prompt", mcpgo.Required(), mcpgo.Description("The task body. Becomes the task's description. The dispatched agent reads this as its primary instruction.")),
				mcpgo.WithString("story_id", mcpgo.Description("Optional owning story id. When omitted, the substrate auto-mints a thin ad-hoc story (status=backlog, single AC: 'see task body').")),
				mcpgo.WithString("kind", mcpgo.Description("work (default) | review.")),
				mcpgo.WithString("action", mcpgo.Description("Optional action string. When shaped contract:<name>, capability is validated against the agent doc. Free-form actions are accepted on the agent doc's authority.")),
				mcpgo.WithString("priority", mcpgo.Description("critical | high | medium (default) | low.")),
			)
			s.mcp.AddTool(taskAddTool, s.handleTaskAdd)

			// task_update (sty_a427368d): mutate a task's lifecycle.
			// Today: status=closed (the agent's close path). Future
			// updates (priority change, agent reassignment) join here.
			taskUpdateTool := mcpgo.NewTool("task_update",
				mcpgo.WithDescription("Mutate a task's lifecycle state. Today the only supported transition is status=closed: closes the target task with outcome=success|failure and optionally tags evidence ledger rows. Closure mutates exactly the target row; any successor task (review, retry) is authored by the reviewer's contract prose via task_add. Validators reject task_not_found, task_already_terminal, invalid_outcome."),
				mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id to update.")),
				mcpgo.WithString("status", mcpgo.Required(), mcpgo.Description("Target status. Today: closed.")),
				mcpgo.WithString("outcome", mcpgo.Description("success (default) | failure. Used when status=closed.")),
				mcpgo.WithString("evidence_ledger_ids", mcpgo.Description("JSON array of ledger row ids referenced as evidence. The agent writes those rows separately (ledger_append) and references them here.")),
			)
			s.mcp.AddTool(taskUpdateTool, s.handleTaskUpdate)

			whoamiTool := mcpgo.NewTool("session_whoami",
				mcpgo.WithDescription("Return the caller's session registry row. session_id resolves from the Mcp-Session-Id header by default (story_31975268); pass session_id as a body arg to override. Returns a structured session_not_registered error when the resolved session is not in the registry."),
				mcpgo.WithString("session_id", mcpgo.Description("Optional session id override. Streamable HTTP callers should let the Mcp-Session-Id header carry the id.")),
			)
			s.mcp.AddTool(whoamiTool, s.handleSessionWhoami)

			registerTool := mcpgo.NewTool("session_register",
				mcpgo.WithDescription("Upsert a session row. story_31975268: session_id is server-minted when neither the body arg nor the Mcp-Session-Id header carries one — Streamable HTTP clients receive the minted id via the initialize response header and echo it on subsequent calls; stdio/test callers may pass session_id as a body arg. story_cef068fe: when project_id is supplied AND no explicit session_id was carried, the handler resumes the caller's most recent non-stale session for that (user, project), returning the same id (resumed=true). Stale sessions are skipped; a fresh id is minted instead."),
				mcpgo.WithString("session_id", mcpgo.Description("Optional session id. When omitted, sourced from the Mcp-Session-Id header; if neither carries one (and no resume hits), the server mints a UUIDv4.")),
				mcpgo.WithString("source", mcpgo.Description("Source string (session_start | enforce_hook | apikey). Defaults to session_start.")),
				mcpgo.WithString("workspace_id", mcpgo.Description("Optional workspace id to bind to the session row. When present in .mcp.json default_workspace, callers should pass it on registration so subsequent verbs scope to this workspace.")),
				mcpgo.WithString("project_id", mcpgo.Description("Optional project id. When supplied + no explicit session_id, the handler tries to resume the caller's most recent non-stale session bound to this project. Also stamped as active_project_id on the resulting session row.")),
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

	// story_33e1a323: re-seed the system-tier configuration markdown
	// without restarting the server. Gated to global_admin via
	// auth.CallerIdentity.GlobalAdmin (story_3548cde2).
	systemSeedTool := mcpgo.NewTool("system_seed_run",
		mcpgo.WithDescription("Re-run the system-tier configseed loader (config/seed + config/help). Global admin only. Returns a summary {loaded, created, updated, skipped, errors, ledger_id}. Each invocation writes a kind:system-seed-run ledger row."),
	)
	s.mcp.AddTool(systemSeedTool, s.handleSystemSeedRun)

	// sty_8868eaf4: re-seed one project's tier from
	// config/seed/<project_id>/<kind>/*.md. Idempotent on body hash;
	// writes a kind:project-seed-run ledger row attached to the
	// project. Global admin only. System rows are never touched.
	projectSeedTool := mcpgo.NewTool("project_seed_run",
		mcpgo.WithDescription("Re-run the project-tier configseed loader for one project (config/seed/<project_id>/<kind>/*.md). Global admin only. Produces only scope=project rows; system seeding is unchanged. Returns a summary {project_id, loaded, created, updated, skipped, errors, ledger_id}. Each invocation writes a kind:project-seed-run ledger row attached to the project."),
		mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Project to re-seed. The project must already exist; the loader does not auto-create projects.")),
	)
	s.mcp.AddTool(projectSeedTool, s.handleProjectSeedRun)

	// sty_51571015: agent dispatch is seed-prescribed, not Go code.
	// The orchestrator session reads the dispatch mechanism from its
	// handshake (default_agent_process artifact body + agent docs) and
	// executes `claude -p` via its own Bash tool — the substrate does
	// not exec subprocesses for dispatch. Configuration over code:
	// tune dispatch by editing config/seed/system/artifacts/default_agent_process.md
	// or config/seed/system/agents/*.md, then call system_seed_run.

	if s.tasks != nil {
		// task_plan is the only remaining bare task-creation MCP verb
		// (sty_c6d76a5b checkpoint 12 retired task_enqueue + task_publish).
		// task_plan stages a bare draft at status=planned; task_add is
		// the single-task creation path that lands at status=published.
		taskCommonOpts := []mcpgo.ToolOption{
			mcpgo.WithString("origin", mcpgo.Required(), mcpgo.Description("story_stage | scheduled | story_producing | event")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Workspace scope. Defaults to caller's first membership.")),
			mcpgo.WithString("project_id", mcpgo.Description("Optional project scope.")),
			mcpgo.WithString("kind", mcpgo.Description("Optional task kind discriminator. Today: \"review\" (consumed by the embedded reviewer service) vs \"work\" (everything else).")),
			mcpgo.WithString("agent_id", mcpgo.Description("Document id of the agent that should execute this task. Stamped on the task row; used to authorise claim and to route the conversation. Inherited from parent_task_id when omitted.")),
			mcpgo.WithString("parent_task_id", mcpgo.Description("Anchors this task to the conversation thread it extends — typically the implement task whose close emitted this successor. The substrate inherits project_id / agent_id from the parent when those args are omitted.")),
			mcpgo.WithString("prior_task_id", mcpgo.Description("Links a fresh implement task to the prior implement task it succeeds in the same-slot retry chain authored by the reviewer's contract prose. Distinct from parent_task_id (the conversation anchor): prior_task_id is the same-slot retry pointer.")),
			mcpgo.WithString("priority", mcpgo.Description("critical | high | medium (default) | low")),
			mcpgo.WithString("trigger", mcpgo.Description("Free-form JSON trigger payload.")),
			mcpgo.WithString("expected_duration", mcpgo.Description("Optional Go duration string (e.g. \"30s\") used by claim-expiry watchdog.")),
		}

		planOpts := append([]mcpgo.ToolOption{mcpgo.WithDescription("Write a task at status=planned (the agent-local drafting state). Subscribers do not see planned rows. task_plan covers bare drafts staged for later publication; task_add is the single-task creation path that lands at status=published. sty_c1200f75.")}, taskCommonOpts...)
		s.mcp.AddTool(mcpgo.NewTool("task_plan", planOpts...), s.handleTaskPlan)

		getTaskTool := mcpgo.NewTool("task_get",
			mcpgo.WithDescription("Return a task by id. Workspace-scoped."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id.")),
		)
		s.mcp.AddTool(getTaskTool, s.handleTaskGet)

		listTaskTool := mcpgo.NewTool("task_list",
			mcpgo.WithDescription("List tasks matching filters. Workspace-scoped. Supports filtering on story_id and kind. Archived rows (sty_dc2998c5 retention sweep) are excluded by default; pass include_archived=true to opt in."),
			mcpgo.WithString("origin", mcpgo.Description("Filter by origin.")),
			mcpgo.WithString("status", mcpgo.Description("Filter by status.")),
			mcpgo.WithString("priority", mcpgo.Description("Filter by priority.")),
			mcpgo.WithString("claimed_by", mcpgo.Description("Filter by claimed_by worker id.")),
			mcpgo.WithString("story_id", mcpgo.Description("Filter by owning story.")),
			mcpgo.WithString("kind", mcpgo.Description("Filter by task kind (review | work).")),
			mcpgo.WithBoolean("include_archived", mcpgo.Description("Include rows with status=archived. Default false — the retention sweep moves closed rows older than the project window into archived; opt in to include them in history queries.")),
			mcpgo.WithNumber("limit", mcpgo.Description("Max rows to return.")),
		)
		s.mcp.AddTool(listTaskTool, s.handleTaskList)

		claimTaskTool := mcpgo.NewTool("task_claim",
			mcpgo.WithDescription("Atomic claim: picks highest-priority oldest-queued task from the worker's workspace(s). Returns null when queue is empty. Writes a kind:task-claimed ledger row."),
			mcpgo.WithString("worker_id", mcpgo.Description("Worker id. Defaults to the caller's user id.")),
			mcpgo.WithString("workspace_id", mcpgo.Description("Narrow to one workspace. Defaults to all caller memberships.")),
		)
		s.mcp.AddTool(claimTaskTool, s.handleTaskClaim)

		// sty_41488515 / sty_c6d76a5b: task_walk returns one coherent
		// payload describing where a story sits in its task chain —
		// story header, ordered task list with per-task action / kind /
		// status / claimer / iteration, a current_task_id pointer, and
		// a per-action summary. Read-only — no state mutation.
		taskWalkTool := mcpgo.NewTool("task_walk",
			mcpgo.WithDescription("Return where a story sits in its task chain: story header, ordered tasks with action / kind / status / claimer / iteration, a current_task_id pointer, and a per-action summary (work/review counts + ledger row count). Single roundtrip orientation. Workspace-scoped. Sty_41488515."),
			mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story whose walk should be returned.")),
		)
		s.mcp.AddTool(taskWalkTool, s.handleTaskWalk)

		// sty_a248f4df: story_export_walk renders the same walk projection
		// as paste-ready markdown for PR descriptions, delivery reports,
		// and stakeholder hand-offs. Currently markdown-only; other
		// formats are out of scope.
		exportWalkTool := mcpgo.NewTool("story_export_walk",
			mcpgo.WithDescription("Render a story's contract walk as paste-ready markdown. Returns {filename, content, format}. Iteration loops collapse under a single H2 header (\"## develop ×3 (loop)\"); each CI in the loop becomes an H3 subsection with role, outcome, timestamps, claimer, and ledger anchor counts. Sty_a248f4df."),
			mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story whose walk should be exported.")),
			mcpgo.WithString("format", mcpgo.Description("Output format. Currently only \"markdown\" (default).")),
		)
		s.mcp.AddTool(exportWalkTool, s.handleStoryExportWalk)

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

		searchTool := mcpgo.NewTool("repo_search",
			mcpgo.WithDescription("Symbol search via the satellites code indexer. Writes a kind:repo-query audit row. Returns the indexer payload as JSON. Indexer outage → structured `code_index_unavailable` error."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Search query.")),
			mcpgo.WithString("kind", mcpgo.Description("Optional symbol kind filter.")),
			mcpgo.WithString("language", mcpgo.Description("Optional language filter.")),
		)
		s.mcp.AddTool(searchTool, s.handleRepoSearch)

		searchTextTool := mcpgo.NewTool("repo_search_text",
			mcpgo.WithDescription("Full-text search via the satellites code indexer. Writes a kind:repo-query audit row."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("Search query.")),
			mcpgo.WithString("file_pattern", mcpgo.Description("Optional file glob.")),
		)
		s.mcp.AddTool(searchTextTool, s.handleRepoSearchText)

		symbolSourceTool := mcpgo.NewTool("repo_get_symbol_source",
			mcpgo.WithDescription("Source of one symbol via the satellites code indexer."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("symbol_id", mcpgo.Required(), mcpgo.Description("Indexer-internal symbol id.")),
		)
		s.mcp.AddTool(symbolSourceTool, s.handleRepoGetSymbolSource)

		fileTool := mcpgo.NewTool("repo_get_file",
			mcpgo.WithDescription("Raw file content via the satellites code indexer."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Repo-relative file path.")),
		)
		s.mcp.AddTool(fileTool, s.handleRepoGetFile)

		outlineTool := mcpgo.NewTool("repo_get_outline",
			mcpgo.WithDescription("File outline (symbols + nesting) via the satellites code indexer."),
			mcpgo.WithString("repo_id", mcpgo.Required(), mcpgo.Description("Repo id.")),
			mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Repo-relative file path.")),
		)
		s.mcp.AddTool(outlineTool, s.handleRepoGetOutline)
	}

	// changelog_*: V3 parity port (sty_12af0bdc). All five verbs honour
	// the `?project_id=` URL scope; cross-project access returns
	// not-found. Service is a free-form discriminator (satellites,
	// satellites-agent, plus future binaries).
	if s.changelog != nil {
		addChangelogTool := mcpgo.NewTool("changelog_add",
			mcpgo.WithDescription("Append a changelog row for one binary in a project. Newest-first ordering on List. Service is free-form; conventions: satellites, satellites-agent, plus future binaries."),
			mcpgo.WithString("project_id", mcpgo.Description("Project scope. Defaults to caller's first owned project.")),
			mcpgo.WithString("service", mcpgo.Required(), mcpgo.Description("Binary the row describes (e.g. satellites, satellites-agent).")),
			mcpgo.WithString("version_from", mcpgo.Description("Prior version (e.g. 0.0.165).")),
			mcpgo.WithString("version_to", mcpgo.Description("New version (e.g. 0.0.166).")),
			mcpgo.WithString("content", mcpgo.Required(), mcpgo.Description("Markdown body. The first line is treated as the heading by the portal panel.")),
			mcpgo.WithString("effective_date", mcpgo.Description("RFC3339 timestamp. Defaults to now.")),
		)
		s.mcp.AddTool(addChangelogTool, s.handleChangelogAdd)

		getChangelogTool := mcpgo.NewTool("changelog_get",
			mcpgo.WithDescription("Return a changelog row by id. Workspace-scoped — cross-workspace returns not-found."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Changelog id (chg_<8hex>).")),
		)
		s.mcp.AddTool(getChangelogTool, s.handleChangelogGet)

		listChangelogTool := mcpgo.NewTool("changelog_list",
			mcpgo.WithDescription("List changelog rows in a project. Newest-first by created_at. Filter by service when set."),
			mcpgo.WithString("project_id", mcpgo.Description("Project scope. Defaults to caller's first owned project.")),
			mcpgo.WithString("service", mcpgo.Description("Optional service filter.")),
			mcpgo.WithNumber("limit", mcpgo.Description("Max rows (default 50, max 500).")),
		)
		s.mcp.AddTool(listChangelogTool, s.handleChangelogList)

		updateChangelogTool := mcpgo.NewTool("changelog_update",
			mcpgo.WithDescription("Edit a changelog row. Service / project / workspace identity is set at create and not editable here. Pass only the fields you want to change."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Changelog id.")),
			mcpgo.WithString("version_from", mcpgo.Description("New prior version.")),
			mcpgo.WithString("version_to", mcpgo.Description("New version_to.")),
			mcpgo.WithString("content", mcpgo.Description("New markdown body.")),
			mcpgo.WithString("effective_date", mcpgo.Description("New RFC3339 effective date.")),
		)
		s.mcp.AddTool(updateChangelogTool, s.handleChangelogUpdate)

		deleteChangelogTool := mcpgo.NewTool("changelog_delete",
			mcpgo.WithDescription("Delete a changelog row. Workspace-scoped — cross-workspace returns not-found."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Changelog id.")),
		)
		s.mcp.AddTool(deleteChangelogTool, s.handleChangelogDelete)
	}

	// portal_replicate: chromedp-driven UI replication, story-scoped.
	// Sty_088f6d5c. Requires the story store (to validate scope) and
	// the ledger (to attach per-action evidence). The vocabulary is
	// installed separately via SetReplicateVocabulary so configseed's
	// post-boot phase can swap in a richer alias map without
	// re-registering the tool.
	if err := s.requireReplicatePrereqs(); err == nil {
		s.replicateVocab = portalreplicate.NewVocabulary()
		s.registerPortalReplicate()
	}

	// Use a tolerant session id manager (sty_31975268): generate a UUID
	// on initialize so the response carries Mcp-Session-Id, but accept
	// empty session ids on non-initialize calls so legacy callers that
	// pass session_id as a body argument still work.
	s.streamable = mcpserver.NewStreamableHTTPServer(s.mcp,
		mcpserver.WithSessionIdManager(&tolerantSessionIDManager{
			inner: &mcpserver.StatelessGeneratingSessionIdManager{},
		}),
	)
	return s
}

// tolerantSessionIDManager is a SessionIdManager that mints UUIDs on
// initialize (so the Mcp-Session-Id round-trip works for spec-compliant
// Streamable HTTP clients) while accepting empty session ids on every
// other call (so legacy stdio-style callers + tests that don't echo the
// header continue to function via the body session_id parameter).
// story_31975268.
type tolerantSessionIDManager struct {
	inner mcpserver.SessionIdManager
}

func (t *tolerantSessionIDManager) Generate() string {
	return t.inner.Generate()
}

func (t *tolerantSessionIDManager) Validate(sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	return t.inner.Validate(sessionID)
}

func (t *tolerantSessionIDManager) Terminate(sessionID string) (bool, error) {
	return t.inner.Terminate(sessionID)
}

// ServeHTTP implements http.Handler. AuthMiddleware is responsible for
// establishing the user context before this handler runs. ServeHTTP also
// extracts an optional ?project_id=<id> from the request URL and stores
// it on the context as the URL-scoped project. Tool handlers consult
// ScopedProjectIDFrom (or use enforceScopedProject) to reject any tool
// call that names a different project — V3-style project-scoped MCP
// endpoints, so .mcp.json can pin a single project per server entry.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if scoped := r.URL.Query().Get("project_id"); scoped != "" {
		r = r.WithContext(withScopedProjectID(r.Context(), scoped))
	}
	// Stash the externally-visible base URL so buildProjectView's MCP
	// URL resolver can derive a value without requiring SATELLITES_PUBLIC_URL.
	// V3 parity — the caller is already connected, so the host they
	// reached is the right one to echo back as mcp_url.
	r = r.WithContext(withRequestBaseURL(r.Context(), auth.SchemeAndHost(r)))
	s.streamable.ServeHTTP(w, r)
}

// handleInfo is the satellites_info tool implementation. Thin
// forwarder per cli-primary order:07a-layer-2 (sty_df1cb227): parses
// no args, calls the typed surface, marshals the result.
func (s *Server) handleInfo(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().SatellitesInfo(ctx, client.Caller{
		UserID:      caller.UserID,
		Email:       caller.Email,
		Memberships: s.resolveCallerMemberships(ctx, caller),
	}, client.SatellitesInfoInput{})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, err := json.Marshal(map[string]any{
		"version":    out.Version,
		"build":      out.Build,
		"commit":     out.Commit,
		"user_email": out.UserEmail,
		"started_at": out.StartedAt.Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "satellites_info").
		Str("user_email", out.UserEmail).
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
// caller. Rules: (1) if the request URL carries ?project_id= scoping,
// any explicit `requested` must match it (cross-project tool calls are
// rejected); when `requested` is empty, the URL-scoped value is used.
// (2) if req supplies project_id, the caller must own that project or
// it must be the system default; cross-project access returns an error.
// (3) otherwise, fall back to the caller's first owned project.
// (4) otherwise, fall back to the system default.
func (s *Server) resolveProjectID(ctx context.Context, requested string, caller auth.CallerIdentity, memberships []string) (string, error) {
	effective, ok := enforceScopedProject(ctx, requested)
	if !ok {
		return "", errors.New("project_id parameter does not match the URL-scoped project_id")
	}
	requested = effective
	if requested != "" {
		if requested == s.defaultProjectID {
			return requested, nil
		}
		// story_3548cde2: global_admin callers may resolve any project
		// regardless of workspace membership or ownership. The
		// impersonating_as_workspace audit field captures the
		// cross-tenancy write at ledger-stamp time.
		lookupMemberships := memberships
		if caller.GlobalAdmin {
			lookupMemberships = nil
		}
		p, err := s.projectsSafe().GetByID(ctx, requested, lookupMemberships)
		if err != nil {
			return "", errors.New("project not found or access denied")
		}
		if p.OwnerUserID != caller.UserID && !caller.GlobalAdmin {
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
func (s *Server) ensureCallerWorkspaces(ctx context.Context, caller auth.CallerIdentity) []string {
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
func (s *Server) resolveCallerWorkspaceID(ctx context.Context, caller auth.CallerIdentity) string {
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
func (s *Server) resolveCallerMemberships(ctx context.Context, caller auth.CallerIdentity) []string {
	return s.ensureCallerWorkspaces(ctx, caller)
}

// ledgerWorkspaceInMemberships reports whether wsID is in the caller's
// memberships slice. Used by handleLedgerAppend to decide whether a
// write crosses the tenancy boundary and warrants stamping
// impersonating_as_workspace. story_3548cde2.
func ledgerWorkspaceInMemberships(wsID string, memberships []string) bool {
	if wsID == "" || len(memberships) == 0 {
		return false
	}
	for _, m := range memberships {
		if m == wsID {
			return true
		}
	}
	return false
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
	caller, _ := auth.UserFrom(ctx)
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

// handleDocumentGet implements document_get. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227 Slice B): resolves
// project + workspace context and delegates to client.DocumentGet.
func (s *Server) handleDocumentGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id := req.GetString("id", "")
	name := req.GetString("name", "")
	if id == "" && name == "" {
		return mcpgo.NewToolResultError("either id or name is required"), nil
	}
	// Hierarchical name lookup leniency (sty_e2bfeffa): a caller with
	// no owned project still resolves system-tier rows by name, so an
	// error from resolveProjectID drops us to resolvedID="" rather
	// than aborting the request.
	resolvedID, projErr := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if projErr != nil {
		resolvedID = ""
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	if wsID == "" {
		wsID = s.resolveCallerWorkspaceID(ctx, caller)
	}
	doc, err := s.cli().DocumentGet(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.DocumentGetInput{
		ID:                id,
		Name:              name,
		Type:              req.GetString("type", ""),
		WorkspaceID:       wsID,
		ResolvedProjectID: resolvedID,
		Memberships:       memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(doc)
	event := s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_get").
		Int64("duration_ms", time.Since(start).Milliseconds())
	if id != "" {
		event.Str("id", id)
	} else {
		event.Str("project_id", resolvedID).Str("workspace_id", wsID).Str("name", name).Str("scope", doc.Scope)
	}
	event.Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// immutableUpdateFields are the document keys that document_update must
// reject if the caller supplies them. The Store interface's UpdateFields
// only carries the mutable subset, so the only place to enforce this is
// the handler.
var immutableUpdateFields = []string{"workspace_id", "project_id", "type", "scope", "name", "id"}

func (s *Server) handleDocumentCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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
		// sty_e2512dbd: system tier is non-tenant. Drop the
		// caller-stamped workspace; Validate() rejects non-empty
		// workspace_id at scope=system.
		doc.WorkspaceID = ""
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

	now := time.Now().UTC()
	created, err := s.docs.Create(ctx, doc, now)
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
	caller, _ := auth.UserFrom(ctx)
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
	// sty_e2512dbd: system tier rows have no workspace; membership-
	// scoped writes would 404 them. Read the row workspace-blind to
	// learn its scope, then drop memberships for system-scope writes.
	if existing, gerr := s.docs.GetByID(ctx, id, nil); gerr == nil && existing.Scope == document.ScopeSystem {
		memberships = nil
	}
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

// handleDocumentList implements document_list. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227 Slice B). The lenient
// project-resolution semantics (sty_e2bfeffa) — a caller without an
// owned project still sees workspace + system tiers — are preserved
// at this seam; client.DocumentList walks the same tier ladder via
// the store's ResolveList.
func (s *Server) handleDocumentList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, projErr := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if projErr != nil {
		resolvedID = ""
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	if wsID == "" {
		wsID = s.resolveCallerWorkspaceID(ctx, caller)
	}
	opts := document.ListOptions{
		Type:            req.GetString("type", ""),
		Scope:           req.GetString("scope", ""),
		ContractBinding: req.GetString("contract_binding", ""),
		ProjectID:       resolvedID,
		Tags:            req.GetStringSlice("tags", nil),
		Limit:           int(req.GetFloat("limit", 0)),
	}
	if opts.Limit > 500 {
		opts.Limit = 500
	}
	rows, err := s.cli().DocumentList(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.DocumentListInput{
		Options:     opts,
		WorkspaceID: wsID,
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_list").
		Str("type", opts.Type).
		Str("scope", opts.Scope).
		Str("project_id", resolvedID).
		Str("workspace_id", wsID).
		Int("count", len(rows)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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
	// Route the non-empty-query branch to SearchSemantic (story_5abfe61c).
	// On ErrSemanticUnavailable (deploy without an embedder configured)
	// fall back to the structured-filter Search so callers don't error
	// out — they just get a filter-only result instead of semantic
	// ranking.
	var rows []document.Document
	var err error
	if opts.Query != "" {
		rows, err = searchSemanticScoped(ctx, s.docs, opts.Query, opts, memberships)
		if errors.Is(err, document.ErrSemanticUnavailable) {
			rows, err = searchScoped(ctx, s.docs, opts, memberships)
		}
	} else {
		rows, err = searchScoped(ctx, s.docs, opts, memberships)
	}
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
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	mode := document.DeleteMode(req.GetString("mode", string(document.DeleteArchive)))
	memberships := s.resolveCallerMemberships(ctx, caller)
	// sty_e2512dbd: system tier rows have no workspace; drop
	// memberships when deleting one so the membership predicate
	// doesn't 404 it.
	if existing, gerr := s.docs.GetByID(ctx, id, nil); gerr == nil && existing.Scope == document.ScopeSystem {
		memberships = nil
	}

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

// projectView is the JSON-marshalled response shape for project_create /
// project_get / project_update. It embeds the durable project.Project row
// and adds two computed fields — `mcp_url` and `mcp_config` — derived
// from the configured public base URL. These let `.mcp.json` be written
// directly from project_get's output without the operator constructing a
// URL by hand. project_list intentionally returns plain Project rows so
// listings stay lightweight.
type projectView struct {
	project.Project
	MCPURL    string         `json:"mcp_url,omitempty"`
	MCPConfig map[string]any `json:"mcp_config,omitempty"`
}

func (s *Server) buildProjectView(ctx context.Context, p project.Project) projectView {
	pv := projectView{Project: p}
	// Resolution chain (V3 parity):
	//   1. p.MCPURL persisted on the row (explicit override).
	//   2. Inbound request's base URL stashed by ServeHTTP — the host
	//      the caller is already connected to.
	//   3. cfg.PublicURL — admin override for deployments where the
	//      external host differs from the one the request came in on.
	//   4. cfg.OAuthRedirectBaseURL — back-compat for setups that pre-date
	//      the PublicURL field.
	base := requestBaseURLFrom(ctx)
	if base == "" {
		base = s.cfg.PublicURL
	}
	if base == "" {
		base = s.cfg.OAuthRedirectBaseURL
	}
	url := project.ResolveMCPURL(p, base)
	if url == "" {
		return pv
	}
	pv.MCPURL = url
	pv.MCPConfig = map[string]any{
		"mcpServers": map[string]any{
			"satellites": map[string]any{
				"type": "http",
				"url":  url,
			},
		},
	}
	return pv
}

func (s *Server) handleProjectCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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
	body, _ := json.Marshal(s.buildProjectView(ctx, p))
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_create").
		Str("project_id", p.ID).
		Str("owner_user_id", p.OwnerUserID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleProjectGet implements `project_get`: returns the orientation
// bundle for an explicit project id — project row + mcp_url + mcp_config
// + intent_body + active principles. Single-shot, no session binding.
// sty_48e38e83 merged the prior thin row-only `project_get` with the
// session-keyed `project_context` no-arg refresh.
func (s *Server) handleProjectGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if _, ok := enforceScopedProject(ctx, id); !ok {
		return mcpgo.NewToolResultError("project id does not match the URL-scoped project_id"), nil
	}
	p, err := s.projects.GetByID(ctx, id, memberships)
	if err != nil || p.OwnerUserID != caller.UserID {
		return mcpgo.NewToolResultError("project not found"), nil
	}
	view := s.buildProjectView(ctx, p)
	bundle := s.cli().BuildOrientation(ctx, p)
	body, _ := json.Marshal(map[string]any{
		"project":     view,
		"intent_body": bundle.IntentBody,
		"principles":  bundle.Principles,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_get").
		Str("project_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleProjectSet implements `project_set`: idempotent bind from a git
// remote URL to an existing project in the caller's workspace. Never
// creates a project. sty_4db7c3a3.
//
// Response shapes:
//
//	{"project_id":"proj_…","status":"resolved","mcp_url":"…","repo_url_canonical":"…"}
//	{"status":"no_project_for_remote","repo_url_canonical":"…"}
// handleProjectSet implements `project_set`. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227 Slice C): resolves
// workspace + session-id from request context, delegates resolution +
// orientation to client.ProjectSet, layers wire-specific fields
// (mcp_url, mcp_config) onto the response.
func (s *Server) handleProjectSet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	repoURL := req.GetString("repo_url", "")
	out, err := s.cli().ProjectSet(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: s.resolveCallerMemberships(ctx, caller)}, client.ProjectSetInput{
		RepoURL:     repoURL,
		WorkspaceID: s.resolveCallerWorkspaceID(ctx, caller),
		SessionID:   resolveSessionID(ctx, req.GetString("session_id", "")),
		Now:         s.nowUTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, client.ErrRepoURLRequired):
			return mcpgo.NewToolResultError("repo_url_required"), nil
		case errors.Is(err, client.ErrRepoURLInvalid):
			return mcpgo.NewToolResultError("repo_url_invalid"), nil
		default:
			return mcpgo.NewToolResultError(err.Error()), nil
		}
	}
	if out.Status == client.ProjectSetStatusNoProject {
		body, _ := json.Marshal(map[string]any{
			"status":             "no_project_for_remote",
			"repo_url_canonical": out.RepoURLCanonical,
		})
		s.logger.Info().
			Str("method", "tools/call").
			Str("tool", "project_set").
			Str("status", "no_project_for_remote").
			Str("repo_url_canonical", out.RepoURLCanonical).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Msg("mcp tool call")
		return mcpgo.NewToolResultText(string(body)), nil
	}
	view := s.buildProjectView(ctx, out.ResolvedProject)
	body, _ := json.Marshal(map[string]any{
		"project_id":         out.ResolvedProject.ID,
		"status":             "resolved",
		"mcp_url":            view.MCPURL,
		"mcp_config":         view.MCPConfig,
		"repo_url_canonical": out.RepoURLCanonical,
		"intent_body":        out.Orientation.IntentBody,
		"principles":         out.Orientation.Principles,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_set").
		Str("status", "resolved").
		Str("project_id", out.ResolvedProject.ID).
		Str("repo_url_canonical", out.RepoURLCanonical).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if _, ok := enforceScopedProject(ctx, id); !ok {
		return mcpgo.NewToolResultError("project id does not match the URL-scoped project_id"), nil
	}
	existing, err := s.projects.GetByID(ctx, id, memberships)
	if err != nil || existing.OwnerUserID != caller.UserID {
		return mcpgo.NewToolResultError("project not found"), nil
	}
	now := time.Now().UTC()
	updated := existing
	if name := req.GetString("name", ""); name != "" && name != existing.Name {
		var renameErr error
		updated, renameErr = s.projects.UpdateName(ctx, id, name, now)
		if renameErr != nil {
			return mcpgo.NewToolResultError(renameErr.Error()), nil
		}
	}
	// mcp_url override: present-vs-absent treatment.
	if mcpURL, ok := req.GetArguments()["mcp_url"]; ok {
		mcpStr, _ := mcpURL.(string)
		if mcpStr != updated.MCPURL {
			next, mcpErr := s.projects.SetMCPURL(ctx, id, mcpStr, now)
			if mcpErr != nil {
				return mcpgo.NewToolResultError(mcpErr.Error()), nil
			}
			updated = next
		}
	}
	body, _ := json.Marshal(s.buildProjectView(ctx, updated))
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_update").
		Str("project_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if _, ok := enforceScopedProject(ctx, id); !ok {
		return mcpgo.NewToolResultError("project id does not match the URL-scoped project_id"), nil
	}
	existing, err := s.projects.GetByID(ctx, id, memberships)
	if err != nil || existing.OwnerUserID != caller.UserID {
		return mcpgo.NewToolResultError("project not found"), nil
	}
	updated, err := s.projects.SetStatus(ctx, id, project.StatusArchived, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_delete").
		Str("project_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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

// handleLedgerAppend implements ledger_append. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227 Slice B): resolves
// project/workspace + global_admin impersonation policy, parses
// structured/expires_at, delegates to client.LedgerAppend.
func (s *Server) handleLedgerAppend(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	eventType, err := req.RequireString("type")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	in := client.LedgerAppendInput{
		ResolvedProjectID: resolvedID,
		WorkspaceID:       wsID,
		StoryID:           req.GetString("story_id", ""),
		EventType:         eventType,
		Content:           req.GetString("content", ""),
		Tags:              req.GetStringSlice("tags", nil),
		Durability:        req.GetString("durability", ""),
		SourceType:        req.GetString("source_type", ""),
		Sensitive:         req.GetBool("sensitive", false),
		Now:               time.Now().UTC(),
	}
	// story_3548cde2: stamp impersonation when a global_admin writes
	// outside their own workspace memberships.
	if caller.GlobalAdmin && wsID != "" && !ledgerWorkspaceInMemberships(wsID, memberships) {
		in.ImpersonatingAsWorkspace = wsID
	}
	if structured := req.GetString("structured", ""); structured != "" {
		if !json.Valid([]byte(structured)) {
			return mcpgo.NewToolResultError("structured must be valid JSON"), nil
		}
		in.Structured = []byte(structured)
	}
	if expires := req.GetString("expires_at", ""); expires != "" {
		t, err := time.Parse(time.RFC3339, expires)
		if err != nil {
			return mcpgo.NewToolResultError("expires_at must be RFC3339"), nil
		}
		in.ExpiresAt = &t
	}
	e, err := s.cli().LedgerAppend(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
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
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	e, err := s.cli().LedgerGet(ctx, client.Caller{
		UserID:      caller.UserID,
		Memberships: s.resolveCallerMemberships(ctx, caller),
	}, client.LedgerGetInput{ID: id})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(e)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_get").Str("id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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
	rows, err := s.cli().LedgerSearch(ctx, client.Caller{UserID: caller.UserID, Memberships: memberships},
		client.LedgerSearchInput{ResolvedProjectID: resolvedID, Memberships: memberships, Options: opts})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_search").Str("project_id", resolvedID).Str("query", opts.Query).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerRecall(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	rootID, err := req.RequireString("root_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	rows, err := s.cli().LedgerRecall(ctx, client.Caller{UserID: caller.UserID, Memberships: memberships},
		client.LedgerRecallInput{RootID: rootID, Memberships: memberships})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_recall").Str("root_id", rootID).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLedgerDereference(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	reason, err := req.RequireString("reason")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	audit, err := s.cli().LedgerDereference(ctx, client.Caller{UserID: caller.UserID, Memberships: memberships},
		client.LedgerDereferenceInput{ID: id, Reason: reason, Memberships: memberships, Now: s.nowUTC()})
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
	caller, _ := auth.UserFrom(ctx)
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

	candidate := story.Story{
		WorkspaceID:        wsID,
		ProjectID:          resolvedID,
		Title:              title,
		Description:        req.GetString("description", ""),
		AcceptanceCriteria: req.GetString("acceptance_criteria", ""),
		Priority:           req.GetString("priority", "medium"),
		Category:           req.GetString("category", "feature"),
		Tags:               tagsRaw,
		CreatedBy:          caller.UserID,
	}
	st, err := s.stories.Create(ctx, candidate, time.Now().UTC())
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

// handleStoryUpdate updates a story's mutable non-status fields. The
// per-call surface (sty_330cc4ab, V3 parity) accepts title, description,
// acceptance_criteria, category, priority, and tags. Omitted fields are
// left untouched; tags replace wholesale — an empty array clears the
// list. Status transitions remain on story_update_status.
func (s *Server) handleStoryUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	current, err := s.stories.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, current.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}

	fields, err := buildStoryUpdateFields(req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	updated, err := s.stories.Update(ctx, id, fields, caller.UserID, time.Now().UTC(), memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_update").
		Str("story_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// validStoryCategories enumerates the categories accepted by story_create
// and story_update. Kept here (rather than in the story package) because
// it gates the MCP surface — the store layer is intentionally
// schema-free on category strings.
var validStoryCategories = map[string]struct{}{
	"feature":        {},
	"bug":            {},
	"improvement":    {},
	"infrastructure": {},
	"documentation":  {},
}

// buildStoryUpdateFields reads optional update fields from req. A field
// is "provided" when its key is present in the argument map (regardless
// of value), so callers can clear strings or tags by passing an empty
// value. Returns a structured error when category is provided but not
// in the allowed set.
func buildStoryUpdateFields(req mcpgo.CallToolRequest) (story.UpdateFields, error) {
	args := req.GetArguments()
	fields := story.UpdateFields{}
	if _, ok := args["title"]; ok {
		v := req.GetString("title", "")
		fields.Title = &v
	}
	if _, ok := args["description"]; ok {
		v := req.GetString("description", "")
		fields.Description = &v
	}
	if _, ok := args["acceptance_criteria"]; ok {
		v := req.GetString("acceptance_criteria", "")
		fields.AcceptanceCriteria = &v
	}
	if _, ok := args["category"]; ok {
		v := req.GetString("category", "")
		if v != "" {
			if _, allowed := validStoryCategories[v]; !allowed {
				return story.UpdateFields{}, fmt.Errorf("invalid category %q (allowed: feature | bug | improvement | infrastructure | documentation)", v)
			}
		}
		fields.Category = &v
	}
	if _, ok := args["priority"]; ok {
		v := req.GetString("priority", "")
		fields.Priority = &v
	}
	if _, ok := args["tags"]; ok {
		v := req.GetStringSlice("tags", []string{})
		if v == nil {
			v = []string{}
		}
		fields.Tags = &v
	}
	return fields, nil
}

// loadStoryTemplate resolves a category → story.Template by reading the
// system-scope document with type=story_template + name=category. Sets
// the lookup is best-effort: missing or malformed templates return
// (zero, false), which the caller treats as "no hooks for this
// category". Sty_d2a03cea.
func (s *Server) loadStoryTemplate(ctx context.Context, category string) (story.Template, bool) {
	if s.docs == nil || category == "" {
		return story.Template{}, false
	}
	// nil memberships → see system-scope rows regardless of caller's
	// workspace. Templates are global by design.
	doc, err := s.docs.GetByName(ctx, "", category, nil)
	if err != nil {
		return story.Template{}, false
	}
	if doc.Type != document.TypeStoryTemplate {
		return story.Template{}, false
	}
	t, err := story.LoadTemplate(doc)
	if err != nil {
		return story.Template{}, false
	}
	return t, true
}

func (s *Server) handleStoryList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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

// handleStoryUpdateStatus implements story_update_status. Thin
// forwarder per cli-primary order:07a-layer-2 (sty_df1cb227 Slice B).
// The wire layer pre-resolves project access; client.StoryUpdateStatus
// owns template-hook evaluation + the UpdateStatus call.
func (s *Server) handleStoryUpdateStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	status, err := req.RequireString("status")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	// Pre-resolve project access. Existing-row lookup mirrors the
	// typed method's so the wire layer can reject cross-tenant access
	// with the same "story not found" envelope the typed method uses
	// for membership-scoped misses.
	existing, err := s.stories.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, existing.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	updated, err := s.cli().StoryUpdateStatus(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.StoryUpdateStatusInput{
		ID:          id,
		Status:      status,
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
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

// handleStoryFieldSet writes a single template-defined value onto a
// story. Validates the field name against the resolved category
// template — fields not declared by the template are rejected with a
// list of what the template does declare. Sty_d2a03cea.
// handleStoryFieldSet implements story_field_set. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227 Slice B).
func (s *Server) handleStoryFieldSet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	field, err := req.RequireString("field")
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
	updated, err := s.cli().StoryFieldSet(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.StoryFieldSetInput{
		ID:          id,
		Field:       field,
		Value:       req.GetString("value", ""),
		Memberships: memberships,
		Now:         s.nowUTC(),
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_field_set").
		Str("story_id", id).
		Str("field", field).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleStoryTemplateGet returns the parsed Template for the given
// category. Convenience over document_get with name=category +
// type=story_template filter. Sty_d2a03cea.
func (s *Server) handleStoryTemplateGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	category, err := req.RequireString("category")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	t, ok := s.loadStoryTemplate(ctx, category)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("no story template registered for category %q", category)), nil
	}
	body, _ := json.Marshal(t)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_template_get").
		Str("category", category).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleStoryTemplateList returns every system-scope story_template
// document parsed into Template form. Convenience over document_list
// with type=story_template filter. Sty_d2a03cea.
func (s *Server) handleStoryTemplateList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	if s.docs == nil {
		body, _ := json.Marshal([]story.Template{})
		return mcpgo.NewToolResultText(string(body)), nil
	}
	docs, err := s.docs.List(ctx, document.ListOptions{
		Type:  document.TypeStoryTemplate,
		Scope: document.ScopeSystem,
		Limit: 100,
	}, nil)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	out := make([]story.Template, 0, len(docs))
	for _, d := range docs {
		t, err := story.LoadTemplate(d)
		if err != nil {
			s.logger.Warn().Str("document_id", d.ID).Str("error", err.Error()).Msg("story_template parse failed; skipping")
			continue
		}
		out = append(out, t)
	}
	body, _ := json.Marshal(out)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_template_list").
		Int("count", len(out)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleWorkspaceCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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
	caller, _ := auth.UserFrom(ctx)
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
	caller, _ := auth.UserFrom(ctx)
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
func (s *Server) requireWorkspaceAdmin(ctx context.Context, caller auth.CallerIdentity, workspaceID string) error {
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
	caller, _ := auth.UserFrom(ctx)
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
	caller, _ := auth.UserFrom(ctx)
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
	caller, _ := auth.UserFrom(ctx)
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
	caller, _ := auth.UserFrom(ctx)
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

// handleLedgerList implements ledger_list. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227 Slice B).
func (s *Server) handleLedgerList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
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
	entries, err := s.cli().LedgerList(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.LedgerListInput{
		ResolvedProjectID: resolvedID,
		Options:           opts,
		Memberships:       memberships,
	})
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

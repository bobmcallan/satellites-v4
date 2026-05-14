// Package mcpserver exposes the satellites MCP surface over Streamable HTTP.
// v4 currently registers: satellites_info, document_ingest_file, document_get,
// project_get/list/set, ledger_append/list, story_get/list/update,
// workspace_add/get/list. Subsequent epics add more.
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

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
)

// Server bundles the mcp-go MCPServer + StreamableHTTPServer with the
// satellites-specific dependencies needed by the tools. Sty_4db0e025
// slice A11 converged the substrate-typed field set onto a single
// `deps` of type client.Deps so this file holds no direct
// substrate-package imports — the only allowed delegation surface is
// internal/client per pr_mcp_cli_shared_path.
type Server struct {
	cfg        *config.Config
	logger     arbor.ILogger
	startedAt  time.Time
	mcp        *mcpserver.MCPServer
	streamable *mcpserver.StreamableHTTPServer
	// deps carries every substrate-typed dependency through one typed
	// surface. cli() rebuilds the *client.Client on every call so test
	// fixtures that mutate fields after New() still see the freshest
	// snapshot (the orchestratorFixture pattern that sets
	// f.server.deps.Tasks late).
	deps    client.Deps
	nowFunc func() time.Time
	audit   *auditLogger
}

// HandshakeFallbackInstructions is the literal MCP server-instructions
// string used when the agent-process artifact resolver returns empty —
// i.e. the seed hasn't run, the doc store isn't wired, or no project
// context narrows the lookup. Kept verbatim from pre-sty_e1ab884d so
// out-of-band MCP clients that grep for it during integration testing
// continue to match.
const HandshakeFallbackInstructions = "Satellites v4 — walking skeleton."

// cli returns the typed business surface used by handlers extracted
// under cli-primary order:02. Rebuilds the *client.Client on every
// call so test fixtures that mutate Server.deps after New() (the
// orchestratorFixture pattern that sets f.server.deps.Tasks late)
// reach typed methods with a fresh snapshot. story_66c02002 +
// sty_df1cb227.
func (s *Server) cli() *client.Client {
	d := s.deps
	d.StartedAt = s.startedAt
	d.Logger = s.logger
	return client.New(d)
}

// toClientCaller converts the wire-layer CallerIdentity (resolved by
// AuthMiddleware) into the typed client.Caller the migrated resolution
// helpers consume. The Memberships slice is populated by the caller
// after calling ResolveCallerMemberships; toClientCaller leaves it nil.
//
// sty_068a6c46: bridge for the resolveProjectID / resolveCallerMemberships
// migration into the client package.
func toClientCaller(c auth.CallerIdentity) client.Caller {
	return client.Caller{
		UserID:      c.UserID,
		Email:       c.Email,
		GlobalAdmin: c.GlobalAdmin,
	}
}

// Deps bundles the optional per-tool dependencies passed through to
// handlers. Sty_4db0e025 slice A11: the substrate-typed dependency
// bundle now lives on client.Deps. mcpserver.Deps wraps it plus the
// MCP-only knobs (AuditReadTTL + NowFunc); main.go constructs both
// halves and passes them to mcpserver.New.
type Deps struct {
	// Client carries every substrate-typed store the handlers reach
	// through (Documents/Projects/Ledger/Stories/Workspaces/Sessions/
	// Tasks/Repos/Changelog/APIKeys/Indexer/DocsDir/DefaultProjectID/
	// ReplicateVocab/ReplicateRunner). A zero-value client.Deps boots
	// a no-store server; the handlers that need a store gate on
	// `s.deps.<Field> != nil` at registration time.
	Client client.Deps
	// AuditReadTTL is the durability TTL applied to read-classified
	// audit rows. Zero falls back to 720h (30 days). Mutations land
	// durable and ignore this knob. Sty_1493c077.
	AuditReadTTL time.Duration
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
		cfg:       cfg,
		logger:    logger,
		startedAt: startedAt,
		deps:      deps.Client,
		nowFunc:   deps.NowFunc,
	}
	if s.deps.Indexer == nil {
		s.deps.Indexer = codeindex.NewStub()
	}
	// sty_66c02002 + sty_df1cb227: typed business surface. cli() builds
	// the per-call client.Client snapshot — no eager init here, so test
	// fixtures that wire stores after New() (the orchestratorFixture
	// pattern that sets f.server.deps.Tasks late) see them at
	// typed-method dispatch.

	// sty_1493c077: per-call audit logger. Wraps every tool handler via
	// mcp-go's middleware seam; writes one ledger row per call tagged
	// kind:mcp-call. Reads land ephemeral, mutations durable. Disabled
	// when no ledger store is wired (early-test fixtures).
	if s.deps.Ledger != nil {
		s.audit = newAuditLogger(s.cli, s.logger,
			deps.AuditReadTTL, s.deps.DefaultProjectID, s.nowFunc)
	}

	// sty_e1ab884d: handshake instructions are sourced from the
	// agent-process artifact. Resolution chain: project-scope override
	// (when this server boots into a project context — not yet wired) →
	// system-scope `default_agent_process` artifact (seeded at boot via
	// agentprocess.SeedSystemDefault). The literal "walking skeleton"
	// tagline is the back-compat fallback for boots where the seed
	// hasn't run (early-test fixtures) or the doc store is unwired.
	// Sty_4db0e025 slice A11: agentprocess.Resolve lives behind the
	// client typed surface (ResolveHandshakeInstructions).
	serverOpts := []mcpserver.ServerOption{
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions(s.cli().ResolveHandshakeInstructions(context.Background(), HandshakeFallbackInstructions)),
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

	// sty_64e69db8: system_version surfaces the GitHub Release
	// manifest stamp so satellites-client boots can detect version
	// drift. Thin forwarder onto *client.Client.SystemVersion per
	// pr_mcp_cli_shared_path.
	systemVersionTool := mcpgo.NewTool("system_version",
		mcpgo.WithDescription("Return the latest published satellites-client release stamp by fetching the configured GitHub Release manifest. Cached server-side for 60s. Read-only."),
	)
	s.mcp.AddTool(systemVersionTool, s.handleSystemVersion)

	// sty_796b8fe1: satellites_init returns the structured install /
	// refresh payload for the canonical `./satellites/` install layout.
	// Read-only — performs no disk writes. Thin forwarder onto
	// *client.Client.SatellitesInit per pr_mcp_cli_shared_path.
	satellitesInitTool := mcpgo.NewTool("satellites_init",
		mcpgo.WithDescription("Return the structured install/refresh payload for satellites-client (state machine: install_required / update_available / up_to_date). Optionally accepts current_version, os, arch to refine the state + artifact selection. Read-only."),
		mcpgo.WithString("current_version",
			mcpgo.Description("Operator-supplied stamp of the locally installed satellites-client; empty drives install_required.")),
		mcpgo.WithString("os",
			mcpgo.Description("Override server-side runtime.GOOS (linux | darwin | windows).")),
		mcpgo.WithString("arch",
			mcpgo.Description("Override server-side runtime.GOARCH (amd64 | arm64).")),
	)
	s.mcp.AddTool(satellitesInitTool, s.handleSatellitesInit)

	// sty_2f0db922: substrate_audit mints a kind=work
	// action=contract:substrate_audit task naming the substrate_auditor
	// agent. Per pr_substrate_model the audit rubric lives in markdown
	// (the contract body + agent body lifts it verbatim); this verb is
	// the thin mint surface. Thin forwarder onto *client.Client.
	// SubstrateAudit per pr_mcp_cli_shared_path. Gated on the document
	// store + task store + story store + ledger store being wired so
	// the TaskAdd dependency chain is satisfiable.
	if s.deps.Documents != nil && s.deps.Tasks != nil && s.deps.Stories != nil && s.deps.Ledger != nil {
		substrateAuditTool := mcpgo.NewTool("substrate_audit",
			mcpgo.WithDescription("Mint a kind=work action=contract:substrate_audit task naming the substrate_auditor agent. The dispatched agent runs the six-check rubric (non-drift / agent-capability / canonical-chain-coverage / principle-citation / story-template-field / orphan) against the substrate and emits one kind:audit-report ledger row carrying the structured findings + a verdict tag (verdict:audit:pass | verdict:audit:warn | verdict:audit:fail). Read-only with respect to substrate documents. Returns {task_id, story_id, agent_id, scope}."),
			mcpgo.WithString("project_id",
				mcpgo.Description("Optional project scope (proj_<8hex>). Empty falls back to the caller's resolution chain.")),
			mcpgo.WithString("workspace_id",
				mcpgo.Description("Optional workspace scope (wksp_<8hex>). Empty falls back to the caller's resolution chain.")),
		)
		s.mcp.AddTool(substrateAuditTool, s.handleSubstrateAudit)
	}

	if s.deps.Documents != nil {
		// document_ingest_file MCP registration removed in sty_4db0e025
		// slice B3 — operator-only verb, now reachable through /api/v1 +
		// the satellites-client CLI only. handleDocumentIngestFile remains
		// (mcp.go) so the typed method on *client.Client continues to back
		// the HTTP route and CLI verb.

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

		s.registerDocumentWrappers()
	}

	if s.deps.Projects != nil {
		// project_add / project_update / project_delete MCP registrations
		// removed in sty_4db0e025 slice C9 — operator authoring per
		// sty_3dc39a5c "Removed from MCP" list. Reachable through /api/v1
		// + the satellites-client CLI only. handleProjectAdd /
		// handleProjectUpdate / handleProjectDelete remain so the typed
		// methods on *client.Client continue to back the HTTP routes and
		// CLI verbs.

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
			mcpgo.WithString("repo_url", mcpgo.Required(), mcpgo.Description("Git remote URL — accepts ssh, https, or git:// forms. Normalised server-side via the same canonicaliser project_add uses. Typically `git remote get-url origin` from the working directory.")),
			mcpgo.WithString("session_id", mcpgo.Description("Optional explicit session id. Streamable HTTP callers should let the Mcp-Session-Id header carry the id; this arg is for stdio/test callers that can't set the header.")),
		)
		s.mcp.AddTool(setProjTool, s.handleProjectSet)
	}

	if s.deps.Ledger != nil {
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

	if s.deps.Stories != nil {
		addStoryTool := mcpgo.NewTool("story_add",
			mcpgo.WithDescription("Mint a new story row in the named project. Returns the freshly-persisted story.Story. Defaults: priority=medium, category=feature. project_id and title are required; everything else is optional."),
			mcpgo.WithString("project_id", mcpgo.Required(), mcpgo.Description("Owning project id (proj_<8hex>).")),
			mcpgo.WithString("title", mcpgo.Required(), mcpgo.Description("Story title.")),
			mcpgo.WithString("description", mcpgo.Description("Story body / why this work matters.")),
			mcpgo.WithString("acceptance_criteria", mcpgo.Description("Per-AC pass/fail conditions.")),
			mcpgo.WithString("priority", mcpgo.Description("critical | high | medium | low (default medium).")),
			mcpgo.WithString("category", mcpgo.Description("feature | bug | improvement | infrastructure | documentation (default feature).")),
			mcpgo.WithArray("tags", mcpgo.Description("Tags/labels."),
				mcpgo.Items(map[string]any{"type": "string"})),
		)
		s.mcp.AddTool(addStoryTool, s.handleStoryAdd)

		updateStoryTool := mcpgo.NewTool("story_update",
			mcpgo.WithDescription("Update a story. Pass only the keys you want to change; omitted keys are left untouched. Tags replace wholesale — pass an empty array to clear. Status moves the story through the lifecycle (ready | in_progress | done | cancelled); the substrate's pr_story_terminal_gate rejects done/cancelled with open work tasks. Fields writes template-declared values (e.g. repro, fix_commit, root_cause); unknown field names are rejected against the category template. Sty_4db0e025 D1 folded story_update_status + story_field_set into this single verb."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id (sty_<8hex>).")),
			mcpgo.WithString("title", mcpgo.Description("New title.")),
			mcpgo.WithString("description", mcpgo.Description("New description.")),
			mcpgo.WithString("acceptance_criteria", mcpgo.Description("New acceptance criteria.")),
			mcpgo.WithString("category", mcpgo.Description("feature | bug | improvement | infrastructure | documentation")),
			mcpgo.WithString("priority", mcpgo.Description("critical | high | medium | low")),
			mcpgo.WithArray("tags", mcpgo.Description("Tags/labels (replaces existing tags). Empty array clears."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithString("status", mcpgo.Description("Target status: ready | in_progress | done | cancelled. Transitions are validated; pr_story_terminal_gate blocks done/cancelled while open tasks remain.")),
			mcpgo.WithObject("fields", mcpgo.Description("Template-declared field writes as {name: value}. Empty string clears the field. Unknown field names are rejected against the resolved category template.")),
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

		closeStoryTool := mcpgo.NewTool("story_close",
			mcpgo.WithDescription("Mechanical close: gate-checks the story's task chain + story_review verdict + template fields; on PASS appends a kind:close-evidence ledger row and walks the story to status=done via UpdateStatusDerived; on FAIL returns {status:\"fail\", gaps:[…]} without mutation. The reasoning surface lives in the upstream contract:story_review task; this verb is structural only (no LLM, same tier as pr_story_terminal_gate)."),
			mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story id (sty_<8hex>).")),
			mcpgo.WithString("resolution_code", mcpgo.Description("Resolution slot for the close-evidence row. Default 'delivered'. Allowed: delivered | plan_only | not_required | duplicate | superseded | failed:complexity | failed:scope_invalid | failed:blocked.")),
		)
		s.mcp.AddTool(closeStoryTool, s.handleStoryClose)

		// story_update_status + story_field_set MCP registrations removed
		// in sty_4db0e025 slice D1 — both folded into story_update above
		// (status + fields arguments). pr_story_terminal_gate is preserved:
		// status transitions still route through MemoryStore.UpdateStatus
		// via client.StoryUpdate.
		//
		// story_template_get / story_template_list MCP registrations removed
		// in sty_4db0e025 slice C1+B2 — operator authoring per sty_3dc39a5c
		// "Removed from MCP" list. Reachable through /api/v1 + the
		// satellites-client CLI only. handleStoryTemplateGet /
		// handleStoryTemplateList remain so the typed methods on
		// *client.Client continue to back the HTTP routes and CLI verbs.
	}

	if s.deps.Stories != nil && s.deps.Ledger != nil && s.deps.Documents != nil && s.deps.Projects != nil {
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

		// agent_compose, agent_ephemeral_summary, agent_apikey_* MCP
		// registrations removed in sty_4db0e025 slice B1 — these verbs
		// are now reachable through /api/v1 + the satellites-client CLI
		// only. Handler bodies remain (handleAgentCompose,
		// handleAgentEphemeralSummary, handleAgentAPIKey* in
		// agent_compose.go / agent_apikey_handlers.go) so the typed
		// methods on *client.Client continue to back the HTTP routes
		// and CLI verbs.

		if s.deps.Sessions != nil {
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

			// session_whoami and session_register MCP registrations removed
			// in sty_4db0e025 slice B3 — session_register is auto-applied
			// via the Mcp-Session-Id header path (story_31975268) and
			// session_whoami overlaps satellites_info. Both reach /api/v1 +
			// satellites-client CLI; handler bodies remain in
			// claim_handlers.go so the typed methods on *client.Client
			// continue to back the HTTP routes (session/whoami,
			// session/register) and CLI verbs.
		}
	}

	if s.deps.Workspaces != nil {
		addWsTool := mcpgo.NewTool("workspace_add",
			mcpgo.WithDescription("Add a new workspace and record the caller as admin. The caller must be authenticated."),
			mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Workspace display name.")),
		)
		s.mcp.AddTool(addWsTool, s.handleWorkspaceAdd)

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

	// system_seed_run MCP registration removed in sty_4db0e025 slice B3 —
	// admin-only verb, now reachable through /api/v1 + the
	// satellites-client CLI only. handleSystemSeedRun
	// (system_seed_handler.go) remains so the typed method on
	// *client.Client continues to back the HTTP route and CLI verb.

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

	if s.deps.Tasks != nil {
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

		// story_export_walk MCP registration removed in sty_4db0e025 slice
		// C1+B2 — operator authoring per sty_3dc39a5c "Removed from MCP"
		// list. Reachable through /api/v1 + the satellites-client CLI only.
		// handleStoryExportWalk remains so the typed method on
		// *client.Client continues to back the HTTP route and CLI verb.

		// sty_8c17b89d: task_log_append + task_log_list MCP verbs (SSE
		// stream is HTTP-only by design; portal subscribes via
		// EventSource). Gated on TaskLogs being wired so test fixtures
		// that don't construct a store don't register the tools.
		if s.deps.TaskLogs != nil {
			taskLogAppendTool := mcpgo.NewTool("task_log_append",
				mcpgo.WithDescription("Append one task_log row (lifecycle event or stdout/stderr chunk) to the substrate. Producer-supplied seq governs replay order; the SSE endpoint /api/v1/task/log/stream fans rows out by (task_id, seq). Sty_8c17b89d."),
				mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id the row belongs to.")),
				mcpgo.WithString("workspace_id", mcpgo.Description("Workspace scope. Defaults to the task's workspace when omitted.")),
				mcpgo.WithString("project_id", mcpgo.Description("Project scope. Defaults to the task's project.")),
				mcpgo.WithNumber("seq", mcpgo.Required(), mcpgo.Description("Producer-supplied monotonic sequence number.")),
				mcpgo.WithString("ts", mcpgo.Description("RFC3339Nano timestamp. Defaults to now.")),
				mcpgo.WithString("kind", mcpgo.Required(), mcpgo.Description("One of start | heartbeat | stdout | stderr | stop.")),
				mcpgo.WithString("payload", mcpgo.Description("Opaque JSON payload, one shape per kind.")),
			)
			s.mcp.AddTool(taskLogAppendTool, s.handleTaskLogAppend)

			taskLogListTool := mcpgo.NewTool("task_log_list",
				mcpgo.WithDescription("List task_log rows for a task ordered by seq ASC. Use from_seq for replay-after-cursor (matches the SSE Last-Event-ID resume shape). Sty_8c17b89d."),
				mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id whose rows to list.")),
				mcpgo.WithNumber("from_seq", mcpgo.Description("Inclusive lower bound on seq. Defaults to 0.")),
				mcpgo.WithNumber("limit", mcpgo.Description("Max rows to return. Defaults to unbounded.")),
			)
			s.mcp.AddTool(taskLogListTool, s.handleTaskLogList)
		}
	}

	if s.deps.Repos != nil {
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
	if s.deps.Changelog != nil {
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
		// Canonical-only fallback vocab so the verb is callable for
		// built-in action types even before main.go runs
		// LoadReplicateVocabularyFromDoc to install the configseed-
		// loaded alias map.
		s.deps.ReplicateVocab = s.cli().NewReplicateVocabDefault()
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

// Client returns the typed *client.Client snapshot for callers that
// need to consume the client surface directly (main.go boot wiring,
// /api/v1 vocab handoff). Sty_4db0e025 slice A11: the former
// portal_replicate vocab/runner accessor methods (whose signatures
// pulled internal/portalreplicate into mcp.go) are replaced by a
// single Client() accessor — main.go reaches the same getters via
// *client.Client.ReplicateVocab / ReplicateRunner.
func (s *Server) Client() *client.Client {
	return s.cli()
}

// LoadReplicateVocabularyFromDoc reads the configured
// replicate_vocabulary document via the typed client and stores the
// result on the Server's deps. Forwards to
// *client.Client.LoadReplicateVocabularyFromDoc; failures fall back to
// the canonical-only vocabulary so the tool stays callable with
// built-in action names. Called from main.go after configseed.RunAll
// completes.
func (s *Server) LoadReplicateVocabularyFromDoc(ctx context.Context, name string) error {
	v, err := s.cli().LoadReplicateVocabularyFromDoc(ctx, name)
	s.deps.ReplicateVocab = v
	return err
}

// requireReplicatePrereqs returns nil when the runner has the
// dependencies it needs (stories + ledger). Used by Server.New to
// gate tool registration. Sty_088f6d5c.
func (s *Server) requireReplicatePrereqs() error {
	if s.deps.Stories == nil {
		return errors.New("portal_replicate: stories store unavailable")
	}
	if s.deps.Ledger == nil {
		return errors.New("portal_replicate: ledger store unavailable")
	}
	return nil
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

// handleInfo is the satellites_info tool adapter. Thin forwarder per
// cli-primary order:07a-layer-2 (sty_df1cb227) + sty_f3f7bf9b slice 11:
// resolve caller → call typed surface → marshal wire-shape payload.
// Per-call logging is handled by the audit middleware; the audit row
// covers the same fields without burdening each adapter.
func (s *Server) handleInfo(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().SatellitesInfo(ctx, toClientCaller(caller), client.SatellitesInfoInput{})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"version":    out.Version,
		"build":      out.Build,
		"commit":     out.Commit,
		"user_email": out.UserEmail,
		"started_at": out.StartedAt.Format(time.RFC3339),
	})
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

// resolveProjectID is a thin wire-side adapter around
// client.Client.ResolveProjectID. It supplies the URL-scoped
// project_id from the inbound request context (via the
// mcpserver-private scopedProjectKey), which is the only piece of
// state the typed method does not own. The body of the resolution
// rules lives in internal/client/resolve.go (sty_068a6c46 migration).
func (s *Server) resolveProjectID(ctx context.Context, requested string, caller auth.CallerIdentity, memberships []string) (string, error) {
	return s.cli().ResolveProjectID(ctx, requested, ScopedProjectIDFrom(ctx), toClientCaller(caller), memberships)
}

// ensureCallerWorkspaces is a thin adapter around
// client.Client.EnsureCallerWorkspaces. Kept on Server to preserve
// the wire-layer call-site signatures; the body of the helper now
// lives in internal/client/resolve.go (sty_068a6c46 migration).
func (s *Server) ensureCallerWorkspaces(ctx context.Context, caller auth.CallerIdentity) []string {
	return s.cli().EnsureCallerWorkspaces(ctx, toClientCaller(caller))
}

// resolveCallerWorkspaceID is a thin adapter around
// client.Client.ResolveCallerWorkspaceID. Kept on Server to preserve
// the wire-layer call-site signatures; the body now lives in
// internal/client/resolve.go (sty_068a6c46 migration).
func (s *Server) resolveCallerWorkspaceID(ctx context.Context, caller auth.CallerIdentity) string {
	return s.cli().ResolveCallerWorkspaceID(ctx, toClientCaller(caller))
}

// resolveCallerMemberships is a thin adapter around
// client.Client.ResolveCallerMemberships. Kept on Server to preserve
// the wire-layer call-site signatures; the body now lives in
// internal/client/resolve.go (sty_068a6c46 migration).
func (s *Server) resolveCallerMemberships(ctx context.Context, caller auth.CallerIdentity) []string {
	return s.cli().ResolveCallerMemberships(ctx, toClientCaller(caller))
}

// ledgerWorkspaceInMemberships is a thin adapter around
// client.WorkspaceInMemberships. Kept at package scope to preserve the
// wire-layer call-site signatures; the body now lives in
// internal/client/resolve.go (sty_068a6c46 migration).
func ledgerWorkspaceInMemberships(wsID string, memberships []string) bool {
	return client.WorkspaceInMemberships(wsID, memberships)
}

// resolveProjectWorkspaceID is a thin adapter around
// client.Client.ResolveProjectWorkspaceID. Kept on Server to preserve
// the wire-layer call-site signatures; the body now lives in
// internal/client/resolve.go (sty_068a6c46 migration).
func (s *Server) resolveProjectWorkspaceID(ctx context.Context, projectID string) string {
	return s.cli().ResolveProjectWorkspaceID(ctx, projectID)
}

func (s *Server) handleDocumentIngestFile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	path, err := req.RequireString("path")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	if wsID == "" {
		wsID = s.resolveCallerWorkspaceID(ctx, caller)
	}
	out, err := s.cli().DocumentIngestFile(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.DocumentIngestFileInput{
		Path: path, WorkspaceID: wsID, ResolvedProjectID: resolvedID,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "document_ingest_file").Str("project_id", resolvedID).Str("name", out.Name).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
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

func (s *Server) handleDocumentAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	in, err := s.buildDocumentAddInput(ctx, caller, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	created, err := s.cli().DocumentAdd(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: in.Memberships}, in.Payload)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(created)
	s.logger.Info().Str("method", "tools/call").Str("tool", "document_add").Str("doc_id", created.ID).Str("type", created.Type).Str("scope", created.Scope).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// documentAddRequest pairs the resolved memberships with the
// payload the typed method consumes; lets handleDocumentAdd stay
// inside the ≤25-line adapter budget.
type documentAddRequest struct {
	Memberships []string
	Payload     client.DocumentAddInput
}

// buildDocumentAddInput owns the wire-side argument parsing +
// per-scope project/workspace resolution that handleDocumentAdd
// previously inlined. The typed client method consumes the resolved
// payload verbatim.
func (s *Server) buildDocumentAddInput(ctx context.Context, caller auth.CallerIdentity, req mcpgo.CallToolRequest) (documentAddRequest, error) {
	docType, err := req.RequireString("type")
	if err != nil {
		return documentAddRequest{}, err
	}
	scope, err := req.RequireString("scope")
	if err != nil {
		return documentAddRequest{}, err
	}
	name, err := req.RequireString("name")
	if err != nil {
		return documentAddRequest{}, err
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	wsID := s.resolveCallerWorkspaceID(ctx, caller)
	var resolvedID string
	// Scope strings mirror internal/document/document.go constants
	// (ScopeProject="project", ScopeSystem="system"). Kept as literals
	// here so the transport file stays clean of internal/document.
	switch scope {
	case "project":
		resolvedID, err = s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
		if err != nil {
			return documentAddRequest{}, err
		}
		if cascade := s.resolveProjectWorkspaceID(ctx, resolvedID); cascade != "" {
			wsID = cascade
		}
	case "system":
		resolvedID = req.GetString("project_id", "")
	}
	return documentAddRequest{
		Memberships: memberships,
		Payload: client.DocumentAddInput{
			Type: docType, Scope: scope, Name: name,
			Body: req.GetString("body", ""), Tags: req.GetStringSlice("tags", nil),
			Status: req.GetString("status", ""), ContractBinding: req.GetString("contract_binding", ""),
			Structured: req.GetString("structured", ""), WorkspaceID: wsID, ResolvedProjectID: resolvedID,
		},
	}, nil
}

func (s *Server) handleDocumentUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	args := req.GetArguments()
	memberships := s.resolveCallerMemberships(ctx, caller)
	updated, err := s.cli().DocumentUpdateByArgs(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships},
		id, args, req.GetStringSlice("tags", nil), memberships, s.nowUTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().Str("method", "tools/call").Str("tool", "document_update").Str("doc_id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
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
	listArgs := client.DocumentListArgs{
		Type:            req.GetString("type", ""),
		Scope:           req.GetString("scope", ""),
		ContractBinding: req.GetString("contract_binding", ""),
		ProjectID:       resolvedID,
		Tags:            req.GetStringSlice("tags", nil),
		Limit:           int(req.GetFloat("limit", 0)),
		WorkspaceID:     wsID,
		Memberships:     memberships,
	}
	rows, err := s.cli().DocumentListByArgs(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, listArgs)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_list").
		Str("type", listArgs.Type).
		Str("scope", listArgs.Scope).
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
	in := client.DocumentSearchScopedInput{
		Type: req.GetString("type", ""), Query: req.GetString("query", ""),
		Scope: req.GetString("scope", ""), ProjectID: req.GetString("project_id", ""),
		ContractBinding: req.GetString("contract_binding", ""), Tags: req.GetStringSlice("tags", nil),
		TopK: int(req.GetFloat("top_k", 0)), Memberships: memberships,
	}
	rows, err := s.cli().DocumentSearchScoped(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "document_search").Str("query", in.Query).Str("type", in.Type).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	modeArg := req.GetString("mode", "")
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().DocumentDeleteByArgs(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, id, modeArg, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "document_delete").Str("doc_id", id).Str("mode", out.Mode).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// resolveBaseURL composes the four-step resolution chain (V3 parity)
// the project view composer threads into client.BuildProjectView:
//
//  1. Inbound request's base URL stashed by ServeHTTP — the host the
//     caller is already connected to.
//  2. cfg.PublicURL — admin override for deployments where the
//     external host differs from the one the request came in on.
//  3. cfg.OAuthRedirectBaseURL — back-compat for setups that pre-date
//     the PublicURL field.
//
// The persisted project MCPURL (step 1 in V3 parity) is consulted
// inside client.BuildProjectView itself.
func (s *Server) resolveBaseURL(ctx context.Context) string {
	base := requestBaseURLFrom(ctx)
	if base == "" {
		base = s.cfg.PublicURL
	}
	if base == "" {
		base = s.cfg.OAuthRedirectBaseURL
	}
	return base
}

func (s *Server) handleProjectAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	view, p, err := s.cli().ProjectAddView(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email}, client.ProjectAddInput{
		Name: name, WorkspaceID: s.resolveCallerWorkspaceID(ctx, caller), Now: s.nowUTC(),
	}, s.resolveBaseURL(ctx))
	if err != nil {
		return mcpgo.NewToolResultError(projectErrMessage(err)), nil
	}
	body, _ := json.Marshal(view)
	s.logger.Info().Str("method", "tools/call").Str("tool", "project_add").
		Str("project_id", p.ID).Str("owner_user_id", p.OwnerUserID).
		Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
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
	out, err := s.cli().ProjectGetView(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.ProjectGetViewInput{
		ID:          id,
		Memberships: memberships,
		BaseURL:     s.resolveBaseURL(ctx),
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"project":     out.Project,
		"intent_body": out.IntentBody,
		"principles":  out.Principles,
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
//
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
	view := s.cli().BuildProjectView(out.ResolvedProject, s.resolveBaseURL(ctx))
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
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if _, ok := enforceScopedProject(ctx, id); !ok {
		return mcpgo.NewToolResultError("project id does not match the URL-scoped project_id"), nil
	}
	in := client.ProjectUpdateInput{ID: id, Name: req.GetString("name", ""),
		Memberships: s.resolveCallerMemberships(ctx, caller), Now: s.nowUTC()}
	if mcpURL, ok := req.GetArguments()["mcp_url"]; ok {
		mcpStr, _ := mcpURL.(string)
		in.MCPURL = &mcpStr
	}
	view, _, err := s.cli().ProjectUpdateView(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email}, in, s.resolveBaseURL(ctx))
	if err != nil {
		return mcpgo.NewToolResultError(projectErrMessage(err)), nil
	}
	body, _ := json.Marshal(view)
	s.logger.Info().Str("method", "tools/call").Str("tool", "project_update").
		Str("project_id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleProjectDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if _, ok := enforceScopedProject(ctx, id); !ok {
		return mcpgo.NewToolResultError("project id does not match the URL-scoped project_id"), nil
	}
	updated, err := s.cli().ProjectDelete(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email}, client.ProjectDeleteInput{
		ID: id, Memberships: s.resolveCallerMemberships(ctx, caller), Now: s.nowUTC(),
	})
	if err != nil {
		return mcpgo.NewToolResultError(projectErrMessage(err)), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().Str("method", "tools/call").Str("tool", "project_delete").
		Str("project_id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// projectErrMessage maps typed sentinels from internal/client into the
// wire-error envelopes the project_add/update/delete handlers
// historically produced. Centralises the mapping so each adapter stays
// inside the ≤25-line floor mandated by sty_f3f7bf9b's review-criteria.
func projectErrMessage(err error) string {
	switch {
	case errors.Is(err, client.ErrNoCallerIdentity):
		return "no caller identity"
	case errors.Is(err, client.ErrProjectNotFound):
		return "project not found"
	default:
		return err.Error()
	}
}

func (s *Server) handleProjectList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	if caller.UserID == "" {
		return mcpgo.NewToolResultError("no caller identity"), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	body, count, err := s.cli().ProjectListJSON(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_list").
		Str("owner_user_id", caller.UserID).
		Int("count", count).
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
	args := buildLedgerListArgs(req)
	query := req.GetString("query", "")
	searchArgs := client.LedgerSearchArgs{
		ResolvedProjectID:   resolvedID,
		Memberships:         memberships,
		Query:               query,
		TopK:                int(req.GetFloat("top_k", 0)),
		Type:                args.Type,
		StoryID:             args.StoryID,
		Tags:                args.Tags,
		Durability:          args.Durability,
		SourceType:          args.SourceType,
		Status:              args.Status,
		IncludeDereferenced: args.IncludeDereferenced,
		Limit:               args.Limit,
		Sensitive:           args.Sensitive,
	}
	rows, err := s.cli().LedgerSearchByArgs(ctx, client.Caller{UserID: caller.UserID, Memberships: memberships}, searchArgs)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "ledger_search").Str("project_id", resolvedID).Str("query", query).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
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

// buildLedgerListArgs translates a CallToolRequest into the flat-field
// LedgerListArgs the typed client surface consumes. Shared by
// handleLedgerList and handleLedgerSearch so the filter surface is
// identical. Sty_4db0e025 slice A11: returns client.LedgerListArgs so
// the transport file does not reference internal/ledger types.
func buildLedgerListArgs(req mcpgo.CallToolRequest) client.LedgerListArgs {
	out := client.LedgerListArgs{
		Type:                req.GetString("type", ""),
		StoryID:             req.GetString("story_id", ""),
		Tags:                req.GetStringSlice("tags", nil),
		Durability:          req.GetString("durability", ""),
		SourceType:          req.GetString("source_type", ""),
		Status:              req.GetString("status", ""),
		IncludeDereferenced: req.GetBool("include_dereferenced", false),
		Limit:               int(req.GetFloat("limit", 0)),
	}
	args := req.GetArguments()
	if v, ok := args["sensitive"]; ok {
		if b, ok := v.(bool); ok {
			out.Sensitive = &b
		}
	}
	return out
}

// handleStoryAdd is the thin wire adapter for story_add. Resolves
// the caller's project + workspace scope, delegates to
// client.StoryAdd, marshals the freshly-persisted row unchanged.
func (s *Server) handleStoryAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	st, err := s.cli().StoryAdd(ctx, toClientCaller(caller), client.StoryAddInput{
		ProjectID: resolvedID, WorkspaceID: s.resolveProjectWorkspaceID(ctx, resolvedID),
		Title: req.GetString("title", ""), Description: req.GetString("description", ""),
		AcceptanceCriteria: req.GetString("acceptance_criteria", ""),
		Priority:           req.GetString("priority", ""), Category: req.GetString("category", ""),
		Tags: req.GetStringSlice("tags", nil), Now: s.nowUTC(),
	})
	if err != nil {
		return mcpgo.NewToolResultError(storyErrMessage(err)), nil
	}
	body, _ := json.Marshal(st)
	s.logger.Info().Str("method", "tools/call").Str("tool", "story_add").
		Str("project_id", resolvedID).Str("story_id", st.ID).
		Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleStoryClose is the thin wire adapter for story_close. The verb
// is structural only: the substrate-side StoryClose method gate-checks
// the chain + review verdict + template fields and on PASS walks the
// story to done via UpdateStatusDerived. Mirrors handleStoryAdd shape
// per pr_mcp_cli_shared_path.
func (s *Server) handleStoryClose(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().StoryClose(ctx, toClientCaller(caller), client.StoryCloseInput{
		StoryID:        id,
		ResolutionCode: req.GetString("resolution_code", ""),
		Memberships:    memberships,
		Now:            s.nowUTC(),
	})
	if err != nil {
		return mcpgo.NewToolResultError(storyCloseErrMessage(err)), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "story_close").
		Str("story_id", id).Str("status", out.Status).
		Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// storyCloseErrMessage maps the story_close sentinels onto the wire
// envelope shape mirroring storyErrMessage.
func storyCloseErrMessage(err error) string {
	switch {
	case errors.Is(err, client.ErrStoryCloseStoryIDRequired):
		return "story_id is required"
	case errors.Is(err, client.ErrStoryCloseStoryNotFound):
		return "story not found"
	default:
		return err.Error()
	}
}

// handleStoryUpdate applies the consolidated story_update verb. The
// per-call surface accepts title, description, acceptance_criteria,
// category, priority, tags, status, and fields. Omitted keys are left
// untouched; tags replace wholesale — an empty array clears the list.
// Sty_4db0e025 slice D1 folded story_update_status + story_field_set
// into this verb; pr_story_terminal_gate is preserved by routing the
// status transition through client.StoryUpdate → MemoryStore.UpdateStatus.
func (s *Server) handleStoryUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	in := buildStoryUpdateInput(req, id, memberships, s.nowUTC())
	updated, err := s.cli().StoryUpdate(ctx, toClientCaller(caller), in)
	if err != nil {
		return mcpgo.NewToolResultError(storyErrMessage(err)), nil
	}
	body, _ := json.Marshal(updated)
	s.logger.Info().Str("method", "tools/call").Str("tool", "story_update").
		Str("story_id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// buildStoryUpdateInput reads optional update fields from req using
// argument-map presence semantics (a key present with an empty value
// clears the field; an omitted key leaves it untouched). The category
// validation lives on client.StoryUpdate so the typed surface owns the
// allowed-set rule. Sty_4db0e025 slice D1 extended this with status +
// fields after folding story_update_status and story_field_set into
// the same verb.
func buildStoryUpdateInput(req mcpgo.CallToolRequest, id string, memberships []string, now time.Time) client.StoryUpdateInput {
	args := req.GetArguments()
	in := client.StoryUpdateInput{ID: id, Memberships: memberships, Now: now}
	if _, ok := args["title"]; ok {
		v := req.GetString("title", "")
		in.Title = &v
	}
	if _, ok := args["description"]; ok {
		v := req.GetString("description", "")
		in.Description = &v
	}
	if _, ok := args["acceptance_criteria"]; ok {
		v := req.GetString("acceptance_criteria", "")
		in.AcceptanceCriteria = &v
	}
	if _, ok := args["category"]; ok {
		v := req.GetString("category", "")
		in.Category = &v
	}
	if _, ok := args["priority"]; ok {
		v := req.GetString("priority", "")
		in.Priority = &v
	}
	if _, ok := args["tags"]; ok {
		v := req.GetStringSlice("tags", []string{})
		if v == nil {
			v = []string{}
		}
		in.Tags = &v
	}
	if _, ok := args["status"]; ok {
		v := req.GetString("status", "")
		in.Status = &v
	}
	if raw, ok := args["fields"]; ok {
		if m, ok := raw.(map[string]any); ok && len(m) > 0 {
			in.Fields = make(map[string]*string, len(m))
			for k, v := range m {
				switch val := v.(type) {
				case string:
					s := val
					in.Fields[k] = &s
				case nil:
					empty := ""
					in.Fields[k] = &empty
				default:
					s := fmt.Sprintf("%v", val)
					in.Fields[k] = &s
				}
			}
		}
	}
	return in
}

// storyErrMessage maps typed sentinels from internal/client into the
// wire-error envelopes the story_add/update handlers historically
// produced. Mirrors the projectErrMessage pattern (slice 1) so each
// adapter stays inside the ≤25-line floor.
func storyErrMessage(err error) string {
	switch {
	case errors.Is(err, client.ErrStoryProjectIDRequired):
		return "project_id is required"
	case errors.Is(err, client.ErrStoryTitleRequired):
		return "title is required"
	case errors.Is(err, client.ErrStoryIDRequired):
		return "id is required"
	default:
		return err.Error()
	}
}

// (loadStoryTemplate removed sty_4db0e025 slice A11 — callers route
// through *client.Client.StoryTemplateGet which holds the same lookup
// logic behind a typed surface.)

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
	list, err := s.cli().StoryList(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.StoryListInput{
		ProjectID:   resolvedID,
		Status:      req.GetString("status", ""),
		Priority:    req.GetString("priority", ""),
		Tag:         req.GetString("tag", ""),
		Limit:       int(req.GetFloat("limit", 0)),
		Memberships: memberships,
	})
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

// handleStoryUpdateStatus + handleStoryFieldSet were removed in
// sty_4db0e025 slice D1. Their behaviour is folded into handleStoryUpdate
// (status + fields arguments) and the consolidated client.StoryUpdate
// method routes status transitions through MemoryStore.UpdateStatus,
// preserving pr_story_terminal_gate.

// handleStoryTemplateGet returns the parsed Template for the given
// category. Convenience over document_get with name=category +
// type=story_template filter. Sty_d2a03cea.
func (s *Server) handleStoryTemplateGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	category, err := req.RequireString("category")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	t, err := s.cli().StoryTemplateGet(ctx, toClientCaller(caller), client.StoryTemplateGetInput{Category: category})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
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
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().StoryTemplateList(ctx, toClientCaller(caller))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
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

// handleWorkspaceAdd parses the request, calls the typed
// WorkspaceAdd surface, and marshals the workspace row verbatim.
func (s *Server) handleWorkspaceAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	w, err := s.cli().WorkspaceAdd(ctx, toClientCaller(caller), client.WorkspaceAddInput{Name: name, Now: s.nowUTC()})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(w)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_add").Str("workspace_id", w.ID).Str("owner_user_id", w.OwnerUserID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleWorkspaceGet delegates to the typed WorkspaceGet surface.
func (s *Server) handleWorkspaceGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	w, err := s.cli().WorkspaceGet(ctx, toClientCaller(caller), client.WorkspaceGetInput{ID: id})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(w)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_get").Str("workspace_id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleWorkspaceList delegates to the typed WorkspaceList surface.
func (s *Server) handleWorkspaceList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	list, err := s.cli().WorkspaceList(ctx, toClientCaller(caller))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(list)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_list").Str("user_id", caller.UserID).Int("count", len(list)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// (classifyLedgerEvent removed sty_4db0e025 slice A11 — was dead code
// referencing internal/ledger constants from the transport layer.)

// handleWorkspaceMemberAdd parses the request, calls the typed
// WorkspaceMemberAdd surface, and marshals the wire envelope.
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
	out, err := s.cli().WorkspaceMemberAdd(ctx, toClientCaller(caller), client.WorkspaceMemberAddInput{WorkspaceID: workspaceID, UserID: userID, Role: role, Now: s.nowUTC()})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_member_add").Str("workspace_id", workspaceID).Str("user_id", userID).Str("role", role).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleWorkspaceMemberList delegates to the typed WorkspaceMemberList
// surface.
func (s *Server) handleWorkspaceMemberList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	workspaceID, err := req.RequireString("workspace_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	members, err := s.cli().WorkspaceMemberList(ctx, toClientCaller(caller), client.WorkspaceMemberListInput{WorkspaceID: workspaceID})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(members)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_member_list").Str("workspace_id", workspaceID).Int("count", len(members)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleWorkspaceMemberUpdateRole parses the request, calls the typed
// WorkspaceMemberUpdateRole surface, and marshals the wire envelope.
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
	out, err := s.cli().WorkspaceMemberUpdateRole(ctx, toClientCaller(caller), client.WorkspaceMemberUpdateRoleInput{WorkspaceID: workspaceID, UserID: userID, Role: newRole, Now: s.nowUTC()})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_member_update_role").Str("workspace_id", workspaceID).Str("user_id", userID).Str("role", newRole).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleWorkspaceMemberRemove parses the request, calls the typed
// WorkspaceMemberRemove surface, and marshals the wire envelope.
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
	out, err := s.cli().WorkspaceMemberRemove(ctx, toClientCaller(caller), client.WorkspaceMemberRemoveInput{WorkspaceID: workspaceID, UserID: userID, Now: s.nowUTC()})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "workspace_member_remove").Str("workspace_id", workspaceID).Str("user_id", userID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
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
	args := buildLedgerListArgs(req)
	args.ResolvedProjectID = resolvedID
	args.Memberships = memberships
	entries, err := s.cli().LedgerListByArgs(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, args)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(entries)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "ledger_list").
		Str("project_id", resolvedID).
		Str("type_filter", args.Type).
		Int("count", len(entries)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

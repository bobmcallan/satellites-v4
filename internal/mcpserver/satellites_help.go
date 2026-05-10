// Package mcpserver — `satellites_help` MCP verb.
//
// `satellites_help` returns a structured JSON catalogue of the
// `bin/satellites-client` CLI surface (noun groups + verbs +
// flags). IDE agents (Claude Code, Cursor) call this verb once at
// session start to discover what `satellites-client` can do
// without paying the per-MCP-tool token tax.
//
// Per cli-primary order:07c (sty_0881d04b). The catalogue mirrors
// docs/cli-primary-design.md §2; staleness is bounded by a unit
// test asserting the noun-group count matches the count in
// cmd/satellites-client/nouns.go's registration list.
//
// Future: replace the static catalogue with a runtime walk of the
// Cobra command tree once the CLI factors `newRootCmd()` out of
// `package main` into `internal/clitree` (deferred to
// order:07c-followup).
package mcpserver

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// helpVerb is one entry in the catalogue's per-noun verb list.
type helpVerb struct {
	Name  string   `json:"name"`
	Short string   `json:"short"`
	Flags []string `json:"flags,omitempty"`
}

// helpNoun is one noun group with its verbs.
type helpNoun struct {
	Name  string     `json:"name"`
	Short string     `json:"short"`
	Verbs []helpVerb `json:"verbs"`
}

// helpCatalogue is the response payload emitted by satellites_help.
type helpCatalogue struct {
	Binary  string     `json:"binary"`
	Version string     `json:"version,omitempty"`
	Nouns   []helpNoun `json:"nouns"`
	Notes   []string   `json:"notes"`
}

// cliCatalogue is the static mirror of cmd/satellites-client/nouns.go's
// registration. The same noun groups + verb shapes; flag lists drawn
// from reads.go / writes.go. Tests assert the noun count + per-noun
// verb count match the binary's runtime tree (via a tracked-count
// constant — see satellites_help_test.go).
var cliCatalogue = []helpNoun{
	{
		Name:  "story",
		Short: "Stories — units of deliverable work.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a new story."},
			{Name: "get", Short: "Get a story by id.", Flags: []string{"--compact"}},
			{Name: "list", Short: "List stories."},
			{Name: "update", Short: "Update a story."},
			{Name: "update-status", Short: "Transition the story's status.", Flags: []string{"--id", "--status"}},
			{Name: "field-set", Short: "Set a single template-defined field.", Flags: []string{"--id", "--field", "--value", "--stdin"}},
			{Name: "template-get", Short: "Return the parsed template for a category."},
			{Name: "template-list", Short: "List story templates."},
			{Name: "export-walk", Short: "Render the contract walk as paste-ready markdown."},
		},
	},
	{
		Name:  "task",
		Short: "Tasks — the dispatch unit.",
		Verbs: []helpVerb{
			{Name: "add", Short: "Mint a task at status=published.", Flags: []string{"--agent-id", "--prompt", "--story-id", "--action", "--kind", "--priority", "--stdin"}},
			{Name: "get", Short: "Get a task by id."},
			{Name: "list", Short: "List tasks."},
			{Name: "claim", Short: "Atomic claim of the highest-priority queued task.", Flags: []string{"--workspace-id", "--worker-id"}},
			{Name: "update", Short: "Mutate a task's lifecycle state.", Flags: []string{"--id", "--status", "--outcome", "--evidence-ledger-ids"}},
			{Name: "walk", Short: "Return the story's task chain orientation.", Flags: []string{"--story-id"}},
			{Name: "plan", Short: "Mint a task at status=planned.", Flags: []string{"--origin", "--agent-id", "--kind"}},
		},
	},
	{
		Name:  "ledger",
		Short: "Ledger — append-only audit log.",
		Verbs: []helpVerb{
			{Name: "append", Short: "Append an event row.", Flags: []string{"--project-id", "--type", "--content", "--story-id", "--tags", "--stdin"}},
			{Name: "get", Short: "Get a ledger row by id."},
			{Name: "list", Short: "List ledger rows newest-first.", Flags: []string{"--project-id", "--story-id", "--type", "--tags", "--limit"}},
			{Name: "search", Short: "Search ledger rows.", Flags: []string{"--project-id", "--query", "--story-id", "--tags", "--top-k"}},
			{Name: "recall", Short: "Walk the dereferenced-row chain."},
			{Name: "dereference", Short: "Mark a row as dereferenced."},
		},
	},
	{
		Name:  "project",
		Short: "Projects — top-level work surface.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a new project."},
			{Name: "get", Short: "Get the project orientation bundle."},
			{Name: "list", Short: "List the caller's projects."},
			{Name: "update", Short: "Update a project's name / mcp_url."},
			{Name: "delete", Short: "Archive a project."},
			{Name: "set", Short: "Bind the session to the project that owns the git remote.", Flags: []string{"--repo-url"}},
			{Name: "seed-run", Short: "Re-run the project-tier configseed loader."},
		},
	},
	{
		Name:  "workspace",
		Short: "Workspaces — tenancy surface (admin).",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a workspace [admin]."},
			{Name: "get", Short: "Get a workspace."},
			{Name: "list", Short: "List the caller's workspaces."},
			{Name: "member-add", Short: "Add a workspace member [admin]."},
			{Name: "member-list", Short: "List workspace members."},
			{Name: "member-update-role", Short: "Change a member's role [admin]."},
			{Name: "member-remove", Short: "Remove a workspace member [admin]."},
		},
	},
	{
		Name:  "kv",
		Short: "KV — typed key/value store.",
		Verbs: []helpVerb{
			{Name: "get", Short: "Get a key."},
			{Name: "set", Short: "Set a key/value."},
			{Name: "delete", Short: "Delete a key."},
			{Name: "get-resolved", Short: "Resolve a key with skill-template substitution."},
			{Name: "list", Short: "List keys."},
		},
	},
	{
		Name:  "repo",
		Short: "Repos — code-index integration.",
		Verbs: []helpVerb{
			{Name: "add", Short: "Register a git remote on a project."},
			{Name: "get", Short: "Get a repo row."},
			{Name: "list", Short: "List repos."},
			{Name: "search", Short: "Symbol search."},
			{Name: "search-text", Short: "Text search."},
			{Name: "get-symbol-source", Short: "Return a symbol's source."},
			{Name: "get-file", Short: "Return a file's contents."},
			{Name: "get-outline", Short: "Return a file's outline."},
		},
	},
	{
		Name:  "agent",
		Short: "Agents — typed roles.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create an agent doc."},
			{Name: "get", Short: "Get an agent doc by id (preferred) or name.", Flags: []string{"--id", "--name", "--project-id"}},
			{Name: "list", Short: "List agent docs."},
			{Name: "update", Short: "Update an agent doc."},
			{Name: "delete", Short: "Archive an agent doc."},
			{Name: "search", Short: "Search agent docs."},
			{Name: "compose", Short: "Compose an ephemeral agent."},
			{Name: "ephemeral-summary", Short: "Summarise ephemeral agents per project."},
			{Name: "apikey-create", Short: "Create an agent API key [admin]."},
			{Name: "apikey-list", Short: "List agent API keys [admin]."},
			{Name: "apikey-delete", Short: "Delete an agent API key [admin]."},
		},
	},
	{
		Name:  "contract",
		Short: "Contracts — lifecycle phase rubrics.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a contract doc."},
			{Name: "get", Short: "Get a contract doc.", Flags: []string{"--id", "--name", "--project-id"}},
			{Name: "list", Short: "List contracts."},
			{Name: "update", Short: "Update a contract doc."},
			{Name: "delete", Short: "Archive a contract."},
			{Name: "search", Short: "Search contracts."},
		},
	},
	{
		Name:  "principle",
		Short: "Principles — workspace + project policy.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a principle."},
			{Name: "get", Short: "Get a principle."},
			{Name: "list", Short: "List principles.", Flags: []string{"--scope", "--project-id", "--active-only"}},
			{Name: "update", Short: "Update a principle."},
			{Name: "delete", Short: "Archive a principle."},
			{Name: "search", Short: "Search principles."},
		},
	},
	{
		Name:  "document",
		Short: "Documents — generic typed markdown.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a document."},
			{Name: "get", Short: "Get a document."},
			{Name: "list", Short: "List documents."},
			{Name: "update", Short: "Update a document."},
			{Name: "delete", Short: "Archive a document."},
			{Name: "search", Short: "Search documents."},
			{Name: "ingest-file", Short: "Ingest a markdown file as a document."},
		},
	},
	{
		Name:  "reviewer",
		Short: "Reviewers — judgment-shape agent identities.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a reviewer doc."},
			{Name: "get", Short: "Get a reviewer doc."},
			{Name: "list", Short: "List reviewers."},
			{Name: "update", Short: "Update a reviewer doc."},
			{Name: "delete", Short: "Archive a reviewer."},
			{Name: "search", Short: "Search reviewers."},
		},
	},
	{
		Name:  "role",
		Short: "Roles — workspace permission grants.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a role doc."},
			{Name: "get", Short: "Get a role doc."},
			{Name: "list", Short: "List roles."},
			{Name: "update", Short: "Update a role doc."},
			{Name: "delete", Short: "Archive a role."},
			{Name: "search", Short: "Search roles."},
		},
	},
	{
		Name:  "skill",
		Short: "Skills — agent capability bundles.",
		Verbs: []helpVerb{
			{Name: "create", Short: "Create a skill doc."},
			{Name: "get", Short: "Get a skill doc."},
			{Name: "list", Short: "List skills."},
			{Name: "update", Short: "Update a skill doc."},
			{Name: "delete", Short: "Archive a skill."},
			{Name: "search", Short: "Search skills."},
		},
	},
	{
		Name:  "changelog",
		Short: "Changelog — per-binary release entries.",
		Verbs: []helpVerb{
			{Name: "add", Short: "Append a changelog row."},
			{Name: "get", Short: "Get a changelog row."},
			{Name: "list", Short: "List changelog rows."},
			{Name: "update", Short: "Update a changelog row."},
			{Name: "delete", Short: "Delete a changelog row."},
		},
	},
	{
		Name:  "session",
		Short: "Session — registry + identity.",
		Verbs: []helpVerb{
			{Name: "register", Short: "Register / re-register the caller's session."},
			{Name: "whoami", Short: "Return the caller's registered session row.", Flags: []string{"--session-id"}},
		},
	},
	{
		Name:  "system",
		Short: "System — admin-tier verbs.",
		Verbs: []helpVerb{
			{Name: "seed-run", Short: "Re-run the system-tier configseed loader [admin]."},
		},
	},
	{
		Name:  "portal",
		Short: "Portal — replication + UI helpers.",
		Verbs: []helpVerb{
			{Name: "replicate", Short: "Replicate the portal UI."},
		},
	},
}

// CLICatalogueNounCount is the expected noun-group count. Tests
// in cmd/satellites-client compare this constant against the live
// nouns.go registration list to catch drift.
const CLICatalogueNounCount = 18

// handleSatellitesHelp returns the CLI catalogue + persistent flags
// + exit-code map. IDE agents call this once at session boot.
func (s *Server) handleSatellitesHelp(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	cat := helpCatalogue{
		Binary:  "satellites-client",
		Version: "", // populated by cmd/satellites-client at build time; left empty here
		Nouns:   cliCatalogue,
		Notes: []string{
			"Persistent flags (root command): --json, --compact, --quiet, --dry-run, --stdin, --yes, --no-input, --no-cache, --no-color, --select, --server, --token.",
			"Auto-JSON when stdout is not a tty.",
			"Typed exit codes: 0 OK, 2 Usage, 3 NotFound, 4 Auth, 5 Server, 7 RateLimit (1 reserved for runtime panics).",
			"Catalogue mirrors docs/cli-primary-design.md §2 and cmd/satellites-client/nouns.go (verified by test).",
		},
	}
	body, err := json.Marshal(cat)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpgo.NewToolResultText(string(body)), nil
}

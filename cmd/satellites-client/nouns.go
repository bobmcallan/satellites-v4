package main

import (
	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// satellitesClientNouns is the slice of registration functions main.go
// invokes. Each function attaches one noun group + its verb stubs.
// Orders 04+05 fill the stubs in.
var satellitesClientNouns = []func(*cobra.Command){
	registerStoryNoun,
	registerTaskNoun,
	registerLedgerNoun,
	registerProjectNoun,
	registerWorkspaceNoun,
	registerKVNoun,
	registerRepoNoun,
	registerAgentNoun,
	registerContractNoun,
	registerPrincipleNoun,
	registerDocumentNoun,
	registerReviewerNoun,
	registerRoleNoun,
	registerSkillNoun,
	registerChangelogNoun,
	registerSessionNoun,
	registerSystemNoun,
	registerPortalNoun,
}

// nounStub builds a noun group cobra.Command with the given short
// description; verbs are added by the caller.
func nounStub(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
	}
}

// verbStub builds a leaf cobra.Command whose RunE returns
// cliexit.NotImplemented. Use is the verb word; long names the
// full <noun> <verb> tuple for the not-implemented message.
func verbStub(use, full, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE:  func(cmd *cobra.Command, args []string) error { return cliexit.NotImplemented(full) },
	}
}

func registerStoryNoun(root *cobra.Command) {
	noun := nounStub("story", "Stories — units of deliverable work.")
	noun.AddCommand(
		verbStub("create", "story create", "Create a new story."),
		verbStub("get", "story get", "Get a story by id."),
		verbStub("list", "story list", "List stories."),
		verbStub("update", "story update", "Update a story."),
		verbStub("update-status", "story update-status", "Transition the story's status."),
		verbStub("field-set", "story field-set", "Set a single template-defined field."),
		verbStub("template-get", "story template-get", "Return the parsed template for a category."),
		verbStub("template-list", "story template-list", "List story templates."),
		verbStub("export-walk", "story export-walk", "Render the contract walk as paste-ready markdown."),
	)
	root.AddCommand(noun)
}

func registerTaskNoun(root *cobra.Command) {
	noun := nounStub("task", "Tasks — the dispatch unit.")
	noun.AddCommand(
		verbStub("add", "task add", "Mint a new task at status=published."),
		verbStub("get", "task get", "Get a task by id."),
		verbStub("list", "task list", "List tasks."),
		verbStub("claim", "task claim", "Atomic claim of the highest-priority queued task."),
		verbStub("update", "task update", "Mutate a task's lifecycle state."),
		verbStub("walk", "task walk", "Return the story's task chain orientation."),
		verbStub("plan", "task plan", "Mint a task at status=planned."),
	)
	root.AddCommand(noun)
}

func registerLedgerNoun(root *cobra.Command) {
	noun := nounStub("ledger", "Ledger — append-only audit log.")
	noun.AddCommand(
		verbStub("append", "ledger append", "Append an event row."),
		verbStub("get", "ledger get", "Get a ledger row by id."),
		verbStub("list", "ledger list", "List ledger rows newest-first."),
		verbStub("search", "ledger search", "Search ledger rows."),
		verbStub("recall", "ledger recall", "Walk the dereferenced-row chain."),
		verbStub("dereference", "ledger dereference", "Mark a row as dereferenced."),
	)
	root.AddCommand(noun)
}

func registerProjectNoun(root *cobra.Command) {
	noun := nounStub("project", "Projects — top-level work surface.")
	noun.AddCommand(
		verbStub("create", "project create", "Create a new project."),
		verbStub("get", "project get", "Get the project orientation bundle."),
		verbStub("list", "project list", "List the caller's projects."),
		verbStub("update", "project update", "Update a project's name / mcp_url."),
		verbStub("delete", "project delete", "Archive a project."),
		verbStub("set", "project set", "Bind the session to the project that owns the git remote."),
		verbStub("seed-run", "project seed-run", "Re-run the project-tier configseed loader."),
	)
	root.AddCommand(noun)
}

func registerWorkspaceNoun(root *cobra.Command) {
	noun := nounStub("workspace", "Workspaces — tenancy surface (admin).")
	noun.AddCommand(
		verbStub("create", "workspace create", "Create a workspace [admin]."),
		verbStub("get", "workspace get", "Get a workspace."),
		verbStub("list", "workspace list", "List the caller's workspaces."),
		verbStub("member-add", "workspace member-add", "Add a workspace member [admin]."),
		verbStub("member-list", "workspace member-list", "List workspace members."),
		verbStub("member-update-role", "workspace member-update-role", "Change a member's role [admin]."),
		verbStub("member-remove", "workspace member-remove", "Remove a workspace member [admin]."),
	)
	root.AddCommand(noun)
}

func registerKVNoun(root *cobra.Command) {
	noun := nounStub("kv", "KV — typed key/value store.")
	noun.AddCommand(
		verbStub("get", "kv get", "Get a key."),
		verbStub("set", "kv set", "Set a key/value."),
		verbStub("delete", "kv delete", "Delete a key."),
		verbStub("get-resolved", "kv get-resolved", "Resolve a key with skill-template substitution."),
		verbStub("list", "kv list", "List keys."),
	)
	root.AddCommand(noun)
}

func registerRepoNoun(root *cobra.Command) {
	noun := nounStub("repo", "Repos — code-index integration.")
	noun.AddCommand(
		verbStub("add", "repo add", "Register a git remote on a project."),
		verbStub("get", "repo get", "Get a repo row."),
		verbStub("list", "repo list", "List repos."),
		verbStub("search", "repo search", "Symbol search."),
		verbStub("search-text", "repo search-text", "Text search."),
		verbStub("get-symbol-source", "repo get-symbol-source", "Return a symbol's source."),
		verbStub("get-file", "repo get-file", "Return a file's contents."),
		verbStub("get-outline", "repo get-outline", "Return a file's outline."),
	)
	root.AddCommand(noun)
}

func registerAgentNoun(root *cobra.Command) {
	noun := nounStub("agent", "Agents — typed roles.")
	noun.AddCommand(
		verbStub("create", "agent create", "Create an agent doc."),
		verbStub("get", "agent get", "Get an agent doc."),
		verbStub("list", "agent list", "List agent docs."),
		verbStub("update", "agent update", "Update an agent doc."),
		verbStub("delete", "agent delete", "Archive an agent doc."),
		verbStub("search", "agent search", "Search agent docs."),
		verbStub("compose", "agent compose", "Compose an ephemeral agent."),
		verbStub("ephemeral-summary", "agent ephemeral-summary", "Summarise ephemeral agents per project."),
		verbStub("apikey-create", "agent apikey-create", "Create an agent API key [admin]."),
		verbStub("apikey-list", "agent apikey-list", "List agent API keys [admin]."),
		verbStub("apikey-delete", "agent apikey-delete", "Delete an agent API key [admin]."),
	)
	root.AddCommand(noun)
}

func registerContractNoun(root *cobra.Command) {
	noun := nounStub("contract", "Contracts — lifecycle phase rubrics.")
	noun.AddCommand(
		verbStub("create", "contract create", "Create a contract doc."),
		verbStub("get", "contract get", "Get a contract doc."),
		verbStub("list", "contract list", "List contracts."),
		verbStub("update", "contract update", "Update a contract doc."),
		verbStub("delete", "contract delete", "Archive a contract."),
		verbStub("search", "contract search", "Search contracts."),
	)
	root.AddCommand(noun)
}

func registerPrincipleNoun(root *cobra.Command) {
	noun := nounStub("principle", "Principles — workspace + project policy.")
	noun.AddCommand(
		verbStub("create", "principle create", "Create a principle."),
		verbStub("get", "principle get", "Get a principle."),
		verbStub("list", "principle list", "List principles."),
		verbStub("update", "principle update", "Update a principle."),
		verbStub("delete", "principle delete", "Archive a principle."),
		verbStub("search", "principle search", "Search principles."),
	)
	root.AddCommand(noun)
}

func registerDocumentNoun(root *cobra.Command) {
	noun := nounStub("document", "Documents — generic typed markdown.")
	noun.AddCommand(
		verbStub("create", "document create", "Create a document."),
		verbStub("get", "document get", "Get a document."),
		verbStub("list", "document list", "List documents."),
		verbStub("update", "document update", "Update a document."),
		verbStub("delete", "document delete", "Archive a document."),
		verbStub("search", "document search", "Search documents."),
		verbStub("ingest-file", "document ingest-file", "Ingest a markdown file as a document."),
	)
	root.AddCommand(noun)
}

func registerReviewerNoun(root *cobra.Command) {
	noun := nounStub("reviewer", "Reviewers — judgment-shape agent identities.")
	noun.AddCommand(
		verbStub("create", "reviewer create", "Create a reviewer doc."),
		verbStub("get", "reviewer get", "Get a reviewer doc."),
		verbStub("list", "reviewer list", "List reviewers."),
		verbStub("update", "reviewer update", "Update a reviewer doc."),
		verbStub("delete", "reviewer delete", "Archive a reviewer."),
		verbStub("search", "reviewer search", "Search reviewers."),
	)
	root.AddCommand(noun)
}

func registerRoleNoun(root *cobra.Command) {
	noun := nounStub("role", "Roles — workspace permission grants.")
	noun.AddCommand(
		verbStub("create", "role create", "Create a role doc."),
		verbStub("get", "role get", "Get a role doc."),
		verbStub("list", "role list", "List roles."),
		verbStub("update", "role update", "Update a role doc."),
		verbStub("delete", "role delete", "Archive a role."),
		verbStub("search", "role search", "Search roles."),
	)
	root.AddCommand(noun)
}

func registerSkillNoun(root *cobra.Command) {
	noun := nounStub("skill", "Skills — agent capability bundles.")
	noun.AddCommand(
		verbStub("create", "skill create", "Create a skill doc."),
		verbStub("get", "skill get", "Get a skill doc."),
		verbStub("list", "skill list", "List skills."),
		verbStub("update", "skill update", "Update a skill doc."),
		verbStub("delete", "skill delete", "Archive a skill."),
		verbStub("search", "skill search", "Search skills."),
	)
	root.AddCommand(noun)
}

func registerChangelogNoun(root *cobra.Command) {
	noun := nounStub("changelog", "Changelog — per-binary release entries.")
	noun.AddCommand(
		verbStub("add", "changelog add", "Append a changelog row."),
		verbStub("get", "changelog get", "Get a changelog row."),
		verbStub("list", "changelog list", "List changelog rows."),
		verbStub("update", "changelog update", "Update a changelog row."),
		verbStub("delete", "changelog delete", "Delete a changelog row."),
	)
	root.AddCommand(noun)
}

func registerSessionNoun(root *cobra.Command) {
	noun := nounStub("session", "Session — registry + identity.")
	noun.AddCommand(
		verbStub("register", "session register", "Register / re-register the caller's session."),
		verbStub("whoami", "session whoami", "Return the caller's registered session row."),
	)
	root.AddCommand(noun)
}

func registerSystemNoun(root *cobra.Command) {
	noun := nounStub("system", "System — admin-tier verbs.")
	noun.AddCommand(
		verbStub("seed-run", "system seed-run", "Re-run the system-tier configseed loader [admin]."),
	)
	root.AddCommand(noun)
}

func registerPortalNoun(root *cobra.Command) {
	noun := nounStub("portal", "Portal — replication + UI helpers.")
	noun.AddCommand(
		verbStub("replicate", "portal replicate", "Replicate the portal UI."),
	)
	root.AddCommand(noun)
}

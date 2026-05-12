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
	registerAuthNoun,
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
		newStoryGetCmd(),  // order:04
		newStoryListCmd(), // sty_ef248ab2
		verbStub("update", "story update", "Update a story."),
		newStoryUpdateStatusCmd(),   // order:05
		newStoryFieldSetCmd(),       // order:05
		newStoryTemplateGetCmd(),    // sty_ef248ab2
		newStoryTemplateListCmd(),   // sty_ef248ab2
		newStoryExportWalkCmd(),     // sty_ef248ab2
	)
	root.AddCommand(noun)
}

func registerTaskNoun(root *cobra.Command) {
	noun := nounStub("task", "Tasks — the dispatch unit.")
	noun.AddCommand(
		newTaskAddCmd(),    // order:05
		newTaskGetCmd(),    // order:04
		newTaskListCmd(),   // sty_ef248ab2
		newTaskClaimCmd(),  // order:05
		newTaskRunCmd(),    // sty_3e27a3f5 — orchestrator-invoked dispatch.
		newTaskUpdateCmd(), // order:05
		newTaskWalkCmd(),   // order:04
		newTaskPlanCmd(),   // order:05
	)
	root.AddCommand(noun)
}

func registerLedgerNoun(root *cobra.Command) {
	noun := nounStub("ledger", "Ledger — append-only audit log.")
	noun.AddCommand(
		newLedgerAppendCmd(), // order:05
		newLedgerGetCmd(),    // order:04
		newLedgerListCmd(),   // order:04
		newLedgerSearchCmd(), // order:04
		verbStub("recall", "ledger recall", "Walk the dereferenced-row chain."),
		verbStub("dereference", "ledger dereference", "Mark a row as dereferenced."),
	)
	root.AddCommand(noun)
}

func registerProjectNoun(root *cobra.Command) {
	noun := nounStub("project", "Projects — top-level work surface.")
	noun.AddCommand(
		verbStub("create", "project create", "Create a new project."),
		newProjectGetCmd(),  // sty_ef248ab2
		newProjectListCmd(), // sty_ef248ab2
		verbStub("update", "project update", "Update a project's name / mcp_url."),
		verbStub("delete", "project delete", "Archive a project."),
		newProjectSetCmd(), // order:05
		verbStub("seed-run", "project seed-run", "Re-run the project-tier configseed loader."),
	)
	root.AddCommand(noun)
}

func registerWorkspaceNoun(root *cobra.Command) {
	noun := nounStub("workspace", "Workspaces — tenancy surface (admin).")
	noun.AddCommand(
		verbStub("create", "workspace create", "Create a workspace [admin]."),
		newWorkspaceGetCmd(),  // sty_ef248ab2
		newWorkspaceListCmd(), // sty_ef248ab2
		verbStub("member-add", "workspace member-add", "Add a workspace member [admin]."),
		newWorkspaceMemberListCmd(), // sty_ef248ab2
		verbStub("member-update-role", "workspace member-update-role", "Change a member's role [admin]."),
		verbStub("member-remove", "workspace member-remove", "Remove a workspace member [admin]."),
	)
	root.AddCommand(noun)
}

func registerKVNoun(root *cobra.Command) {
	noun := nounStub("kv", "KV — typed key/value store.")
	noun.AddCommand(
		newKVGetCmd(), // sty_ef248ab2
		verbStub("set", "kv set", "Set a key/value."),
		verbStub("delete", "kv delete", "Delete a key."),
		newKVGetResolvedCmd(), // sty_ef248ab2
		newKVListCmd(),        // sty_ef248ab2
	)
	root.AddCommand(noun)
}

func registerRepoNoun(root *cobra.Command) {
	noun := nounStub("repo", "Repos — code-index integration.")
	noun.AddCommand(
		verbStub("add", "repo add", "Register a git remote on a project."),
		newRepoGetCmd(),             // sty_ef248ab2
		newRepoListCmd(),            // sty_ef248ab2
		newRepoSearchCmd(),          // sty_ef248ab2
		newRepoSearchTextCmd(),      // sty_ef248ab2
		newRepoGetSymbolSourceCmd(), // sty_ef248ab2
		newRepoGetFileCmd(),         // sty_ef248ab2
		newRepoGetOutlineCmd(),      // sty_ef248ab2
	)
	root.AddCommand(noun)
}

func registerAgentNoun(root *cobra.Command) {
	noun := nounStub("agent", "Agents — typed roles.")
	noun.AddCommand(
		verbStub("create", "agent create", "Create an agent doc."),
		newAgentGetCmd(),  // order:04
		newAgentListCmd(), // sty_ef248ab2
		verbStub("update", "agent update", "Update an agent doc."),
		verbStub("delete", "agent delete", "Archive an agent doc."),
		newAgentSearchCmd(), // sty_ef248ab2
		verbStub("compose", "agent compose", "Compose an ephemeral agent."),
		newAgentEphemeralSummaryCmd(), // sty_ef248ab2
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
		newContractGetCmd(),  // order:04
		newContractListCmd(), // sty_ef248ab2
		verbStub("update", "contract update", "Update a contract doc."),
		verbStub("delete", "contract delete", "Archive a contract."),
		newContractSearchCmd(), // sty_ef248ab2
	)
	root.AddCommand(noun)
}

func registerPrincipleNoun(root *cobra.Command) {
	noun := nounStub("principle", "Principles — workspace + project policy.")
	noun.AddCommand(
		verbStub("create", "principle create", "Create a principle."),
		newPrincipleGetCmd(),  // sty_ef248ab2
		newPrincipleListCmd(), // order:04
		verbStub("update", "principle update", "Update a principle."),
		verbStub("delete", "principle delete", "Archive a principle."),
		newPrincipleSearchCmd(), // sty_ef248ab2
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
		newDocumentSearchCmd(), // sty_ef248ab2
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
		newChangelogGetCmd(),  // sty_ef248ab2
		newChangelogListCmd(), // sty_ef248ab2
		verbStub("update", "changelog update", "Update a changelog row."),
		verbStub("delete", "changelog delete", "Delete a changelog row."),
	)
	root.AddCommand(noun)
}

func registerSessionNoun(root *cobra.Command) {
	noun := nounStub("session", "Session — registry + identity.")
	noun.AddCommand(
		verbStub("register", "session register", "Register / re-register the caller's session."),
		newSessionWhoamiCmd(), // order:04
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

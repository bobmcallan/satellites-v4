// reads_operator.go — operator-tier read verb handlers landed in
// sty_ef248ab2 (cli-primary order:04-followup). Each constructor builds
// a cobra leaf bound to a typed-method call.go path on the server.
//
// Pattern matches reads.go: readHandler builds the RunE; argsFn maps
// cobra flags + positional args into the JSON request body.

package main

import (
	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// ----- story -----

func newStoryListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List stories scoped to a project.",
		RunE: readHandler("story_list", func(cmd *cobra.Command, args []string) (any, error) {
			projectID, _ := cmd.Flags().GetString("project-id")
			if projectID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "story list: --project-id is required")
			}
			out := map[string]any{"project_id": projectID}
			if v, _ := cmd.Flags().GetString("status"); v != "" {
				out["status"] = v
			}
			if v, _ := cmd.Flags().GetString("priority"); v != "" {
				out["priority"] = v
			}
			if v, _ := cmd.Flags().GetString("tag"); v != "" {
				out["tag"] = v
			}
			if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
				out["limit"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope (required).")
	c.Flags().String("status", "", "Filter by status.")
	c.Flags().String("priority", "", "Filter by priority.")
	c.Flags().String("tag", "", "Filter by tag.")
	c.Flags().Int("limit", 0, "Max rows.")
	_ = c.MarkFlagRequired("project-id")
	return c
}

func newStoryTemplateGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "template-get",
		Short: "Return the parsed template for a category.",
		RunE: readHandler("story_template_get", func(cmd *cobra.Command, args []string) (any, error) {
			category, _ := cmd.Flags().GetString("category")
			if category == "" {
				return nil, cliexit.Newf(cliexit.Usage, "story template-get: --category is required")
			}
			return map[string]any{"category": category}, nil
		}),
	}
	c.Flags().String("category", "", "Story category (required).")
	_ = c.MarkFlagRequired("category")
	return c
}

func newStoryTemplateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "template-list",
		Short: "List story templates.",
		RunE:  readHandler("story_template_list", func(cmd *cobra.Command, args []string) (any, error) { return map[string]any{}, nil }),
	}
}

func newStoryExportWalkCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "export-walk <story_id>",
		Short: "Render the contract walk as paste-ready markdown.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("story_export_walk", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{"story_id": args[0]}
			if v, _ := cmd.Flags().GetString("format"); v != "" {
				out["format"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("format", "", "Output format (default markdown).")
	return c
}

// ----- task -----

func newTaskListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List tasks matching filters.",
		RunE: readHandler("task_list", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("story-id"); v != "" {
				out["story_id"] = v
			}
			if v, _ := cmd.Flags().GetString("status"); v != "" {
				out["status"] = v
			}
			if v, _ := cmd.Flags().GetString("kind"); v != "" {
				out["kind"] = v
			}
			if v, _ := cmd.Flags().GetString("origin"); v != "" {
				out["origin"] = v
			}
			if v, _ := cmd.Flags().GetString("priority"); v != "" {
				out["priority"] = v
			}
			if v, _ := cmd.Flags().GetString("claimed-by"); v != "" {
				out["claimed_by"] = v
			}
			if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
				out["limit"] = v
			}
			if v, _ := cmd.Flags().GetBool("include-archived"); v {
				out["include_archived"] = true
			}
			return out, nil
		}),
	}
	c.Flags().String("story-id", "", "Filter by story.")
	c.Flags().String("status", "", "Filter by status.")
	c.Flags().String("kind", "", "work | review.")
	c.Flags().String("origin", "", "Filter by origin.")
	c.Flags().String("priority", "", "Filter by priority.")
	c.Flags().String("claimed-by", "", "Filter by claimed_by worker id.")
	c.Flags().Int("limit", 0, "Max rows.")
	c.Flags().Bool("include-archived", false, "Include archived rows.")
	return c
}

// ----- project -----

func newProjectGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <project_id>",
		Short: "Get the project orientation bundle.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("project_get", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

func newProjectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the caller's projects.",
		RunE:  readHandler("project_list", func(cmd *cobra.Command, args []string) (any, error) { return map[string]any{}, nil }),
	}
}

// ----- workspace -----

func newWorkspaceGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <workspace_id>",
		Short: "Get a workspace.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("workspace_get", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

func newWorkspaceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the caller's workspaces.",
		RunE:  readHandler("workspace_list", func(cmd *cobra.Command, args []string) (any, error) { return map[string]any{}, nil }),
	}
}

func newWorkspaceMemberListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "member-list",
		Short: "List workspace members.",
		RunE: readHandler("workspace_member_list", func(cmd *cobra.Command, args []string) (any, error) {
			workspaceID, _ := cmd.Flags().GetString("workspace-id")
			if workspaceID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "workspace member-list: --workspace-id is required")
			}
			return map[string]any{"workspace_id": workspaceID}, nil
		}),
	}
	c.Flags().String("workspace-id", "", "Workspace id (required).")
	_ = c.MarkFlagRequired("workspace-id")
	return c
}

// ----- changelog -----

func newChangelogGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a changelog row.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("changelog_get", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

func newChangelogListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List changelog rows.",
		RunE: readHandler("changelog_list", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("service"); v != "" {
				out["service"] = v
			}
			if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
				out["limit"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope.")
	c.Flags().String("service", "", "Filter by service.")
	c.Flags().Int("limit", 0, "Max rows.")
	return c
}

// ----- document family -----

// documentListArgs builds the shared list-shape args. Per-noun wrappers
// (agent_list, contract_list, etc.) reuse this with type omitted — the
// server-side handler pins type from the route.
func documentListArgs(cmd *cobra.Command, args []string) (any, error) {
	out := map[string]any{}
	if v, _ := cmd.Flags().GetString("scope"); v != "" {
		out["scope"] = v
	}
	if v, _ := cmd.Flags().GetString("project-id"); v != "" {
		out["project_id"] = v
	}
	if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
		out["tags"] = v
	}
	if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
		out["limit"] = v
	}
	return out, nil
}

func documentSearchArgs(cmd *cobra.Command, args []string) (any, error) {
	out := map[string]any{}
	if v, _ := cmd.Flags().GetString("query"); v != "" {
		out["query"] = v
	}
	if v, _ := cmd.Flags().GetString("scope"); v != "" {
		out["scope"] = v
	}
	if v, _ := cmd.Flags().GetString("project-id"); v != "" {
		out["project_id"] = v
	}
	if v, _ := cmd.Flags().GetString("contract-binding"); v != "" {
		out["contract_binding"] = v
	}
	if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
		out["tags"] = v
	}
	if v, _ := cmd.Flags().GetInt("top-k"); v > 0 {
		out["top_k"] = v
	}
	return out, nil
}

func addDocumentListFlags(c *cobra.Command) {
	c.Flags().String("scope", "", "Filter by scope.")
	c.Flags().String("project-id", "", "Filter by project.")
	c.Flags().StringSlice("tags", nil, "Filter by tags.")
	c.Flags().Int("limit", 0, "Max rows.")
}

func addDocumentSearchFlags(c *cobra.Command) {
	c.Flags().String("query", "", "Free-text query.")
	c.Flags().String("scope", "", "Filter by scope.")
	c.Flags().String("project-id", "", "Filter by project.")
	c.Flags().String("contract-binding", "", "Filter by contract_binding.")
	c.Flags().StringSlice("tags", nil, "Filter by tags.")
	c.Flags().Int("top-k", 0, "Max rows.")
}

func newDocumentSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search",
		Short: "Search documents.",
		RunE: readHandler("document_search", func(cmd *cobra.Command, args []string) (any, error) {
			out, err := documentSearchArgs(cmd, args)
			if err != nil {
				return nil, err
			}
			m := out.(map[string]any)
			if v, _ := cmd.Flags().GetString("type"); v != "" {
				m["type"] = v
			}
			return m, nil
		}),
	}
	addDocumentSearchFlags(c)
	c.Flags().String("type", "", "Filter by document type.")
	return c
}

func newAgentListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List agent docs.",
		RunE:  readHandler("agent_list", documentListArgs),
	}
	addDocumentListFlags(c)
	return c
}

func newAgentSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search",
		Short: "Search agent docs.",
		RunE:  readHandler("agent_search", documentSearchArgs),
	}
	addDocumentSearchFlags(c)
	return c
}

func newAgentEphemeralSummaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ephemeral-summary",
		Short: "Summarise ephemeral agents per project.",
		RunE:  readHandler("agent_ephemeral_summary", func(cmd *cobra.Command, args []string) (any, error) { return map[string]any{}, nil }),
	}
}

func newContractListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List contracts.",
		RunE:  readHandler("contract_list", documentListArgs),
	}
	addDocumentListFlags(c)
	return c
}

func newContractSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search",
		Short: "Search contracts.",
		RunE:  readHandler("contract_search", documentSearchArgs),
	}
	addDocumentSearchFlags(c)
	return c
}

func newPrincipleGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get",
		Short: "Get a principle.",
		RunE: readHandler("principle_get", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			name, _ := cmd.Flags().GetString("name")
			if id == "" && name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "principle get: --id or --name is required")
			}
			out := map[string]any{}
			if id != "" {
				out["id"] = id
			}
			if name != "" {
				out["name"] = name
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Document id.")
	c.Flags().String("name", "", "Document name.")
	c.Flags().String("project-id", "", "Project scope for name-keyed lookups.")
	return c
}

func newPrincipleSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search",
		Short: "Search principles.",
		RunE:  readHandler("principle_search", documentSearchArgs),
	}
	addDocumentSearchFlags(c)
	return c
}

// ----- document/ledger/reviewer/role/skill reads (sty_004f3d3a gap-fill) -----

// documentGetArgs builds the shared get-shape args. Per-noun wrappers
// (reviewer_get / role_get / skill_get) pin type at the server-route
// layer; this builder accepts a --type flag for the generic document_get.
func documentGetArgs(cmd *cobra.Command, args []string) (any, error) {
	id, _ := cmd.Flags().GetString("id")
	name, _ := cmd.Flags().GetString("name")
	if id == "" && name == "" {
		return nil, cliexit.Newf(cliexit.Usage, "get: --id or --name is required")
	}
	out := map[string]any{}
	if id != "" {
		out["id"] = id
	}
	if name != "" {
		out["name"] = name
	}
	if v, _ := cmd.Flags().GetString("type"); v != "" {
		out["type"] = v
	}
	if v, _ := cmd.Flags().GetString("project-id"); v != "" {
		out["project_id"] = v
	}
	return out, nil
}

func addDocumentGetFlags(c *cobra.Command, withType bool) {
	c.Flags().String("id", "", "Document id (preferred).")
	c.Flags().String("name", "", "Document name (used when id is omitted).")
	if withType {
		c.Flags().String("type", "", "Document type.")
	}
	c.Flags().String("project-id", "", "Project scope for name-keyed lookups.")
}

func newDocumentGetCmd() *cobra.Command {
	c := &cobra.Command{Use: "get", Short: "Get a document by id (preferred) or name.", RunE: readHandler("document_get", documentGetArgs)}
	addDocumentGetFlags(c, true)
	return c
}
func newDocumentListCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "list", Short: "List documents.",
		RunE: readHandler("document_list", func(cmd *cobra.Command, args []string) (any, error) {
			out, err := documentListArgs(cmd, args)
			if err != nil {
				return nil, err
			}
			m := out.(map[string]any)
			if v, _ := cmd.Flags().GetString("type"); v != "" {
				m["type"] = v
			}
			return m, nil
		}),
	}
	addDocumentListFlags(c)
	c.Flags().String("type", "", "Filter by document type.")
	return c
}

func newReviewerGetCmd() *cobra.Command {
	c := &cobra.Command{Use: "get", Short: "Get a reviewer doc by id (preferred) or name.", RunE: readHandler("reviewer_get", documentGetArgs)}
	addDocumentGetFlags(c, false)
	return c
}
func newReviewerListCmd() *cobra.Command {
	c := &cobra.Command{Use: "list", Short: "List reviewers.", RunE: readHandler("reviewer_list", documentListArgs)}
	addDocumentListFlags(c)
	return c
}
func newReviewerSearchCmd() *cobra.Command {
	c := &cobra.Command{Use: "search", Short: "Search reviewers.", RunE: readHandler("reviewer_search", documentSearchArgs)}
	addDocumentSearchFlags(c)
	return c
}
func newRoleGetCmd() *cobra.Command {
	c := &cobra.Command{Use: "get", Short: "Get a role doc by id (preferred) or name.", RunE: readHandler("role_get", documentGetArgs)}
	addDocumentGetFlags(c, false)
	return c
}
func newRoleListCmd() *cobra.Command {
	c := &cobra.Command{Use: "list", Short: "List roles.", RunE: readHandler("role_list", documentListArgs)}
	addDocumentListFlags(c)
	return c
}
func newRoleSearchCmd() *cobra.Command {
	c := &cobra.Command{Use: "search", Short: "Search roles.", RunE: readHandler("role_search", documentSearchArgs)}
	addDocumentSearchFlags(c)
	return c
}
func newSkillGetCmd() *cobra.Command {
	c := &cobra.Command{Use: "get", Short: "Get a skill doc by id (preferred) or name.", RunE: readHandler("skill_get", documentGetArgs)}
	addDocumentGetFlags(c, false)
	return c
}
func newSkillListCmd() *cobra.Command {
	c := &cobra.Command{Use: "list", Short: "List skills.", RunE: readHandler("skill_list", documentListArgs)}
	addDocumentListFlags(c)
	return c
}
func newSkillSearchCmd() *cobra.Command {
	c := &cobra.Command{Use: "search", Short: "Search skills.", RunE: readHandler("skill_search", documentSearchArgs)}
	addDocumentSearchFlags(c)
	return c
}

func newLedgerRecallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "recall",
		Short: "Walk the dereferenced-row chain.",
		RunE: readHandler("ledger_recall", func(cmd *cobra.Command, args []string) (any, error) {
			rootID, _ := cmd.Flags().GetString("root-id")
			if rootID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "ledger recall: --root-id is required")
			}
			out := map[string]any{"root_id": rootID}
			if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
				out["limit"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("root-id", "", "Root ledger row id (required).")
	c.Flags().Int("limit", 0, "Max rows.")
	_ = c.MarkFlagRequired("root-id")
	return c
}

// ----- kv -----

func newKVGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get",
		Short: "Get a key.",
		RunE: readHandler("kv_get", func(cmd *cobra.Command, args []string) (any, error) {
			scope, _ := cmd.Flags().GetString("scope")
			key, _ := cmd.Flags().GetString("key")
			if scope == "" || key == "" {
				return nil, cliexit.Newf(cliexit.Usage, "kv get: --scope and --key are required")
			}
			out := map[string]any{"scope": scope, "key": key}
			if v, _ := cmd.Flags().GetString("workspace-id"); v != "" {
				out["workspace_id"] = v
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("user-id"); v != "" {
				out["user_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("scope", "", "system | workspace | project | user (required).")
	c.Flags().String("key", "", "Key (required).")
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("project-id", "", "Project id.")
	c.Flags().String("user-id", "", "User id.")
	_ = c.MarkFlagRequired("scope")
	_ = c.MarkFlagRequired("key")
	return c
}

func newKVListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List keys.",
		RunE: readHandler("kv_list", func(cmd *cobra.Command, args []string) (any, error) {
			scope, _ := cmd.Flags().GetString("scope")
			if scope == "" {
				return nil, cliexit.Newf(cliexit.Usage, "kv list: --scope is required")
			}
			out := map[string]any{"scope": scope}
			if v, _ := cmd.Flags().GetString("workspace-id"); v != "" {
				out["workspace_id"] = v
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("user-id"); v != "" {
				out["user_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("scope", "", "system | workspace | project | user (required).")
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("project-id", "", "Project id.")
	c.Flags().String("user-id", "", "User id.")
	_ = c.MarkFlagRequired("scope")
	return c
}

func newKVGetResolvedCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get-resolved",
		Short: "Resolve a key walking system → user → project → workspace.",
		RunE: readHandler("kv_get_resolved", func(cmd *cobra.Command, args []string) (any, error) {
			key, _ := cmd.Flags().GetString("key")
			if key == "" {
				return nil, cliexit.Newf(cliexit.Usage, "kv get-resolved: --key is required")
			}
			out := map[string]any{"key": key}
			if v, _ := cmd.Flags().GetString("workspace-id"); v != "" {
				out["workspace_id"] = v
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("user-id"); v != "" {
				out["user_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("key", "", "Key (required).")
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("project-id", "", "Project id.")
	c.Flags().String("user-id", "", "User id.")
	_ = c.MarkFlagRequired("key")
	return c
}

// ----- repo -----

func newRepoGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <repo_id>",
		Short: "Get a repo row.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("repo_get", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"repo_id": args[0]}, nil
		}),
	}
	return c
}

func newRepoListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List repos.",
		RunE: readHandler("repo_list", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("status"); v != "" {
				out["status"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope.")
	c.Flags().String("status", "", "active | archived | all.")
	return c
}

func newRepoSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search",
		Short: "Symbol search via the code indexer.",
		RunE: readHandler("repo_search", func(cmd *cobra.Command, args []string) (any, error) {
			repoID, _ := cmd.Flags().GetString("repo-id")
			query, _ := cmd.Flags().GetString("query")
			if repoID == "" || query == "" {
				return nil, cliexit.Newf(cliexit.Usage, "repo search: --repo-id and --query are required")
			}
			out := map[string]any{"repo_id": repoID, "query": query}
			if v, _ := cmd.Flags().GetString("kind"); v != "" {
				out["kind"] = v
			}
			if v, _ := cmd.Flags().GetString("language"); v != "" {
				out["language"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("repo-id", "", "Repo id (required).")
	c.Flags().String("query", "", "Symbol query (required).")
	c.Flags().String("kind", "", "Filter by symbol kind.")
	c.Flags().String("language", "", "Filter by language.")
	_ = c.MarkFlagRequired("repo-id")
	_ = c.MarkFlagRequired("query")
	return c
}

func newRepoSearchTextCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search-text",
		Short: "Full-text search via the code indexer.",
		RunE: readHandler("repo_search_text", func(cmd *cobra.Command, args []string) (any, error) {
			repoID, _ := cmd.Flags().GetString("repo-id")
			query, _ := cmd.Flags().GetString("query")
			if repoID == "" || query == "" {
				return nil, cliexit.Newf(cliexit.Usage, "repo search-text: --repo-id and --query are required")
			}
			out := map[string]any{"repo_id": repoID, "query": query}
			if v, _ := cmd.Flags().GetString("file-pattern"); v != "" {
				out["file_pattern"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("repo-id", "", "Repo id (required).")
	c.Flags().String("query", "", "Text query (required).")
	c.Flags().String("file-pattern", "", "Restrict to files matching pattern.")
	_ = c.MarkFlagRequired("repo-id")
	_ = c.MarkFlagRequired("query")
	return c
}

func newRepoGetSymbolSourceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get-symbol-source",
		Short: "Return a symbol's source.",
		RunE: readHandler("repo_get_symbol_source", func(cmd *cobra.Command, args []string) (any, error) {
			repoID, _ := cmd.Flags().GetString("repo-id")
			symbolID, _ := cmd.Flags().GetString("symbol-id")
			if repoID == "" || symbolID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "repo get-symbol-source: --repo-id and --symbol-id are required")
			}
			return map[string]any{"repo_id": repoID, "symbol_id": symbolID}, nil
		}),
	}
	c.Flags().String("repo-id", "", "Repo id (required).")
	c.Flags().String("symbol-id", "", "Symbol id (required).")
	_ = c.MarkFlagRequired("repo-id")
	_ = c.MarkFlagRequired("symbol-id")
	return c
}

func newRepoGetFileCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get-file",
		Short: "Return a file's contents.",
		RunE: readHandler("repo_get_file", func(cmd *cobra.Command, args []string) (any, error) {
			repoID, _ := cmd.Flags().GetString("repo-id")
			path, _ := cmd.Flags().GetString("path")
			if repoID == "" || path == "" {
				return nil, cliexit.Newf(cliexit.Usage, "repo get-file: --repo-id and --path are required")
			}
			return map[string]any{"repo_id": repoID, "path": path}, nil
		}),
	}
	c.Flags().String("repo-id", "", "Repo id (required).")
	c.Flags().String("path", "", "File path (required).")
	_ = c.MarkFlagRequired("repo-id")
	_ = c.MarkFlagRequired("path")
	return c
}

func newRepoGetOutlineCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get-outline",
		Short: "Return a file's outline.",
		RunE: readHandler("repo_get_outline", func(cmd *cobra.Command, args []string) (any, error) {
			repoID, _ := cmd.Flags().GetString("repo-id")
			path, _ := cmd.Flags().GetString("path")
			if repoID == "" || path == "" {
				return nil, cliexit.Newf(cliexit.Usage, "repo get-outline: --repo-id and --path are required")
			}
			return map[string]any{"repo_id": repoID, "path": path}, nil
		}),
	}
	c.Flags().String("repo-id", "", "Repo id (required).")
	c.Flags().String("path", "", "File path (required).")
	_ = c.MarkFlagRequired("repo-id")
	_ = c.MarkFlagRequired("path")
	return c
}

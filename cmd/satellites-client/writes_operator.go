// writes_operator.go — operator-tier mutating verb handlers landed in
// sty_f38bd573 Tier A. Replaces verbStubs in nouns.go for the ~30 CRUD
// mutates that route through stores directly (no agent_compose,
// agent_apikey_*, *_seed_run, portal_replicate, document_ingest_file —
// those live in Tier B follow-up).

package main

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// unmarshalJSON is a tiny helper for the CLI flag parsers that need
// JSON-array payloads (portal replicate actions/cookies).
func unmarshalJSON(raw string, dst any) error { return json.Unmarshal([]byte(raw), dst) }

// ----- story -----

func newStoryCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a new story.",
		RunE: writeHandler("story_create", func(cmd *cobra.Command, args []string) (any, error) {
			projectID, _ := cmd.Flags().GetString("project-id")
			title, _ := cmd.Flags().GetString("title")
			if projectID == "" || title == "" {
				return nil, cliexit.Newf(cliexit.Usage, "story create: --project-id and --title are required")
			}
			out := map[string]any{"project_id": projectID, "title": title}
			if v, _ := cmd.Flags().GetString("description"); v != "" {
				out["description"] = v
			}
			if v, _ := cmd.Flags().GetString("acceptance-criteria"); v != "" {
				out["acceptance_criteria"] = v
			}
			if v, _ := cmd.Flags().GetString("priority"); v != "" {
				out["priority"] = v
			}
			if v, _ := cmd.Flags().GetString("category"); v != "" {
				out["category"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
				out["tags"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope (required).")
	c.Flags().String("title", "", "Story title (required).")
	c.Flags().String("description", "", "Description body.")
	c.Flags().String("acceptance-criteria", "", "Acceptance criteria body.")
	c.Flags().String("priority", "", "critical | high | medium | low.")
	c.Flags().String("category", "", "feature | bug | improvement | infrastructure | documentation.")
	c.Flags().StringSlice("tags", nil, "Free-form tags.")
	_ = c.MarkFlagRequired("project-id")
	_ = c.MarkFlagRequired("title")
	return c
}

func newStoryUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Update a story's mutable fields.",
		RunE: writeHandler("story_update", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			if id == "" {
				return nil, cliexit.Newf(cliexit.Usage, "story update: --id is required")
			}
			out := map[string]any{"id": id}
			if cmd.Flags().Changed("title") {
				v, _ := cmd.Flags().GetString("title")
				out["title"] = v
			}
			if cmd.Flags().Changed("description") {
				v, _ := cmd.Flags().GetString("description")
				out["description"] = v
			}
			if cmd.Flags().Changed("acceptance-criteria") {
				v, _ := cmd.Flags().GetString("acceptance-criteria")
				out["acceptance_criteria"] = v
			}
			if cmd.Flags().Changed("category") {
				v, _ := cmd.Flags().GetString("category")
				out["category"] = v
			}
			if cmd.Flags().Changed("priority") {
				v, _ := cmd.Flags().GetString("priority")
				out["priority"] = v
			}
			if cmd.Flags().Changed("tags") {
				v, _ := cmd.Flags().GetStringSlice("tags")
				out["tags"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Story id (required).")
	c.Flags().String("title", "", "New title.")
	c.Flags().String("description", "", "New description.")
	c.Flags().String("acceptance-criteria", "", "New acceptance criteria.")
	c.Flags().String("category", "", "New category.")
	c.Flags().String("priority", "", "New priority.")
	c.Flags().StringSlice("tags", nil, "Replace tag set (empty clears).")
	_ = c.MarkFlagRequired("id")
	return c
}

func newStoryDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <story_id>",
		Short: "Cancel a story (terminal transition).",
		Args:  cobra.ExactArgs(1),
		RunE: writeHandler("story_delete", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

// ----- project -----

func newProjectCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a new project.",
		RunE: writeHandler("project_create", func(cmd *cobra.Command, args []string) (any, error) {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "project create: --name is required")
			}
			return map[string]any{"name": name}, nil
		}),
	}
	c.Flags().String("name", "", "Project name (required).")
	_ = c.MarkFlagRequired("name")
	return c
}

func newProjectUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Update a project's name / mcp_url.",
		RunE: writeHandler("project_update", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			if id == "" {
				return nil, cliexit.Newf(cliexit.Usage, "project update: --id is required")
			}
			out := map[string]any{"id": id}
			if cmd.Flags().Changed("name") {
				v, _ := cmd.Flags().GetString("name")
				out["name"] = v
			}
			if cmd.Flags().Changed("mcp-url") {
				v, _ := cmd.Flags().GetString("mcp-url")
				out["mcp_url"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Project id (required).")
	c.Flags().String("name", "", "New name.")
	c.Flags().String("mcp-url", "", "Override MCP URL.")
	_ = c.MarkFlagRequired("id")
	return c
}

func newProjectDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <project_id>",
		Short: "Archive a project.",
		Args:  cobra.ExactArgs(1),
		RunE: writeHandler("project_delete", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

// ----- workspace -----

func newWorkspaceCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a workspace [admin].",
		RunE: writeHandler("workspace_create", func(cmd *cobra.Command, args []string) (any, error) {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "workspace create: --name is required")
			}
			return map[string]any{"name": name}, nil
		}),
	}
	c.Flags().String("name", "", "Workspace name (required).")
	_ = c.MarkFlagRequired("name")
	return c
}

func newWorkspaceMemberAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "member-add",
		Short: "Add a workspace member [admin].",
		RunE: writeHandler("workspace_member_add", func(cmd *cobra.Command, args []string) (any, error) {
			workspaceID, _ := cmd.Flags().GetString("workspace-id")
			userID, _ := cmd.Flags().GetString("user-id")
			role, _ := cmd.Flags().GetString("role")
			if workspaceID == "" || userID == "" || role == "" {
				return nil, cliexit.Newf(cliexit.Usage, "workspace member-add: --workspace-id, --user-id, --role required")
			}
			return map[string]any{"workspace_id": workspaceID, "user_id": userID, "role": role}, nil
		}),
	}
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("user-id", "", "User id.")
	c.Flags().String("role", "", "Role (admin | member).")
	_ = c.MarkFlagRequired("workspace-id")
	_ = c.MarkFlagRequired("user-id")
	_ = c.MarkFlagRequired("role")
	return c
}

func newWorkspaceMemberUpdateRoleCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "member-update-role",
		Short: "Change a member's role [admin].",
		RunE: writeHandler("workspace_member_update_role", func(cmd *cobra.Command, args []string) (any, error) {
			workspaceID, _ := cmd.Flags().GetString("workspace-id")
			userID, _ := cmd.Flags().GetString("user-id")
			role, _ := cmd.Flags().GetString("role")
			if workspaceID == "" || userID == "" || role == "" {
				return nil, cliexit.Newf(cliexit.Usage, "workspace member-update-role: --workspace-id, --user-id, --role required")
			}
			return map[string]any{"workspace_id": workspaceID, "user_id": userID, "role": role}, nil
		}),
	}
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("user-id", "", "User id.")
	c.Flags().String("role", "", "New role (admin | member).")
	_ = c.MarkFlagRequired("workspace-id")
	_ = c.MarkFlagRequired("user-id")
	_ = c.MarkFlagRequired("role")
	return c
}

func newWorkspaceMemberRemoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "member-remove",
		Short: "Remove a workspace member [admin].",
		RunE: writeHandler("workspace_member_remove", func(cmd *cobra.Command, args []string) (any, error) {
			workspaceID, _ := cmd.Flags().GetString("workspace-id")
			userID, _ := cmd.Flags().GetString("user-id")
			if workspaceID == "" || userID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "workspace member-remove: --workspace-id and --user-id required")
			}
			return map[string]any{"workspace_id": workspaceID, "user_id": userID}, nil
		}),
	}
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("user-id", "", "User id.")
	_ = c.MarkFlagRequired("workspace-id")
	_ = c.MarkFlagRequired("user-id")
	return c
}

// ----- kv -----

func newKVSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set",
		Short: "Set a key/value.",
		RunE: writeHandler("kv_set", func(cmd *cobra.Command, args []string) (any, error) {
			scope, _ := cmd.Flags().GetString("scope")
			key, _ := cmd.Flags().GetString("key")
			value, _ := cmd.Flags().GetString("value")
			if scope == "" || key == "" {
				return nil, cliexit.Newf(cliexit.Usage, "kv set: --scope and --key required")
			}
			if pf.Stdin {
				if value != "" {
					return nil, cliexit.Newf(cliexit.Usage, "kv set: --stdin and --value are mutually exclusive")
				}
				body, err := readBodyFromStdin()
				if err != nil {
					return nil, err
				}
				value = body
			}
			out := map[string]any{"scope": scope, "key": key, "value": value}
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
	c.Flags().String("value", "", "Value (use --stdin to read from stdin).")
	c.Flags().String("workspace-id", "", "Workspace id.")
	c.Flags().String("project-id", "", "Project id.")
	c.Flags().String("user-id", "", "User id.")
	_ = c.MarkFlagRequired("scope")
	_ = c.MarkFlagRequired("key")
	return c
}

func newKVDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete a key (writes tombstone).",
		RunE: writeHandler("kv_delete", func(cmd *cobra.Command, args []string) (any, error) {
			scope, _ := cmd.Flags().GetString("scope")
			key, _ := cmd.Flags().GetString("key")
			if scope == "" || key == "" {
				return nil, cliexit.Newf(cliexit.Usage, "kv delete: --scope and --key required")
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

// ----- changelog -----

func newChangelogAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add",
		Short: "Append a changelog row.",
		RunE: writeHandler("changelog_add", func(cmd *cobra.Command, args []string) (any, error) {
			service, _ := cmd.Flags().GetString("service")
			content, _ := cmd.Flags().GetString("content")
			if service == "" {
				return nil, cliexit.Newf(cliexit.Usage, "changelog add: --service is required")
			}
			if pf.Stdin {
				if content != "" {
					return nil, cliexit.Newf(cliexit.Usage, "changelog add: --stdin and --content are mutually exclusive")
				}
				body, err := readBodyFromStdin()
				if err != nil {
					return nil, err
				}
				content = body
			}
			if content == "" {
				return nil, cliexit.Newf(cliexit.Usage, "changelog add: --content or --stdin required")
			}
			out := map[string]any{"service": service, "content": content}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("version-from"); v != "" {
				out["version_from"] = v
			}
			if v, _ := cmd.Flags().GetString("version-to"); v != "" {
				out["version_to"] = v
			}
			if v, _ := cmd.Flags().GetString("effective-date"); v != "" {
				out["effective_date"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("service", "", "Service name (required).")
	c.Flags().String("content", "", "Content body (use --stdin to read from stdin).")
	c.Flags().String("project-id", "", "Project scope.")
	c.Flags().String("version-from", "", "Previous version.")
	c.Flags().String("version-to", "", "New version.")
	c.Flags().String("effective-date", "", "RFC3339 timestamp.")
	_ = c.MarkFlagRequired("service")
	return c
}

func newChangelogUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Update a changelog row.",
		RunE: writeHandler("changelog_update", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			if id == "" {
				return nil, cliexit.Newf(cliexit.Usage, "changelog update: --id is required")
			}
			out := map[string]any{"id": id}
			if cmd.Flags().Changed("content") {
				v, _ := cmd.Flags().GetString("content")
				out["content"] = v
			}
			if cmd.Flags().Changed("version-from") {
				v, _ := cmd.Flags().GetString("version-from")
				out["version_from"] = v
			}
			if cmd.Flags().Changed("version-to") {
				v, _ := cmd.Flags().GetString("version-to")
				out["version_to"] = v
			}
			if cmd.Flags().Changed("effective-date") {
				v, _ := cmd.Flags().GetString("effective-date")
				out["effective_date"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Changelog id (required).")
	c.Flags().String("content", "", "New content.")
	c.Flags().String("version-from", "", "New version_from.")
	c.Flags().String("version-to", "", "New version_to.")
	c.Flags().String("effective-date", "", "New effective date (RFC3339).")
	_ = c.MarkFlagRequired("id")
	return c
}

func newChangelogDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a changelog row.",
		Args:  cobra.ExactArgs(1),
		RunE: writeHandler("changelog_delete", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

// ----- repo -----

func newRepoAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add",
		Short: "Register a git remote on a project.",
		RunE: writeHandler("repo_add", func(cmd *cobra.Command, args []string) (any, error) {
			gitRemote, _ := cmd.Flags().GetString("git-remote")
			if gitRemote == "" {
				return nil, cliexit.Newf(cliexit.Usage, "repo add: --git-remote is required")
			}
			out := map[string]any{"git_remote": gitRemote}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("default-branch"); v != "" {
				out["default_branch"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("git-remote", "", "Git remote URL (required).")
	c.Flags().String("project-id", "", "Project scope.")
	c.Flags().String("default-branch", "", "Default branch (defaults to main).")
	_ = c.MarkFlagRequired("git-remote")
	return c
}

// ----- ledger -----

func newLedgerDereferenceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dereference",
		Short: "Mark a ledger row as dereferenced.",
		RunE: writeHandler("ledger_dereference", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			reason, _ := cmd.Flags().GetString("reason")
			if id == "" || reason == "" {
				return nil, cliexit.Newf(cliexit.Usage, "ledger dereference: --id and --reason required")
			}
			return map[string]any{"id": id, "reason": reason}, nil
		}),
	}
	c.Flags().String("id", "", "Ledger row id (required).")
	c.Flags().String("reason", "", "Human-readable reason (required).")
	_ = c.MarkFlagRequired("id")
	_ = c.MarkFlagRequired("reason")
	return c
}

// ----- session -----

func newSessionRegisterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "register",
		Short: "Register / re-register the caller's session.",
		RunE: writeHandler("session_register", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("session-id"); v != "" {
				out["session_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope.")
	c.Flags().String("session-id", "", "Explicit session id.")
	return c
}

// ----- document family (create/update/delete) -----

// documentCreateArgs builds shared create-shape. Per-noun wrappers
// (agent_add / contract_add / etc.) pin type at the server-route
// layer; the CLI just sends the supplied --type-flag value (or none).
func documentCreateArgs(cmd *cobra.Command, args []string) (any, error) {
	scope, _ := cmd.Flags().GetString("scope")
	name, _ := cmd.Flags().GetString("name")
	if scope == "" || name == "" {
		return nil, cliexit.Newf(cliexit.Usage, "--scope and --name are required")
	}
	out := map[string]any{"scope": scope, "name": name}
	if v, _ := cmd.Flags().GetString("type"); v != "" {
		out["type"] = v
	}
	if v, _ := cmd.Flags().GetString("project-id"); v != "" {
		out["project_id"] = v
	}
	body, _ := cmd.Flags().GetString("body")
	if pf.Stdin {
		if body != "" {
			return nil, cliexit.Newf(cliexit.Usage, "--stdin and --body are mutually exclusive")
		}
		got, err := readBodyFromStdin()
		if err != nil {
			return nil, err
		}
		body = got
	}
	if body != "" {
		out["body"] = body
	}
	if v, _ := cmd.Flags().GetString("structured"); v != "" {
		out["structured"] = v
	}
	if v, _ := cmd.Flags().GetString("contract-binding"); v != "" {
		out["contract_binding"] = v
	}
	if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
		out["tags"] = v
	}
	if v, _ := cmd.Flags().GetString("status"); v != "" {
		out["status"] = v
	}
	return out, nil
}

func addDocumentCreateFlags(c *cobra.Command, withType bool) {
	c.Flags().String("scope", "", "system | project (required).")
	c.Flags().String("name", "", "Document name (required).")
	if withType {
		c.Flags().String("type", "", "Document type.")
	}
	c.Flags().String("project-id", "", "Project scope (required for scope=project).")
	c.Flags().String("body", "", "Markdown body (use --stdin to read from stdin).")
	c.Flags().String("structured", "", "Type-specific JSON payload.")
	c.Flags().String("contract-binding", "", "Contract binding (for skill/reviewer).")
	c.Flags().StringSlice("tags", nil, "Free-form tags.")
	c.Flags().String("status", "", "active (default) | archived.")
	_ = c.MarkFlagRequired("scope")
	_ = c.MarkFlagRequired("name")
}

func documentUpdateArgs(cmd *cobra.Command, args []string) (any, error) {
	id, _ := cmd.Flags().GetString("id")
	if id == "" {
		return nil, cliexit.Newf(cliexit.Usage, "--id is required")
	}
	out := map[string]any{"id": id}
	if cmd.Flags().Changed("body") {
		v, _ := cmd.Flags().GetString("body")
		out["body"] = v
	}
	if pf.Stdin {
		if cmd.Flags().Changed("body") {
			return nil, cliexit.Newf(cliexit.Usage, "--stdin and --body are mutually exclusive")
		}
		body, err := readBodyFromStdin()
		if err != nil {
			return nil, err
		}
		out["body"] = body
	}
	if cmd.Flags().Changed("structured") {
		v, _ := cmd.Flags().GetString("structured")
		out["structured"] = v
	}
	if cmd.Flags().Changed("tags") {
		v, _ := cmd.Flags().GetStringSlice("tags")
		out["tags"] = v
	}
	if cmd.Flags().Changed("status") {
		v, _ := cmd.Flags().GetString("status")
		out["status"] = v
	}
	if cmd.Flags().Changed("contract-binding") {
		v, _ := cmd.Flags().GetString("contract-binding")
		out["contract_binding"] = v
	}
	return out, nil
}

func addDocumentUpdateFlags(c *cobra.Command) {
	c.Flags().String("id", "", "Document id (required).")
	c.Flags().String("body", "", "New body.")
	c.Flags().String("structured", "", "New structured payload (JSON string).")
	c.Flags().StringSlice("tags", nil, "Replace tag set.")
	c.Flags().String("status", "", "active | archived.")
	c.Flags().String("contract-binding", "", "Replace contract binding.")
	_ = c.MarkFlagRequired("id")
}

func documentDeleteArgs(cmd *cobra.Command, args []string) (any, error) {
	id, _ := cmd.Flags().GetString("id")
	if id == "" {
		return nil, cliexit.Newf(cliexit.Usage, "--id is required")
	}
	out := map[string]any{"id": id}
	if v, _ := cmd.Flags().GetString("mode"); v != "" {
		out["mode"] = v
	}
	return out, nil
}

func addDocumentDeleteFlags(c *cobra.Command) {
	c.Flags().String("id", "", "Document id (required).")
	c.Flags().String("mode", "", "archive (default) | hard.")
	_ = c.MarkFlagRequired("id")
}

func newDocumentCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a document.",
		RunE:  writeHandler("document_create", documentCreateArgs),
	}
	addDocumentCreateFlags(c, true)
	return c
}

func newDocumentUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Update a document.",
		RunE:  writeHandler("document_update", documentUpdateArgs),
	}
	addDocumentUpdateFlags(c)
	return c
}

func newDocumentDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete",
		Short: "Archive (default) or hard-delete a document.",
		RunE:  writeHandler("document_delete", documentDeleteArgs),
	}
	addDocumentDeleteFlags(c)
	return c
}

// Doc-family wrappers — same arg-builders, route-pinned type on server.
func newAgentAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add an agent doc.", RunE: writeHandler("agent_add", documentCreateArgs)}
	addDocumentCreateFlags(c, false)
	return c
}
func newAgentUpdateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Update an agent doc.", RunE: writeHandler("agent_update", documentUpdateArgs)}
	addDocumentUpdateFlags(c)
	return c
}
func newAgentDeleteCmd() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Archive an agent doc.", RunE: writeHandler("agent_delete", documentDeleteArgs)}
	addDocumentDeleteFlags(c)
	return c
}
func newContractAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add a contract doc.", RunE: writeHandler("contract_add", documentCreateArgs)}
	addDocumentCreateFlags(c, false)
	return c
}
func newContractUpdateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Update a contract doc.", RunE: writeHandler("contract_update", documentUpdateArgs)}
	addDocumentUpdateFlags(c)
	return c
}
func newContractDeleteCmd() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Archive a contract.", RunE: writeHandler("contract_delete", documentDeleteArgs)}
	addDocumentDeleteFlags(c)
	return c
}
func newPrincipleAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add a principle.", RunE: writeHandler("principle_add", documentCreateArgs)}
	addDocumentCreateFlags(c, false)
	return c
}
func newPrincipleUpdateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Update a principle.", RunE: writeHandler("principle_update", documentUpdateArgs)}
	addDocumentUpdateFlags(c)
	return c
}
func newPrincipleDeleteCmd() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Archive a principle.", RunE: writeHandler("principle_delete", documentDeleteArgs)}
	addDocumentDeleteFlags(c)
	return c
}
func newReviewerAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add a reviewer doc.", RunE: writeHandler("reviewer_add", documentCreateArgs)}
	addDocumentCreateFlags(c, false)
	return c
}
func newReviewerUpdateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Update a reviewer doc.", RunE: writeHandler("reviewer_update", documentUpdateArgs)}
	addDocumentUpdateFlags(c)
	return c
}
func newReviewerDeleteCmd() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Archive a reviewer.", RunE: writeHandler("reviewer_delete", documentDeleteArgs)}
	addDocumentDeleteFlags(c)
	return c
}
func newRoleAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add a role doc.", RunE: writeHandler("role_add", documentCreateArgs)}
	addDocumentCreateFlags(c, false)
	return c
}
func newRoleUpdateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Update a role doc.", RunE: writeHandler("role_update", documentUpdateArgs)}
	addDocumentUpdateFlags(c)
	return c
}
func newRoleDeleteCmd() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Archive a role.", RunE: writeHandler("role_delete", documentDeleteArgs)}
	addDocumentDeleteFlags(c)
	return c
}
func newSkillAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add a skill doc.", RunE: writeHandler("skill_add", documentCreateArgs)}
	addDocumentCreateFlags(c, false)
	return c
}
func newSkillUpdateCmd() *cobra.Command {
	c := &cobra.Command{Use: "update", Short: "Update a skill doc.", RunE: writeHandler("skill_update", documentUpdateArgs)}
	addDocumentUpdateFlags(c)
	return c
}
func newSkillDeleteCmd() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Archive a skill.", RunE: writeHandler("skill_delete", documentDeleteArgs)}
	addDocumentDeleteFlags(c)
	return c
}

// ----- sty_0b419d98 Tier B mutates -----

func newAgentAPIKeyCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apikey-create",
		Short: "Create an agent API key [admin].",
		RunE: writeHandler("agent_apikey_create", func(cmd *cobra.Command, args []string) (any, error) {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "agent apikey-create: --name is required")
			}
			out := map[string]any{"name": name}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("expires-at"); v != "" {
				out["expires_at"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("name", "", "Key name (required).")
	c.Flags().String("project-id", "", "Optional project scope.")
	c.Flags().String("expires-at", "", "Optional RFC3339 expiry.")
	_ = c.MarkFlagRequired("name")
	return c
}

func newAgentAPIKeyListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apikey-list",
		Short: "List agent API keys [admin].",
		RunE: writeHandler("agent_apikey_list", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetBool("include-archived"); v {
				out["include_archived"] = true
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Filter by project.")
	c.Flags().Bool("include-archived", false, "Include archived keys.")
	return c
}

func newAgentAPIKeyDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apikey-delete <id>",
		Short: "Delete an agent API key [admin].",
		Args:  cobra.ExactArgs(1),
		RunE: writeHandler("agent_apikey_delete", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

func newAgentComposeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "compose",
		Short: "Compose an ephemeral or canonical agent.",
		RunE: writeHandler("agent_compose", func(cmd *cobra.Command, args []string) (any, error) {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "agent compose: --name is required")
			}
			out := map[string]any{"name": name}
			body, _ := cmd.Flags().GetString("body")
			if pf.Stdin {
				if body != "" {
					return nil, cliexit.Newf(cliexit.Usage, "agent compose: --stdin and --body are mutually exclusive")
				}
				got, err := readBodyFromStdin()
				if err != nil {
					return nil, err
				}
				body = got
			}
			if body != "" {
				out["body"] = body
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("skill-refs"); len(v) > 0 {
				out["skill_refs"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("permission-patterns"); len(v) > 0 {
				out["permission_patterns"] = v
			}
			if v, _ := cmd.Flags().GetBool("ephemeral"); v {
				out["ephemeral"] = true
			}
			if v, _ := cmd.Flags().GetString("story-id"); v != "" {
				out["story_id"] = v
			}
			if v, _ := cmd.Flags().GetString("reason"); v != "" {
				out["reason"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
				out["tags"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("name", "", "Agent name (required).")
	c.Flags().String("body", "", "Markdown body (use --stdin to read from stdin).")
	c.Flags().String("project-id", "", "Optional project scope (omit for scope=system).")
	c.Flags().StringSlice("skill-refs", nil, "Skill document ids.")
	c.Flags().StringSlice("permission-patterns", nil, "PreToolUse hook patterns.")
	c.Flags().Bool("ephemeral", false, "Mark agent as ephemeral (requires --story-id).")
	c.Flags().String("story-id", "", "Required when ephemeral=true.")
	c.Flags().String("reason", "", "Free-form rationale.")
	c.Flags().StringSlice("tags", nil, "Free-form tags.")
	_ = c.MarkFlagRequired("name")
	return c
}

func newProjectSeedRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "seed-run",
		Short: "Re-run the project-tier configseed loader [admin].",
		RunE: writeHandler("project_seed_run", func(cmd *cobra.Command, args []string) (any, error) {
			projectID, _ := cmd.Flags().GetString("project-id")
			if projectID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "project seed-run: --project-id is required")
			}
			return map[string]any{"project_id": projectID}, nil
		}),
	}
	c.Flags().String("project-id", "", "Project id (required).")
	_ = c.MarkFlagRequired("project-id")
	return c
}

func newSystemSeedRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seed-run",
		Short: "Re-run the system-tier configseed loader [admin].",
		RunE:  writeHandler("system_seed_run", func(cmd *cobra.Command, args []string) (any, error) { return map[string]any{}, nil }),
	}
}

func newPortalReplicateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "replicate",
		Short: "Drive a headless browser through actions + ledger evidence onto a story.",
		RunE: writeHandler("portal_replicate", func(cmd *cobra.Command, args []string) (any, error) {
			storyID, _ := cmd.Flags().GetString("story-id")
			targetURL, _ := cmd.Flags().GetString("target-url")
			actionsRaw, _ := cmd.Flags().GetString("actions")
			if storyID == "" || targetURL == "" || actionsRaw == "" {
				return nil, cliexit.Newf(cliexit.Usage, "portal replicate: --story-id, --target-url, --actions required")
			}
			var actions []any
			if err := unmarshalJSON(actionsRaw, &actions); err != nil {
				return nil, cliexit.Newf(cliexit.Usage, "portal replicate: --actions must be valid JSON array: %v", err)
			}
			out := map[string]any{
				"story_id":   storyID,
				"target_url": targetURL,
				"actions":    actions,
			}
			if cookiesRaw, _ := cmd.Flags().GetString("cookies"); cookiesRaw != "" {
				var cookies []any
				if err := unmarshalJSON(cookiesRaw, &cookies); err != nil {
					return nil, cliexit.Newf(cliexit.Usage, "portal replicate: --cookies must be valid JSON array: %v", err)
				}
				out["cookies"] = cookies
			}
			return out, nil
		}),
	}
	c.Flags().String("story-id", "", "Story to attach evidence to (required).")
	c.Flags().String("target-url", "", "Absolute base URL (required).")
	c.Flags().String("actions", "", "JSON array of {type, selector?, value?, label?} (required).")
	c.Flags().String("cookies", "", "Optional JSON array of cookie objects.")
	_ = c.MarkFlagRequired("story-id")
	_ = c.MarkFlagRequired("target-url")
	_ = c.MarkFlagRequired("actions")
	return c
}

func newDocumentIngestFileCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ingest-file",
		Short: "Ingest a markdown file as a document.",
		RunE: writeHandler("document_ingest_file", func(cmd *cobra.Command, args []string) (any, error) {
			path, _ := cmd.Flags().GetString("path")
			if path == "" {
				return nil, cliexit.Newf(cliexit.Usage, "document ingest-file: --path is required")
			}
			out := map[string]any{"path": path}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("path", "", "Path to markdown file (relative to docsDir; required).")
	c.Flags().String("project-id", "", "Optional project scope.")
	_ = c.MarkFlagRequired("path")
	return c
}

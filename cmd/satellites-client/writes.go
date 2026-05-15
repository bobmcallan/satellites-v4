package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliio"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// readBodyFromStdin returns os.Stdin's contents as a string, or an
// error wrapping cliexit.Usage on empty input. Verbs that take a
// long body (story description, ledger content, document body, task
// prompt) call this when --stdin is set.
func readBodyFromStdin() (string, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", cliexit.Wrap(cliexit.Usage, err)
	}
	if len(raw) == 0 {
		return "", cliexit.Newf(cliexit.Usage, "stdin: empty body (use the inline flag or pipe non-empty input)")
	}
	return string(raw), nil
}

// renderDryRun prints the HTTP wire shape that would be POSTed to the
// satellites-server /api/v1/<noun>/<verb> route. Used by mutating verbs
// when --dry-run is set; the caller returns nil immediately after.
func renderDryRun(toolName string, args any) error {
	envelope := map[string]any{
		"method": "POST",
		"path":   "/api/v1" + cliremote.ToolNameToPath(toolName),
		"body":   args,
	}
	return cliio.RenderJSONIndent(os.Stdout, envelope)
}

// writeHandler is the per-verb boilerplate for mutating verbs. The
// argsFn produces the typed input map. When --dry-run is set the
// envelope is printed and the call is skipped; otherwise callRemote
// + renderResponse run as for read verbs.
func writeHandler(toolName string, argsFn func(cmd *cobra.Command, args []string) (any, error)) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		argv, err := argsFn(cmd, args)
		if err != nil {
			return err
		}
		if pf.DryRun {
			return renderDryRun(toolName, argv)
		}
		raw, err := callRemote(cmd, toolName, argv)
		if err != nil {
			return err
		}
		return renderResponse(raw)
	}
}

// task add
func newTaskAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add",
		Short: "Mint a task at status=published.",
		RunE: writeHandler("task_add", func(cmd *cobra.Command, args []string) (any, error) {
			agentID, _ := cmd.Flags().GetString("agent-id")
			if agentID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task add: --agent-id is required")
			}
			prompt, _ := cmd.Flags().GetString("prompt")
			if pf.Stdin {
				if prompt != "" {
					return nil, cliexit.Newf(cliexit.Usage, "task add: --stdin and --prompt are mutually exclusive")
				}
				body, err := readBodyFromStdin()
				if err != nil {
					return nil, err
				}
				prompt = body
			}
			if prompt == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task add: --prompt or --stdin is required")
			}
			out := map[string]any{
				"agent_id": agentID,
				"prompt":   prompt,
			}
			if v, _ := cmd.Flags().GetString("story-id"); v != "" {
				out["story_id"] = v
			}
			if v, _ := cmd.Flags().GetString("action"); v != "" {
				out["action"] = v
			}
			if v, _ := cmd.Flags().GetString("kind"); v != "" {
				out["kind"] = v
			}
			if v, _ := cmd.Flags().GetString("priority"); v != "" {
				out["priority"] = v
			}
			if v, _ := cmd.Flags().GetString("prior-task-id"); v != "" {
				out["prior_task_id"] = v
			}
			if v, _ := cmd.Flags().GetString("parent-task-id"); v != "" {
				out["parent_task_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("agent-id", "", "Agent doc id (required).")
	c.Flags().String("prompt", "", "Task prompt body (use --stdin to read from stdin).")
	c.Flags().String("story-id", "", "Owning story id (optional; substrate auto-mints when absent).")
	c.Flags().String("action", "", "Action string, e.g. contract:develop.")
	c.Flags().String("kind", "", "Task kind: work (default) or review.")
	c.Flags().String("priority", "", "Priority: critical | high | medium (default) | low.")
	c.Flags().String("prior-task-id", "", "Same-slot retry pointer. Caller wins over auto-supersession detection.")
	c.Flags().String("parent-task-id", "", "Conversation-thread anchor.")
	_ = c.MarkFlagRequired("agent-id")
	return c
}

// task claim
func newTaskClaimCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "claim",
		Short: "Atomic claim of the highest-priority queued task.",
		RunE: writeHandler("task_claim", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("workspace-id"); v != "" {
				out["workspace_id"] = v
			}
			if v, _ := cmd.Flags().GetString("worker-id"); v != "" {
				out["worker_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("workspace-id", "", "Narrow to one workspace.")
	c.Flags().String("worker-id", "", "Worker id (defaults to caller user id).")
	return c
}

// task update
func newTaskUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Mutate a task's lifecycle state or patch its linkage.",
		RunE: writeHandler("task_update", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			if id == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task update: --id is required")
			}
			out := map[string]any{"id": id}
			status, _ := cmd.Flags().GetString("status")
			priorChanged := cmd.Flags().Changed("prior-task-id")
			parentChanged := cmd.Flags().Changed("parent-task-id")
			if status == "" && !priorChanged && !parentChanged {
				return nil, cliexit.Newf(cliexit.Usage, "task update: supply --status, --prior-task-id, or --parent-task-id")
			}
			if status != "" {
				out["status"] = status
			}
			if v, _ := cmd.Flags().GetString("outcome"); v != "" {
				out["outcome"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("evidence-ledger-ids"); len(v) > 0 {
				out["evidence_ledger_ids"] = v
			}
			if priorChanged {
				v, _ := cmd.Flags().GetString("prior-task-id")
				out["prior_task_id"] = v
			}
			if parentChanged {
				v, _ := cmd.Flags().GetString("parent-task-id")
				out["parent_task_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Task id (required).")
	c.Flags().String("status", "", "Target status (today: closed). Omit to patch linkage fields only.")
	c.Flags().String("outcome", "", "success (default) | failure when status=closed.")
	c.Flags().StringSlice("evidence-ledger-ids", nil, "Evidence ledger row ids referenced.")
	c.Flags().String("prior-task-id", "", "Patch the same-slot retry pointer. Pass empty string to clear.")
	c.Flags().String("parent-task-id", "", "Patch the conversation-thread anchor. Pass empty string to clear.")
	_ = c.MarkFlagRequired("id")
	return c
}

// ledger append
func newLedgerAppendCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "append",
		Short: "Append an event row to a project's ledger.",
		RunE: writeHandler("ledger_append", func(cmd *cobra.Command, args []string) (any, error) {
			projectID, _ := cmd.Flags().GetString("project-id")
			typ, _ := cmd.Flags().GetString("type")
			if projectID == "" || typ == "" {
				return nil, cliexit.Newf(cliexit.Usage, "ledger append: --project-id and --type are required")
			}
			content, _ := cmd.Flags().GetString("content")
			if pf.Stdin {
				if content != "" {
					return nil, cliexit.Newf(cliexit.Usage, "ledger append: --stdin and --content are mutually exclusive")
				}
				body, err := readBodyFromStdin()
				if err != nil {
					return nil, err
				}
				content = body
			}
			out := map[string]any{
				"project_id": projectID,
				"type":       typ,
			}
			if content != "" {
				out["content"] = content
			}
			if v, _ := cmd.Flags().GetString("story-id"); v != "" {
				out["story_id"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
				out["tags"] = v
			}
			if v, _ := cmd.Flags().GetString("durability"); v != "" {
				out["durability"] = v
			}
			if v, _ := cmd.Flags().GetString("source-type"); v != "" {
				out["source_type"] = v
			}
			if v, _ := cmd.Flags().GetString("structured"); v != "" {
				out["structured"] = v
			}
			if v, _ := cmd.Flags().GetString("expires-at"); v != "" {
				out["expires_at"] = v
			}
			if v, _ := cmd.Flags().GetBool("sensitive"); v {
				out["sensitive"] = true
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope (required).")
	c.Flags().String("type", "", "Event type (required).")
	c.Flags().String("content", "", "Event content / markdown body.")
	c.Flags().String("story-id", "", "Owning story id.")
	c.Flags().StringSlice("tags", nil, "Free-form tags.")
	c.Flags().String("durability", "", "ephemeral | pipeline | durable.")
	c.Flags().String("source-type", "", "manifest | feedback | agent | user | system | migration.")
	c.Flags().String("structured", "", "Type-specific JSON payload (raw string).")
	c.Flags().String("expires-at", "", "RFC3339 timestamp; required when durability=ephemeral.")
	c.Flags().Bool("sensitive", false, "Mark the row as sensitive (visible only to author).")
	_ = c.MarkFlagRequired("project-id")
	_ = c.MarkFlagRequired("type")
	return c
}

// story update-status + story field-set CLI subcommands were removed
// in sty_4db0e025 slice D1. Their flags fold into `satellites-client
// story update --id <id> --status <s>` and `satellites-client story
// update --id <id> --fields '<json>'` against the consolidated
// story_update verb. pr_story_terminal_gate continues to fire from the
// substrate when status moves to done/cancelled with open tasks.

// project set
func newProjectSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set",
		Short: "Bind the session to the project that owns the git remote.",
		RunE: writeHandler("project_set", func(cmd *cobra.Command, args []string) (any, error) {
			repoURL, _ := cmd.Flags().GetString("repo-url")
			if repoURL == "" {
				return nil, cliexit.Newf(cliexit.Usage, "project set: --repo-url is required")
			}
			out := map[string]any{"repo_url": repoURL}
			if v, _ := cmd.Flags().GetString("session-id"); v != "" {
				out["session_id"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("repo-url", "", "Git remote URL (required).")
	c.Flags().String("session-id", "", "Explicit session id (otherwise from header).")
	_ = c.MarkFlagRequired("repo-url")
	return c
}

// _ silences unused-import warnings for fmt during scaffold edits.
// Removed once a verb here uses fmt directly.
var _ = fmt.Sprint

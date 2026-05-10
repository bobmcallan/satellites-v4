package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliio"
)

// callRemote is the per-handler boilerplate: ensures the remote
// client is wired, runs the call with cobra's Context, marshals the
// result through cliio. Returns typed errors.
func callRemote(cmd *cobra.Command, toolName string, args any) (json.RawMessage, error) {
	c, err := ensureRemote()
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.Call(cmd.Context(), toolName, args, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// renderResponse writes the JSON response to stdout, applying the
// optional --select projection. --compact projection is per-noun and
// applied by the caller before this. ModeJSON is the default for any
// non-tty stdout per cliio.Resolve.
func renderResponse(raw json.RawMessage) error {
	payload := any(json.RawMessage(raw))
	if pf.Select != "" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil {
			if v, ok := obj[pf.Select]; ok {
				payload = v
			} else {
				return cliexit.Newf(cliexit.NotFound, "--select: field %q not present in response", pf.Select)
			}
		}
	}
	if resolvedMode.IsJSON() {
		return cliio.RenderJSON(os.Stdout, payload)
	}
	return cliio.RenderJSONIndent(os.Stdout, payload)
}

// readHandler builds a Cobra RunE that calls toolName with the args
// builder. argsFn typically reads cobra flags + positional args.
func readHandler(toolName string, argsFn func(cmd *cobra.Command, args []string) (any, error)) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		argv, err := argsFn(cmd, args)
		if err != nil {
			return err
		}
		raw, err := callRemote(cmd, toolName, argv)
		if err != nil {
			return err
		}
		return renderResponse(raw)
	}
}

// info — `satellites_info`. No args.
func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Return server identity + version metadata.",
		RunE:  readHandler("satellites_info", func(cmd *cobra.Command, args []string) (any, error) { return map[string]any{}, nil }),
	}
}

// session whoami.
func newSessionWhoamiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "whoami",
		Short: "Return the caller's registered session row.",
		RunE: readHandler("session_whoami", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if id, _ := cmd.Flags().GetString("session-id"); id != "" {
				out["session_id"] = id
			}
			return out, nil
		}),
	}
	c.Flags().String("session-id", "", "Session id (otherwise read from Mcp-Session-Id header).")
	return c
}

// task get.
func newTaskGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <task_id>",
		Short: "Get a task by id.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("task_get", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

// task walk.
func newTaskWalkCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "walk",
		Short: "Return the story's task chain orientation.",
		RunE: readHandler("task_walk", func(cmd *cobra.Command, args []string) (any, error) {
			storyID, err := cmd.Flags().GetString("story-id")
			if err != nil || storyID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task walk: --story-id is required")
			}
			return map[string]any{"story_id": storyID}, nil
		}),
	}
	c.Flags().String("story-id", "", "Owning story id (required).")
	_ = c.MarkFlagRequired("story-id")
	return c
}

// story get — adds --compact projection.
func newStoryGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <story_id>",
		Short: "Get a story by id.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := map[string]any{"id": args[0]}
			raw, err := callRemote(cmd, "story_get", argv)
			if err != nil {
				return err
			}
			if pf.Compact {
				raw, err = projectStoryCompact(raw)
				if err != nil {
					return err
				}
			}
			return renderResponse(raw)
		},
	}
	return c
}

// projectStoryCompact reduces story_get's orientation bundle to the
// high-gravity field set declared in docs/cli-primary-design.md §3:
// id, title, status, priority, tags. Drops project / recent_evidence
// / agent_process / template / intent_body / principles. The bundle
// shape is documented in internal/mcpserver/story_get.go.
func projectStoryCompact(raw json.RawMessage) (json.RawMessage, error) {
	var bundle struct {
		Story struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			Status   string   `json:"status"`
			Priority string   `json:"priority"`
			Tags     []string `json:"tags"`
		} `json:"story"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, fmt.Errorf("decode story bundle: %w", err)
	}
	out := map[string]any{
		"id":       bundle.Story.ID,
		"title":    bundle.Story.Title,
		"status":   bundle.Story.Status,
		"priority": bundle.Story.Priority,
		"tags":     bundle.Story.Tags,
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// agent get — supports --id or --name.
func newAgentGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get",
		Short: "Get an agent doc by id (preferred) or name.",
		RunE: readHandler("agent_get", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			name, _ := cmd.Flags().GetString("name")
			projectID, _ := cmd.Flags().GetString("project-id")
			if id == "" && name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "agent get: --id or --name is required")
			}
			out := map[string]any{}
			if id != "" {
				out["id"] = id
			}
			if name != "" {
				out["name"] = name
			}
			if projectID != "" {
				out["project_id"] = projectID
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Document id (preferred).")
	c.Flags().String("name", "", "Document name (used when id is omitted).")
	c.Flags().String("project-id", "", "Project scope for name-keyed lookups.")
	return c
}

// contract get — same shape as agent get.
func newContractGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get",
		Short: "Get a contract doc by id (preferred) or name.",
		RunE: readHandler("contract_get", func(cmd *cobra.Command, args []string) (any, error) {
			id, _ := cmd.Flags().GetString("id")
			name, _ := cmd.Flags().GetString("name")
			projectID, _ := cmd.Flags().GetString("project-id")
			if id == "" && name == "" {
				return nil, cliexit.Newf(cliexit.Usage, "contract get: --id or --name is required")
			}
			out := map[string]any{}
			if id != "" {
				out["id"] = id
			}
			if name != "" {
				out["name"] = name
			}
			if projectID != "" {
				out["project_id"] = projectID
			}
			return out, nil
		}),
	}
	c.Flags().String("id", "", "Document id (preferred).")
	c.Flags().String("name", "", "Document name (used when id is omitted).")
	c.Flags().String("project-id", "", "Project scope for name-keyed lookups.")
	return c
}

// principle list — supports --active-only + --project-id.
func newPrincipleListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List principles.",
		RunE: readHandler("principle_list", func(cmd *cobra.Command, args []string) (any, error) {
			out := map[string]any{}
			if v, _ := cmd.Flags().GetString("scope"); v != "" {
				out["scope"] = v
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetBool("active-only"); v {
				out["active_only"] = true
			}
			return out, nil
		}),
	}
	c.Flags().String("scope", "", "Filter by scope (system / workspace / project).")
	c.Flags().String("project-id", "", "Filter by project.")
	c.Flags().Bool("active-only", false, "Return only active principles.")
	return c
}

// ledger get — single id.
func newLedgerGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <ledger_id>",
		Short: "Get a ledger row by id.",
		Args:  cobra.ExactArgs(1),
		RunE: readHandler("ledger_get", func(cmd *cobra.Command, args []string) (any, error) {
			return map[string]any{"id": args[0]}, nil
		}),
	}
	return c
}

// ledger list — paginated, scoped by story_id / type / tags.
func newLedgerListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List ledger rows newest-first.",
		RunE: readHandler("ledger_list", func(cmd *cobra.Command, args []string) (any, error) {
			projectID, _ := cmd.Flags().GetString("project-id")
			if projectID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "ledger list: --project-id is required")
			}
			out := map[string]any{"project_id": projectID}
			if v, _ := cmd.Flags().GetString("story-id"); v != "" {
				out["story_id"] = v
			}
			if v, _ := cmd.Flags().GetString("type"); v != "" {
				out["type"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
				out["tags"] = v
			}
			if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
				out["limit"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope (required).")
	c.Flags().String("story-id", "", "Filter by story id.")
	c.Flags().String("type", "", "Filter by ledger type (architecture.md §6 enum).")
	c.Flags().StringSlice("tags", nil, "Filter by tags (any-of).")
	c.Flags().Int("limit", 0, "Max rows to return.")
	_ = c.MarkFlagRequired("project-id")
	return c
}

// ledger search — substring + structured filters.
func newLedgerSearchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "search",
		Short: "Search ledger rows.",
		RunE: readHandler("ledger_search", func(cmd *cobra.Command, args []string) (any, error) {
			projectID, _ := cmd.Flags().GetString("project-id")
			if projectID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "ledger search: --project-id is required")
			}
			out := map[string]any{"project_id": projectID}
			if v, _ := cmd.Flags().GetString("query"); v != "" {
				out["query"] = v
			}
			if v, _ := cmd.Flags().GetString("story-id"); v != "" {
				out["story_id"] = v
			}
			if v, _ := cmd.Flags().GetStringSlice("tags"); len(v) > 0 {
				out["tags"] = v
			}
			if v, _ := cmd.Flags().GetInt("top-k"); v > 0 {
				out["top_k"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("project-id", "", "Project scope (required).")
	c.Flags().String("query", "", "Free-text query.")
	c.Flags().String("story-id", "", "Filter by story id.")
	c.Flags().StringSlice("tags", nil, "Filter by tags (any-of).")
	c.Flags().Int("top-k", 0, "Max rows (default 20, capped 100).")
	_ = c.MarkFlagRequired("project-id")
	return c
}

// _ ensures context import is referenced even if no helper above
// uses it directly. Removed when the file's first context-using
// helper lands.
var _ = context.Background

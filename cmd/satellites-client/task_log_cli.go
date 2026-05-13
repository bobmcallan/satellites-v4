package main

import (
	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// newTaskLogAppendCmd registers `task log-append`. Operator-facing
// helper for the task_log_append verb — the typical caller is
// `satellites-client task run` itself (see task_run.go), but exposing
// the verb on the CLI keeps the wire shape testable and matches the
// existing `<noun> <verb>` pattern. sty_8c17b89d.
func newTaskLogAppendCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "log-append",
		Short: "Append a task_log row (lifecycle event or chunk).",
		RunE: writeHandler("task_log_append", func(cmd *cobra.Command, args []string) (any, error) {
			taskID, _ := cmd.Flags().GetString("task-id")
			if taskID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task log-append: --task-id is required")
			}
			kind, _ := cmd.Flags().GetString("kind")
			if kind == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task log-append: --kind is required")
			}
			seq, _ := cmd.Flags().GetInt64("seq")
			out := map[string]any{
				"task_id": taskID,
				"seq":     seq,
				"kind":    kind,
			}
			if v, _ := cmd.Flags().GetString("workspace-id"); v != "" {
				out["workspace_id"] = v
			}
			if v, _ := cmd.Flags().GetString("project-id"); v != "" {
				out["project_id"] = v
			}
			if v, _ := cmd.Flags().GetString("ts"); v != "" {
				out["ts"] = v
			}
			if v, _ := cmd.Flags().GetString("payload"); v != "" {
				out["payload"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("task-id", "", "Task id the row belongs to (required).")
	c.Flags().Int64("seq", 0, "Producer-supplied monotonic sequence (required).")
	c.Flags().String("kind", "", "One of start | heartbeat | stdout | stderr | stop (required).")
	c.Flags().String("workspace-id", "", "Workspace scope. Defaults to the task's workspace.")
	c.Flags().String("project-id", "", "Project scope.")
	c.Flags().String("ts", "", "RFC3339Nano timestamp. Defaults to now.")
	c.Flags().String("payload", "", "Opaque JSON payload, shape varies per kind.")
	_ = c.MarkFlagRequired("task-id")
	_ = c.MarkFlagRequired("kind")
	return c
}

// newTaskLogListCmd registers `task log-list`. sty_8c17b89d.
func newTaskLogListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "log-list",
		Short: "List task_log rows for a task ordered by seq.",
		RunE: readHandler("task_log_list", func(cmd *cobra.Command, args []string) (any, error) {
			taskID, _ := cmd.Flags().GetString("task-id")
			if taskID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "task log-list: --task-id is required")
			}
			out := map[string]any{"task_id": taskID}
			if v, _ := cmd.Flags().GetInt64("from-seq"); v > 0 {
				out["from_seq"] = v
			}
			if v, _ := cmd.Flags().GetInt("limit"); v > 0 {
				out["limit"] = v
			}
			return out, nil
		}),
	}
	c.Flags().String("task-id", "", "Task id whose rows to list (required).")
	c.Flags().Int64("from-seq", 0, "Inclusive lower bound on seq.")
	c.Flags().Int("limit", 0, "Max rows to return.")
	_ = c.MarkFlagRequired("task-id")
	return c
}

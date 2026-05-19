package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// newTaskRenderPromptCmd registers `satellites-client task render-prompt`
// — the orchestrator-side inline prompt builder (sty_72e36256). The
// command writes the rendered markdown to stdout RAW (no JSON
// envelope) so callers can pipe it into `task add --stdin` directly.
// All markdown assembly lives in client.RenderTaskPrompt; this shim
// only parses flags, calls the wire verb, and writes the result.
func newTaskRenderPromptCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "render-prompt",
		Short: "Render a task's full context into a self-contained markdown prompt (sty_72e36256).",
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID, _ := cmd.Flags().GetString("task-id")
			action, _ := cmd.Flags().GetString("action")
			storyID, _ := cmd.Flags().GetString("story-id")
			if taskID == "" || action == "" || storyID == "" {
				return cliexit.Newf(cliexit.Usage, "task render-prompt: --task-id, --action, --story-id are required")
			}
			work, _ := cmd.Flags().GetString("work")
			fromStdin, _ := cmd.Flags().GetBool("stdin")
			if fromStdin {
				if work != "" {
					return cliexit.Newf(cliexit.Usage, "task render-prompt: --work and --stdin are mutually exclusive")
				}
				raw, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return cliexit.Wrap(cliexit.Usage, err)
				}
				work = string(raw)
			}
			argv := map[string]any{"task_id": taskID, "action": action, "story_id": storyID}
			if work != "" {
				argv["work"] = work
			}
			raw, err := callRemote(cmd, "task_render_prompt", argv)
			if err != nil {
				return err
			}
			var env struct {
				Prompt string `json:"prompt"`
				Error  string `json:"error,omitempty"`
			}
			if jerr := json.Unmarshal(raw, &env); jerr != nil {
				return cliexit.Wrap(cliexit.Server, jerr)
			}
			if env.Error != "" {
				return cliexit.Newf(cliexit.Server, "task_render_prompt: %s", env.Error)
			}
			out := cmd.OutOrStdout()
			if out == nil {
				out = os.Stdout
			}
			_, werr := io.WriteString(out, env.Prompt)
			return werr
		},
	}
	c.Flags().String("task-id", "", "Task whose context to render (required).")
	c.Flags().String("action", "", "Action of the form contract:<name> (required).")
	c.Flags().String("story-id", "", "Owning story id (required).")
	c.Flags().String("work", "", "Optional work body to thread under '## Your work'.")
	c.Flags().Bool("stdin", false, "Read the work body from stdin (mutually exclusive with --work).")
	return c
}

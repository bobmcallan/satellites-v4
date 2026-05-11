package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleStoryExportWalk implements story_export_walk: render a story's
// task chain as paste-ready markdown for PR descriptions, delivery
// reports, and stakeholder hand-offs. Reuses client.TaskWalk so the
// markdown matches the JSON walk verbatim. sty_a248f4df.
func (s *Server) handleStoryExportWalk(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.stories == nil || s.tasks == nil {
		return mcpgo.NewToolResultError("story_export_walk unavailable: story or task store missing"), nil
	}
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	format := req.GetString("format", "markdown")
	if format != "markdown" {
		return mcpgo.NewToolResultError(fmt.Sprintf("story_export_walk: unsupported format %q", format)), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)

	st, err := s.stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": "story_not_found"})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	walk, err := s.cli().TaskWalk(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.TaskWalkInput{StoryID: storyID, Memberships: memberships})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	content := renderWalkMarkdown(walk, st.Description, st.AcceptanceCriteria, st.Priority, st.Tags)
	body, _ := json.Marshal(map[string]any{
		"filename": slugifyStoryFilename(st.ID, st.Title) + "-walk.md",
		"content":  content,
		"format":   format,
	})
	return mcpgo.NewToolResultText(string(body)), nil
}

// renderWalkMarkdown formats the task-chain walk into the
// "process followed" markdown shape. Tasks are grouped by Action so
// loops collapse under one H2 ("## contract:develop ×3 (loop)") with
// per-task H3 entries inside. Deterministic for snapshot testing —
// the source tasks are already in CreatedAt order from buildTaskWalk.
func renderWalkMarkdown(walk client.TaskWalkOutput, description, ac, priority string, tags []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — %s\n", walk.Story.ID, walk.Story.Title)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "**Status:** %s", emptyDash(walk.Story.Status))
	if priority != "" {
		fmt.Fprintf(&sb, "   **Priority:** %s", priority)
	}
	if len(tags) > 0 {
		fmt.Fprintf(&sb, "   **Tags:** %s", strings.Join(tags, ", "))
	}
	sb.WriteString("\n\n")
	if strings.TrimSpace(ac) != "" {
		sb.WriteString("## Acceptance criteria\n\n")
		sb.WriteString(strings.TrimRight(ac, "\n"))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(description) != "" {
		sb.WriteString("## Description\n\n")
		sb.WriteString(strings.TrimRight(description, "\n"))
		sb.WriteString("\n\n")
	}
	sb.WriteString("---\n\n")

	// Group by Action; preserve first-seen order so the markdown reads
	// in workflow sequence.
	type group struct {
		action  string
		entries []client.TaskWalkTask
	}
	groups := []group{}
	groupIndex := map[string]int{}
	for _, t := range walk.Tasks {
		key := t.Action
		if key == "" {
			key = "(no action)"
		}
		idx, ok := groupIndex[key]
		if !ok {
			groupIndex[key] = len(groups)
			groups = append(groups, group{action: key, entries: []client.TaskWalkTask{t}})
			continue
		}
		groups[idx].entries = append(groups[idx].entries, t)
	}
	for _, g := range groups {
		header := fmt.Sprintf("## %s", g.action)
		if len(g.entries) > 1 {
			header += fmt.Sprintf(" ×%d (loop)", len(g.entries))
		}
		sb.WriteString(header)
		sb.WriteString("\n\n")
		for _, t := range g.entries {
			outcome := t.Outcome
			if outcome == "" {
				outcome = t.Status
			}
			label := fmt.Sprintf("### %s #%d   %s", g.action, t.Iteration, outcome)
			if t.Kind != "" {
				label += fmt.Sprintf(" (%s)", t.Kind)
			}
			sb.WriteString(label)
			if t.ClaimedAt != nil {
				fmt.Fprintf(&sb, "   %s", formatTimestamp(*t.ClaimedAt))
				if t.CompletedAt != nil {
					fmt.Fprintf(&sb, " → %s", formatTimestamp(*t.CompletedAt))
				}
			} else if t.CompletedAt != nil {
				fmt.Fprintf(&sb, "   closed %s", formatTimestamp(*t.CompletedAt))
			}
			if t.ClaimedBy != "" {
				fmt.Fprintf(&sb, "   by %s", t.ClaimedBy)
			} else if t.AgentID != "" {
				fmt.Fprintf(&sb, "   agent %s", t.AgentID)
			}
			sb.WriteString("\n")
			fmt.Fprintf(&sb, "- task: `%s`\n", t.ID)
			if t.Description != "" {
				fmt.Fprintf(&sb, "- description: %s\n", t.Description)
			}
			if t.PriorTaskID != "" {
				fmt.Fprintf(&sb, "- prior: `%s`\n", t.PriorTaskID)
			}
			if t.ParentTaskID != "" {
				fmt.Fprintf(&sb, "- parent: `%s`\n", t.ParentTaskID)
			}
			sb.WriteString("\n")
		}
	}
	if len(walk.ActionSummary) > 0 {
		sb.WriteString("---\n\n")
		sb.WriteString("## Action summary\n\n")
		for _, a := range walk.ActionSummary {
			fmt.Fprintf(&sb, "- %s — work %d (%d open / %d closed), review %d (%d open / %d closed), ledger rows %d\n",
				a.Action, a.WorkTotal, a.WorkOpen, a.WorkClosed,
				a.ReviewTotal, a.ReviewOpen, a.ReviewClosed, a.LedgerRowCount)
		}
	}
	return sb.String()
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04")
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// slugifyStoryFilename converts a story title into a kebab-case slug
// suitable for a filename. Falls back to the story id when the title
// reduces to an empty string.
func slugifyStoryFilename(id, title string) string {
	var sb strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(sb.String(), "-")
	if slug == "" {
		return id
	}
	if id != "" {
		return id + "-" + slug
	}
	return slug
}

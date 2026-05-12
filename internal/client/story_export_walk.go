// story_export_walk.go — typed StoryExportWalk method + the markdown
// rendering helpers it depends on. Moved out of mcpserver in sty_ef248ab2
// so /api/v1 can call the same renderer; mcpserver's handler now thin-
// forwards to this typed surface.

package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StoryExportWalkInput names a story to render as paste-ready markdown.
type StoryExportWalkInput struct {
	StoryID     string
	Format      string
	Memberships []string
}

// StoryExportWalkOutput pairs the rendered markdown with a slugified
// filename callers can drop into a PR.
type StoryExportWalkOutput struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
	Format   string `json:"format"`
}

// StoryExportWalk renders a story's task chain as paste-ready markdown.
// Reuses TaskWalk so the rendering matches the JSON walk verbatim.
func (c *Client) StoryExportWalk(ctx context.Context, caller Caller, in StoryExportWalkInput) (StoryExportWalkOutput, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil {
		return StoryExportWalkOutput{}, errors.New("story_export_walk unavailable: story or task store missing")
	}
	if in.StoryID == "" {
		return StoryExportWalkOutput{}, errors.New("story_id required")
	}
	format := in.Format
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" {
		return StoryExportWalkOutput{}, fmt.Errorf("story_export_walk: unsupported format %q", format)
	}
	st, err := c.deps.Stories.GetByID(ctx, in.StoryID, in.Memberships)
	if err != nil {
		return StoryExportWalkOutput{}, errors.New("story_not_found")
	}
	walk, err := c.TaskWalk(ctx, caller, TaskWalkInput{StoryID: in.StoryID, Memberships: in.Memberships})
	if err != nil {
		return StoryExportWalkOutput{}, err
	}
	content := RenderWalkMarkdown(walk, st.Description, st.AcceptanceCriteria, st.Priority, st.Tags)
	return StoryExportWalkOutput{
		Filename: SlugifyStoryFilename(st.ID, st.Title) + "-walk.md",
		Content:  content,
		Format:   format,
	}, nil
}

// RenderWalkMarkdown formats the task-chain walk into the
// "process followed" markdown shape. Tasks are grouped by Action so
// loops collapse under one H2 with per-task H3 entries inside.
// Deterministic for snapshot testing — the source tasks are already in
// CreatedAt order from buildTaskWalk.
func RenderWalkMarkdown(walk TaskWalkOutput, description, ac, priority string, tags []string) string {
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
	type group struct {
		action  string
		entries []TaskWalkTask
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
			groups = append(groups, group{action: key, entries: []TaskWalkTask{t}})
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
				fmt.Fprintf(&sb, "   %s", formatExportTimestamp(*t.ClaimedAt))
				if t.CompletedAt != nil {
					fmt.Fprintf(&sb, " → %s", formatExportTimestamp(*t.CompletedAt))
				}
			} else if t.CompletedAt != nil {
				fmt.Fprintf(&sb, "   closed %s", formatExportTimestamp(*t.CompletedAt))
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

func formatExportTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04")
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// SlugifyStoryFilename converts a story title into a kebab-case slug
// suitable for a filename. Falls back to the story id when the title
// reduces to an empty string.
func SlugifyStoryFilename(id, title string) string {
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

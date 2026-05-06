// Package mcpserver — story_context MCP verb (sty_509a46fa item 6).
//
// story_context is a single-roundtrip composer that returns the
// orientation bundle an agent needs when picking up work on a story:
// the story row (body / status / fields / tags), the owning project,
// recent ledger evidence, the resolved agent_process instruction
// markdown, and the category template (when one applies).
//
// Replaces the four-call stitch agents otherwise have to do via
// story_get + project_get + ledger_recall + task_walk.
package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
)

// storyContextView is the JSON-marshalled response shape for
// story_context. Each field is independently optional so that a
// missing project / template / ledger / docs store degrades a single
// section instead of failing the whole call.
type storyContextView struct {
	Story          story.Story          `json:"story"`
	Project        *project.Project     `json:"project,omitempty"`
	RecentEvidence []ledger.LedgerEntry `json:"recent_evidence,omitempty"`
	AgentProcess   string               `json:"agent_process,omitempty"`
	Template       *story.Template      `json:"template,omitempty"`
	// IntentBody and Principles mirror project_set / project_context
	// (sty_31d51494 layer 2) so a session that walked in via a story
	// id has the same orientation context an agent that bootstrapped
	// via project_set would. Empty when the project's intent
	// artifact is missing or no principles apply.
	IntentBody string           `json:"intent_body,omitempty"`
	Principles []PrincipleEntry `json:"principles,omitempty"`
}

// handleStoryContext implements `story_context`. Workspace-scoped via
// memberships; cross-workspace stories return story_not_found
// identical to story_get.
func (s *Server) handleStoryContext(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	if s.stories == nil {
		return mcpgo.NewToolResultError("story_context unavailable: story store not configured"), nil
	}
	caller, _ := UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	st, err := s.stories.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	// Owner check is project-scoped — same gate as story_get.
	if _, err := s.resolveProjectID(ctx, st.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}

	view := storyContextView{Story: st}

	if s.projects != nil {
		if p, err := s.projects.GetByID(ctx, st.ProjectID, memberships); err == nil {
			view.Project = &p
			// sty_31d51494 layer 2: include the orientation bundle so
			// a story-id-first session has the same context a
			// project_set-first session would.
			bundle := s.buildOrientation(ctx, p)
			view.IntentBody = bundle.IntentBody
			view.Principles = bundle.Principles
		}
	}

	if s.ledger != nil {
		entries, err := s.ledger.List(ctx, st.ProjectID, ledger.ListOptions{
			StoryID: st.ID,
			Limit:   recentEvidenceLimit,
		}, memberships)
		if err == nil && len(entries) > 0 {
			view.RecentEvidence = entries
		}
	}

	if s.docs != nil {
		// Pass nil memberships so the system-scope default resolves
		// regardless of the caller's workspace set, matching the boot
		// handshake's behaviour. Owner authorisation has already cleared
		// at resolveProjectID above.
		view.AgentProcess = agentprocess.Resolve(ctx, s.docs, st.ProjectID, nil)
	}

	if t, ok := s.loadStoryTemplate(ctx, st.Category); ok {
		view.Template = &t
	}

	body, _ := json.Marshal(view)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_context").
		Str("story_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// recentEvidenceLimit caps the recent_evidence section. Matches
// story_get's bound so the two views agree on what "recent" means.
const recentEvidenceLimit = 10

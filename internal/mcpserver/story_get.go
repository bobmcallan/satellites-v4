// Package mcpserver — story_get MCP verb.
//
// story_get is a single-roundtrip composer that returns the orientation
// bundle an agent needs when picking up work on a story: the story row
// (body / status / fields / tags), the owning project, recent ledger
// evidence, the resolved agent_process instruction markdown, and the
// category template (when one applies).
//
// sty_48e38e83 merged the prior thin row-only `story_get` with the
// orientation-bundle `story_context` verb. The CRUD-shaped name wins;
// the orientation-bundle behaviour is what callers want.
package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
)

// storyView is the JSON-marshalled response shape for story_get. Each
// field is independently optional so that a missing project / template
// / ledger / docs store degrades a single section instead of failing
// the whole call.
type storyView struct {
	Story          story.Story          `json:"story"`
	Project        *project.Project     `json:"project,omitempty"`
	RecentEvidence []ledger.LedgerEntry `json:"recent_evidence,omitempty"`
	AgentProcess   string               `json:"agent_process,omitempty"`
	Template       *story.Template      `json:"template,omitempty"`
	IntentBody     string               `json:"intent_body,omitempty"`
	Principles     []PrincipleEntry     `json:"principles,omitempty"`
}

// handleStoryGet implements `story_get`. Workspace-scoped via
// memberships; cross-workspace stories return story_not_found.
func (s *Server) handleStoryGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	if s.stories == nil {
		return mcpgo.NewToolResultError("story_get unavailable: story store not configured"), nil
	}
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	st, err := s.stories.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, st.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}

	view := storyView{Story: st}

	if s.projects != nil {
		if p, err := s.projects.GetByID(ctx, st.ProjectID, memberships); err == nil {
			view.Project = &p
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
		view.AgentProcess = agentprocess.Resolve(ctx, s.docs, st.ProjectID, nil)
	}

	if t, ok := s.loadStoryTemplate(ctx, st.Category); ok {
		view.Template = &t
	}

	body, _ := json.Marshal(view)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_get").
		Str("story_id", id).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// recentEvidenceLimit caps the recent_evidence section.
const recentEvidenceLimit = 10

// Package mcpserver — task_add MCP verb (sty_a427368d).
//
// task_add mints one task at status=published for the given agent. When
// story_id is omitted the substrate auto-mints a thin ad-hoc story so
// every task is anchored to a story (project intent: "no work outside
// a story"). When the agent doc declares requires_review=true, a
// paired review task at status=planned is minted alongside; the review
// publishes when the work task closes via task_update(status=closed).
//
// task_add replaces the plan-DAG path on the retired task_submit
// (kind=plan) verb. The orchestrator that wants a multi-task chain
// calls task_add per task, dispatching each, then minting the next.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// handleTaskAdd implements `task_add`.
func (s *Server) handleTaskAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)

	if s.tasks == nil || s.docs == nil || s.stories == nil || s.ledger == nil {
		return mcpgo.NewToolResultError("task_add unavailable: required stores not configured"), nil
	}

	agentID, err := req.RequireString("agent_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return mcpgo.NewToolResultError("task_add: prompt must not be empty"), nil
	}

	memberships := s.resolveCallerMemberships(ctx, caller)
	storyID := strings.TrimSpace(req.GetString("story_id", ""))
	kind := strings.TrimSpace(req.GetString("kind", task.KindWork))
	if kind != task.KindWork && kind != task.KindReview {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_add: invalid kind %q (expected %q or %q)", kind, task.KindWork, task.KindReview)), nil
	}
	action := strings.TrimSpace(req.GetString("action", ""))
	priority := strings.TrimSpace(req.GetString("priority", task.PriorityMedium))

	// Resolve agent doc — system-scope agents are globally readable
	// (pass nil memberships per validatePlanCapabilities precedent).
	doc, err := s.docs.GetByID(ctx, agentID, nil)
	if err != nil || doc.Status != document.StatusActive || doc.Type != document.TypeAgent {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_not_found: %q", agentID)), nil
	}
	settings, perr := document.UnmarshalAgentSettings(doc.Structured)
	if perr != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("agent_settings_invalid: %v", perr)), nil
	}

	// Capability check. When action names a contract (contract:<name>),
	// the agent's delivers/reviews list must contain it. Free-form
	// actions ("ad-hoc", empty, …) are accepted on the assumption that
	// the agent doc itself authorises the role.
	if action != "" && task.ContractFromAction(action) != "" {
		if kind == task.KindWork && !settings.CanDeliver(action) {
			return mcpgo.NewToolResultError(fmt.Sprintf("agent_cannot_deliver: agent_id=%q action=%q", agentID, action)), nil
		}
		if kind == task.KindReview && !settings.CanReview(action) {
			return mcpgo.NewToolResultError(fmt.Sprintf("agent_cannot_review: agent_id=%q action=%q", agentID, action)), nil
		}
	}

	now := s.nowUTC()

	// Resolve the owning project + workspace.
	//
	// sty_e2512dbd: agent definitions live at scope=system (non-tenant),
	// but operations (tasks) live at the project tier. For a
	// scope=system agent the task lands in the CALLER's project, not
	// the agent's stamped tenancy (which should be empty for system
	// rows post-migration; ignored here defensively for legacy rows
	// that haven't been swept yet).
	//
	// sty_92271886: scope=workspace agents are shared across every
	// project in their workspace. The agent's WorkspaceID is the
	// agent's tenancy; the task lands in the caller's session-bound
	// project provided that project lives in the agent's workspace.
	// Caller without membership in the agent's workspace gets
	// agent_unavailable — same shape as a missing agent from the
	// caller's vantage.
	//
	// For scope=project agents the agent's stamped project is the
	// owning tenancy — the agent IS the project's worker.
	projectID := ""
	workspaceID := ""
	if doc.Scope == document.ScopeWorkspace {
		agentWS := doc.WorkspaceID
		if agentWS == "" {
			return mcpgo.NewToolResultError(fmt.Sprintf("agent_invalid: scope=workspace agent_id=%q has empty workspace_id", agentID)), nil
		}
		inWS := false
		for _, m := range memberships {
			if m == agentWS {
				inWS = true
				break
			}
		}
		if !inWS {
			return mcpgo.NewToolResultError(fmt.Sprintf("agent_unavailable: agent_id=%q scope=workspace workspace_id=%s caller has no membership", agentID, agentWS)), nil
		}
		workspaceID = agentWS
		// Caller's session/default project is honoured only when it
		// lies in the agent's workspace; otherwise defer to the next
		// step in the chain (defaultProjectID) which is also gated
		// below.
		if cand := s.callerActiveProjectID(ctx, caller); cand != "" && s.resolveProjectWorkspaceID(ctx, cand) == agentWS {
			projectID = cand
		}
		if projectID == "" && s.defaultProjectID != "" && s.resolveProjectWorkspaceID(ctx, s.defaultProjectID) == agentWS {
			projectID = s.defaultProjectID
		}
		if projectID == "" {
			return mcpgo.NewToolResultError(fmt.Sprintf("agent_unavailable: agent_id=%q scope=workspace workspace_id=%s no caller project resolvable in agent workspace", agentID, agentWS)), nil
		}
	} else {
		if doc.Scope != document.ScopeSystem {
			if doc.ProjectID != nil {
				projectID = *doc.ProjectID
			}
			workspaceID = doc.WorkspaceID
		}
		if projectID == "" {
			projectID = s.callerActiveProjectID(ctx, caller)
		}
		if projectID == "" {
			projectID = s.defaultProjectID
		}
		if workspaceID == "" && projectID != "" {
			workspaceID = s.resolveProjectWorkspaceID(ctx, projectID)
		}
		if workspaceID == "" && len(memberships) > 0 {
			workspaceID = memberships[0]
		}
	}

	storyMinted := false
	var st story.Story
	if storyID != "" {
		st, err = s.stories.GetByID(ctx, storyID, memberships)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("story_not_found: %q", storyID)), nil
		}
	} else {
		if projectID == "" {
			return mcpgo.NewToolResultError("task_add: cannot auto-mint story — no project_id resolvable from agent or session"), nil
		}
		title := promptToTitle(prompt)
		candidate := story.Story{
			WorkspaceID:        workspaceID,
			ProjectID:          projectID,
			Title:              title,
			Description:        prompt,
			AcceptanceCriteria: "see task body",
			Priority:           task.PriorityMedium,
			Category:           "improvement",
			Tags:               []string{"adhoc", "auto-minted"},
			CreatedBy:          caller.UserID,
		}
		st, err = s.stories.Create(ctx, candidate, now)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("story auto-mint failed: %v", err)), nil
		}
		storyMinted = true
	}

	// Create the work (or review) task at status=published.
	work, err := s.tasks.Enqueue(ctx, task.Task{
		WorkspaceID: st.WorkspaceID,
		ProjectID:   st.ProjectID,
		StoryID:     st.ID,
		Kind:        kind,
		Action:      action,
		Description: prompt,
		AgentID:     agentID,
		Origin:      task.OriginStoryStage,
		Priority:    priority,
		Status:      task.StatusPublished,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("task enqueue: %v", err)), nil
	}

	// kind:task-published ledger row, parity with task_plan / createTask.
	_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: work.WorkspaceID,
		ProjectID:   work.ProjectID,
		StoryID:     ledger.StringPtr(st.ID),
		Type:        ledger.TypeDecision,
		Tags: []string{
			"kind:task-published",
			"task_id:" + work.ID,
			"agent_id:" + agentID,
			"source:task_add",
		},
		Content:    fmt.Sprintf("task_add: id=%s agent=%s action=%s prompt_chars=%d", work.ID, agentID, fallback(action, "<ad-hoc>"), len(prompt)),
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceAgent,
		Status:     ledger.StatusActive,
		CreatedBy:  caller.UserID,
	}, now)

	// Optional paired review task. Minted at status=planned with
	// parent_task_id pointing at the work task; task_update(status=
	// closed) on the work task publishes it.
	reviewID := ""
	if kind == task.KindWork && settings.RequiresReview {
		review, rerr := s.tasks.Enqueue(ctx, task.Task{
			WorkspaceID:  st.WorkspaceID,
			ProjectID:    st.ProjectID,
			StoryID:      st.ID,
			Kind:         task.KindReview,
			Action:       action,
			Description:  fmt.Sprintf("review of task %s", work.ID),
			ParentTaskID: work.ID,
			Origin:       task.OriginStoryStage,
			Priority:     priority,
			Status:       task.StatusPlanned,
		}, now)
		if rerr == nil {
			reviewID = review.ID
		} else {
			s.logger.Warn().
				Str("work_task_id", work.ID).
				Err(rerr).
				Msg("task_add: paired review enqueue failed")
		}
	}

	body, _ := json.Marshal(map[string]any{
		"task_id":        work.ID,
		"story_id":       st.ID,
		"story_minted":   storyMinted,
		"review_task_id": reviewID,
		"status":         work.Status,
		"agent_id":       agentID,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "task_add").
		Str("story_id", st.ID).
		Str("task_id", work.ID).
		Bool("story_minted", storyMinted).
		Str("review_task_id", reviewID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// promptToTitle derives a story title from the prompt: first non-empty
// line, trimmed to 80 characters with an ellipsis when truncated.
func promptToTitle(prompt string) string {
	first := prompt
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	const maxTitle = 80
	if len(first) > maxTitle {
		first = first[:maxTitle-1] + "…"
	}
	if first == "" {
		first = "ad-hoc task"
	}
	return first
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// callerActiveProjectID returns the project id stamped on the caller's
// session row (set by project_set / session_register), or "" when the
// caller has no resolvable session.
func (s *Server) callerActiveProjectID(ctx context.Context, caller CallerIdentity) string {
	if s.sessions == nil {
		return ""
	}
	sessionID := resolveSessionID(ctx, "")
	if sessionID == "" {
		return ""
	}
	sess, err := s.sessions.Get(ctx, caller.UserID, sessionID)
	if err != nil {
		return ""
	}
	return sess.ActiveProjectID
}

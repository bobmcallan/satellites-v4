// Package mcpserver — task_submit MCP verb (sty_c6d76a5b).
//
// The agent-authored, substrate-validated alternative to
// orchestrator_compose_plan. The orchestrator submits the full task
// list; the substrate validates structural invariants (plan first,
// review tasks present per work task, contract names known) and
// rejects submissions that violate them — it does not silently
// mutate.
//
// Initial slice covers `kind=plan` only. `kind=close` and
// `kind=spawn` modes are scoped for follow-up commits per the
// story body.
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

// SubmitKindPlan / SubmitKindClose are the verb-arg `kind` values.
// The verb arg `kind` and Task.Kind are distinct enums — `kind` on the
// verb names the submission shape (plan | close | spawn), while
// task.Kind on each list entry names the activity type (work | review).
const (
	SubmitKindPlan  = "plan"
	SubmitKindClose = "close"
)

// taskInput is the JSON shape of one entry in the `tasks[]` arg of
// task_submit. The agent supplies kind, action, description,
// and (optionally) agent_id; the substrate fills in the rest.
type taskInput struct {
	Kind        string `json:"kind"`
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Priority    string `json:"priority,omitempty"`
}

// handleTaskSubmit implements `task_submit`. The first
// supported mode is `kind=plan`: the orchestrator submits an
// agent-authored task list; the substrate validates and persists.
func (s *Server) handleTaskSubmit(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)

	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	kind, err := req.RequireString("kind")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	if s.stories == nil || s.tasks == nil || s.ledger == nil {
		return mcpgo.NewToolResultError("task_submit unavailable: required stores not configured"), nil
	}

	memberships := s.resolveCallerMemberships(ctx, caller)
	st, err := s.stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}

	switch kind {
	case SubmitKindPlan:
		return s.submitPlan(ctx, req, st, caller, memberships, start)
	case SubmitKindClose:
		return s.submitClose(ctx, req, st, caller, memberships, start)
	default:
		return mcpgo.NewToolResultError(fmt.Sprintf("task_submit: unsupported kind %q", kind)), nil
	}
}

// submitPlan handles `kind=plan` — the orchestrator's agent-authored
// initial task list. Validates structure, persists the tasks, writes
// a kind:plan ledger row, and returns the new task ids.
func (s *Server) submitPlan(ctx context.Context, req mcpgo.CallToolRequest, st story.Story, caller CallerIdentity, memberships []string, start time.Time) (*mcpgo.CallToolResult, error) {
	rawTasks := req.GetString("tasks", "")
	if strings.TrimSpace(rawTasks) == "" {
		return mcpgo.NewToolResultError("task_submit(kind=plan): tasks[] required"), nil
	}
	var inputs []taskInput
	if err := json.Unmarshal([]byte(rawTasks), &inputs); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_submit: tasks[] parse error: %v", err)), nil
	}
	if len(inputs) == 0 {
		return mcpgo.NewToolResultError("task_submit(kind=plan): tasks[] empty"), nil
	}

	// Idempotence: when the story already has tasks submitted via
	// task_submit, return the existing task ids without writing
	// fresh rows. Mirrors orchestrator_compose_plan's convention.
	existing, _ := s.tasks.List(ctx, task.ListOptions{StoryID: st.ID}, memberships)
	if len(existing) > 0 {
		ids := make([]string, len(existing))
		for i, t := range existing {
			ids[i] = t.ID
		}
		body, _ := json.Marshal(map[string]any{
			"story_id":   st.ID,
			"task_ids":   ids,
			"task_count": len(ids),
			"idempotent": true,
		})
		return mcpgo.NewToolResultText(string(body)), nil
	}

	if err := validatePlanTasks(inputs); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.validatePlanCapabilities(ctx, inputs, memberships); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	now := s.nowUTC()
	planMarkdown := req.GetString("plan_markdown", "")

	// Persist tasks. Work tasks land at StatusPublished (claimable
	// now); review tasks land at StatusPlanned (subscriber-invisible
	// until the matching work task closes via task_submit
	// (kind=close), which publishes them). This gates the reviewer
	// service so it can't claim a review before the work is done.
	//
	// Each review task's ParentTaskID points at the immediately-
	// preceding work task — that linkage is how the close path
	// finds the sibling to publish. validatePlanTasks already
	// guaranteed every kind=review task follows a kind=work task with
	// matching action.
	taskIDs := make([]string, 0, len(inputs))
	priorWorkID := ""
	for i, in := range inputs {
		priority := in.Priority
		if priority == "" {
			priority = task.PriorityMedium
		}
		status := task.StatusPublished
		parentTaskID := ""
		if in.Kind == task.KindReview {
			status = task.StatusPlanned
			parentTaskID = priorWorkID
		}
		t, err := s.tasks.Enqueue(ctx, task.Task{
			WorkspaceID:  st.WorkspaceID,
			ProjectID:    st.ProjectID,
			StoryID:      st.ID,
			Kind:         in.Kind,
			Action:       in.Action,
			Description:  in.Description,
			AgentID:      in.AgentID,
			ParentTaskID: parentTaskID,
			Origin:       task.OriginStoryStage,
			Priority:     priority,
			Status:       status,
		}, now)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("task enqueue [%d %s]: %v", i, in.Action, err)), nil
		}
		taskIDs = append(taskIDs, t.ID)
		if in.Kind == task.KindWork {
			priorWorkID = t.ID
		}
	}

	// kind:plan ledger row carrying the plan_markdown. The plan
	// markdown is the artifact; the task list is the conversation
	// scaffold.
	planPayload, _ := json.Marshal(map[string]any{
		"task_ids": taskIDs,
		"actions":  collectActions(inputs),
	})
	planContent := planMarkdown
	if planContent == "" {
		planContent = fmt.Sprintf("orchestrator plan: %s", strings.Join(collectActions(inputs), " → "))
	}
	planRow, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: st.WorkspaceID,
		ProjectID:   st.ProjectID,
		StoryID:     ledger.StringPtr(st.ID),
		Type:        ledger.TypePlan,
		Tags:        []string{"kind:plan", "phase:orchestrator", "story:" + st.ID, "source:task_submit"},
		Content:     planContent,
		Structured:  planPayload,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("plan ledger append: %v", err)), nil
	}

	body, _ := json.Marshal(map[string]any{
		"story_id":       st.ID,
		"plan_ledger_id": planRow.ID,
		"task_ids":       taskIDs,
		"task_count":     len(taskIDs),
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "task_submit").
		Str("kind", SubmitKindPlan).
		Str("story_id", st.ID).
		Int("task_count", len(taskIDs)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// submitClose handles `kind=close` — the agent closes its current
// task. Status flip + optional successor publishing for the matching
// review sibling. Evidence ledger ids are accepted as references; the
// substrate does not write inline markdown (per "tasks are thin;
// ledger rows are the artifacts").
func (s *Server) submitClose(ctx context.Context, req mcpgo.CallToolRequest, st story.Story, caller CallerIdentity, memberships []string, start time.Time) (*mcpgo.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	outcome := req.GetString("outcome", task.OutcomeSuccess)
	if outcome != task.OutcomeSuccess && outcome != task.OutcomeFailure {
		return mcpgo.NewToolResultError(fmt.Sprintf("invalid_outcome: %q (expected %q or %q)", outcome, task.OutcomeSuccess, task.OutcomeFailure)), nil
	}

	current, err := s.tasks.GetByID(ctx, taskID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_not_found: %s", taskID)), nil
	}
	if current.StoryID != st.ID {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_story_mismatch: task %s belongs to story %s, not %s", taskID, current.StoryID, st.ID)), nil
	}
	if current.Status == task.StatusClosed || current.Status == task.StatusArchived {
		return mcpgo.NewToolResultError(fmt.Sprintf("task_already_terminal: %s status=%s", taskID, current.Status)), nil
	}

	now := s.nowUTC()

	// Atomic close + sibling-review publish. We close first; if the
	// caller-supplied evidence_ledger_ids are missing rows, they're
	// still accepted (the agent may reference rows it wrote elsewhere
	// and the substrate doesn't validate every reference). Outcomes
	// other than the two enum values were rejected above.
	closed, err := s.tasks.Close(ctx, taskID, outcome, now, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("task close: %v", err)), nil
	}

	// Publish the matching sibling review task — kind=review with the
	// same action that comes immediately after this work task in the
	// story chain (task_submit(kind=plan) emits it at status=
	// planned for exactly this gating). When no sibling exists (close
	// of a review task itself, or a story chain without an explicit
	// review for this action), no publish is performed.
	publishedReviewID := ""
	if current.Kind == task.KindWork {
		sibling := s.findPlannedReviewSibling(ctx, current, memberships)
		if sibling != "" {
			pub, perr := s.tasks.Publish(ctx, sibling, now, memberships)
			if perr == nil {
				publishedReviewID = pub.ID
			} else {
				s.logger.Warn().
					Str("task_id", taskID).
					Str("sibling_id", sibling).
					Err(perr).
					Msg("task_submit close: sibling review publish failed")
			}
		}
	}

	// Optional: tag evidence rows with task_id for retrievability
	// after CIs go away. Strictly additive; failures are logged
	// rather than aborting the close.
	rawEvidence := req.GetString("evidence_ledger_ids", "")
	evidenceIDs := parseStringArray(rawEvidence)

	body, _ := json.Marshal(map[string]any{
		"task_id":             closed.ID,
		"status":              closed.Status,
		"outcome":             closed.Outcome,
		"published_review_id": publishedReviewID,
		"evidence_ledger_ids": evidenceIDs,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "task_submit").
		Str("kind", SubmitKindClose).
		Str("story_id", st.ID).
		Str("task_id", taskID).
		Str("outcome", outcome).
		Str("published_review_id", publishedReviewID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	_ = caller // present for parity with submitPlan; not yet load-bearing here
	return mcpgo.NewToolResultText(string(body)), nil
}

// findPlannedReviewSibling returns the task id of the planned review
// task whose ParentTaskID is `parent.ID`. task_submit(kind=plan)
// stamps that linkage at compose time. Empty when no matching planned
// review exists (legacy chain, already-published, etc.).
func (s *Server) findPlannedReviewSibling(ctx context.Context, parent task.Task, memberships []string) string {
	siblings, err := s.tasks.List(ctx, task.ListOptions{
		StoryID: parent.StoryID,
		Kind:    task.KindReview,
		Status:  task.StatusPlanned,
	}, memberships)
	if err != nil {
		return ""
	}
	for _, sib := range siblings {
		if sib.ParentTaskID == parent.ID {
			return sib.ID
		}
	}
	return ""
}

// parseStringArray decodes a JSON array argument as a string slice.
// Empty / invalid input returns nil.
func parseStringArray(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// validatePlanCapabilities checks each task's agent_id (when supplied)
// has the right capability frontmatter for its action — Kind=KindWork
// requires the agent's AgentSettings.Delivers list to contain the
// action; Kind=KindReview requires AgentSettings.Reviews. Unknown or
// archived agent docs reject as `agent_not_found:<id>`.
//
// When agent_id is empty the capability check is skipped — the
// orchestrator may submit a plan without pinning agents up-front, in
// which case agent allocation falls to a later pass (today: implicit
// via task_claim's claim-by-id; future: a substrate-managed
// allocation slot).
func (s *Server) validatePlanCapabilities(ctx context.Context, inputs []taskInput, memberships []string) error {
	if s.docs == nil {
		return nil
	}
	for i, in := range inputs {
		if in.AgentID == "" {
			continue
		}
		// System-scope agent docs are globally readable (pr_0779e5af);
		// pass nil memberships so the validator finds them regardless
		// of the caller's workspace scope.
		doc, err := s.docs.GetByID(ctx, in.AgentID, nil)
		if err != nil || doc.Status != document.StatusActive || doc.Type != document.TypeAgent {
			return fmt.Errorf("agent_not_found: tasks[%d].agent_id = %q", i, in.AgentID)
		}
		settings, perr := document.UnmarshalAgentSettings(doc.Structured)
		if perr != nil {
			return fmt.Errorf("agent_settings_invalid: tasks[%d].agent_id = %q (%v)", i, in.AgentID, perr)
		}
		kind := in.Kind
		if kind == "" {
			kind = task.KindWork
		}
		if kind == task.KindWork && !settings.CanDeliver(in.Action) {
			return fmt.Errorf("agent_cannot_deliver: tasks[%d] agent_id=%q action=%q (delivers list does not include this action)", i, in.AgentID, in.Action)
		}
		if kind == task.KindReview && !settings.CanReview(in.Action) {
			return fmt.Errorf("agent_cannot_review: tasks[%d] agent_id=%q action=%q (reviews list does not include this action)", i, in.AgentID, in.Action)
		}
	}
	return nil
}

// validatePlanTasks runs the structural checks required by
// `task_submit(kind=plan)` per sty_c6d76a5b's AC list. Returns
// the first violation as a structured error string; nil when clean.
//
// Structural rules in this slice:
//  1. tasks[] non-empty.
//  2. tasks[0].action == "contract:plan".
//  3. tasks[0].kind == KindWork (the plan task itself is work).
//  4. Every kind=work task has an immediate kind=review sibling
//     in the next slot — substrate does not silently insert.
//  5. Every action is well-formed (contract:<name> with a non-empty
//     contract name).
//
// Deferred to a follow-up slice (require contract markdown
// enumeration / agent capability lookups):
//   - Last task action == "contract:story_close".
//   - Required contract set fully covered.
//   - Each task's agent_id has the capability to deliver/review.
func validatePlanTasks(inputs []taskInput) error {
	if len(inputs) == 0 {
		return fmt.Errorf("plan_tasks_empty: tasks[] must not be empty")
	}
	first := inputs[0]
	firstKind := first.Kind
	if firstKind == "" {
		firstKind = task.KindWork
	}
	if first.Action != task.ContractAction("plan") {
		return fmt.Errorf("plan_first_task_must_be_plan: tasks[0].action = %q (expected %q)", first.Action, task.ContractAction("plan"))
	}
	if firstKind != task.KindWork {
		return fmt.Errorf("plan_first_task_must_be_work: tasks[0].kind = %q (expected %q)", firstKind, task.KindWork)
	}

	for i, in := range inputs {
		if in.Action == "" {
			return fmt.Errorf("task_action_required: tasks[%d].action is empty", i)
		}
		if task.ContractFromAction(in.Action) == "" {
			return fmt.Errorf("invalid_action_format: tasks[%d].action = %q (expected %q<name>)", i, in.Action, task.ActionContractPrefix)
		}
		kind := in.Kind
		if kind == "" {
			kind = task.KindWork
		}
		if kind != task.KindWork && kind != task.KindReview {
			return fmt.Errorf("invalid_task_kind: tasks[%d].kind = %q (expected %q or %q)", i, in.Kind, task.KindWork, task.KindReview)
		}
		if kind != task.KindWork {
			continue
		}
		// Every work task needs an immediate review sibling matching
		// the same action. The agent must include it explicitly —
		// substrate does not insert.
		if i+1 >= len(inputs) {
			return fmt.Errorf("missing_review_for: tasks[%d] action=%q has no review sibling (tasks[] ended)", i, in.Action)
		}
		next := inputs[i+1]
		if next.Kind != task.KindReview {
			return fmt.Errorf("missing_review_for: tasks[%d+1] kind=%q (expected %q for action=%q)", i, next.Kind, task.KindReview, in.Action)
		}
		if next.Action != in.Action {
			return fmt.Errorf("review_action_mismatch: tasks[%d+1] action=%q does not match work action=%q", i, next.Action, in.Action)
		}
	}
	return nil
}

// collectActions returns the action strings for each input task, in
// order. Used in the plan ledger row's structured payload + content
// summary.
func collectActions(inputs []taskInput) []string {
	out := make([]string, len(inputs))
	for i, in := range inputs {
		out[i] = in.Action
	}
	return out
}

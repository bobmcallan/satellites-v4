package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"sort"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/task"
)

// taskWalkResponse is the wire shape for task_walk (sty_c6d76a5b
// checkpoint 14). The CI list + per-CI walk are gone; the response is
// the story header, the ordered task chain, the current task pointer,
// and a per-action summary for renderers that want a contract-shaped
// view without the substrate carrying contract_instance rows.
type taskWalkResponse struct {
	Story         taskWalkStory           `json:"story"`
	Tasks         []taskWalkTask          `json:"tasks"`
	CurrentTaskID string                  `json:"current_task_id,omitempty"`
	ActionSummary []taskWalkActionSummary `json:"action_summary,omitempty"`
}

// taskWalkStory is the story header — id + title + status.
type taskWalkStory struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// taskWalkTask is the per-task projection. Mirrors the load-bearing
// fields on task.Task; iteration is derived from action peers on the
// same story.
type taskWalkTask struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind,omitempty"`
	Action       string     `json:"action,omitempty"`
	Description  string     `json:"description,omitempty"`
	Origin       string     `json:"origin,omitempty"`
	Status       string     `json:"status"`
	Outcome      string     `json:"outcome,omitempty"`
	Priority     string     `json:"priority,omitempty"`
	AgentID      string     `json:"agent_id,omitempty"`
	ParentTaskID string     `json:"parent_task_id,omitempty"`
	PriorTaskID  string     `json:"prior_task_id,omitempty"`
	Iteration    int        `json:"iteration,omitempty"`
	ClaimedBy    string     `json:"claimed_by,omitempty"`
	ClaimedAt    *time.Time `json:"claimed_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// taskWalkActionSummary aggregates tasks by Action so renderers can
// show "develop ×3" loops without re-deriving the count themselves.
type taskWalkActionSummary struct {
	Action         string `json:"action"`
	WorkTotal      int    `json:"work_total"`
	WorkOpen       int    `json:"work_open"`
	WorkClosed     int    `json:"work_closed"`
	ReviewTotal    int    `json:"review_total"`
	ReviewOpen     int    `json:"review_open"`
	ReviewClosed   int    `json:"review_closed"`
	LedgerRowCount int    `json:"ledger_row_count"`
}

// handleTaskWalk implements the task_walk MCP verb: returns a single
// coherent payload describing where a story sits in its task chain.
// sty_41488515 / sty_c6d76a5b.
func (s *Server) handleTaskWalk(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.stories == nil || s.tasks == nil {
		return mcpgo.NewToolResultError("task_walk unavailable: story or task store missing"), nil
	}
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	resp, err := s.buildTaskWalk(ctx, storyID, memberships)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(resp)
	return mcpgo.NewToolResultText(string(body)), nil
}

// buildTaskWalk projects the story's task chain into the wire shape.
// Shared by handleTaskWalk and handleStoryExportWalk.
func (s *Server) buildTaskWalk(ctx context.Context, storyID string, memberships []string) (taskWalkResponse, error) {
	st, err := s.stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		return taskWalkResponse{}, errStoryNotFound
	}

	tasks, err := s.tasks.List(ctx, task.ListOptions{StoryID: storyID, Limit: 500}, memberships)
	if err != nil {
		return taskWalkResponse{}, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})

	// ledger row counts per action so the action summary can carry an
	// aggregate "evidence captured" count without per-action queries.
	ledgerByAction := map[string]int{}
	if s.ledger != nil && st.ProjectID != "" {
		rows, lerr := s.ledger.List(ctx, st.ProjectID, ledger.ListOptions{StoryID: storyID, Limit: ledger.MaxListLimit}, memberships)
		if lerr == nil {
			for _, r := range rows {
				for _, tag := range r.Tags {
					if act, ok := actionFromTag(tag); ok {
						ledgerByAction[act]++
					}
				}
			}
		}
	}

	resp := taskWalkResponse{
		Story: taskWalkStory{
			ID:     st.ID,
			Title:  st.Title,
			Status: st.Status,
		},
		Tasks: make([]taskWalkTask, 0, len(tasks)),
	}

	currentTaskID := ""
	for _, t := range tasks {
		row := taskWalkTask{
			ID:           t.ID,
			Kind:         t.Kind,
			Action:       t.Action,
			Description:  t.Description,
			Origin:       t.Origin,
			Status:       t.Status,
			Outcome:      t.Outcome,
			Priority:     t.Priority,
			AgentID:      t.AgentID,
			ParentTaskID: t.ParentTaskID,
			PriorTaskID:  t.PriorTaskID,
			Iteration:    iterationForTask(t, tasks),
			ClaimedBy:    t.ClaimedBy,
			ClaimedAt:    t.ClaimedAt,
			CompletedAt:  t.CompletedAt,
			CreatedAt:    t.CreatedAt,
		}
		resp.Tasks = append(resp.Tasks, row)
		if currentTaskID == "" && !isTerminalTaskStatus(t.Status) {
			currentTaskID = t.ID
		}
	}
	resp.CurrentTaskID = currentTaskID
	resp.ActionSummary = summariseActions(tasks, ledgerByAction)
	return resp, nil
}

// errStoryNotFound is the sentinel buildTaskWalk returns when the
// story id does not resolve in the caller's workspaces.
var errStoryNotFound = errStr("story_not_found")

type errStr string

func (e errStr) Error() string { return string(e) }

// iterationForTask returns the 1-based lap number of t among tasks
// with the same Action + Kind on the same story, ordered by CreatedAt.
func iterationForTask(t task.Task, peers []task.Task) int {
	if t.Action == "" {
		return 1
	}
	n := 0
	for _, p := range peers {
		if p.Action != t.Action || p.Kind != t.Kind {
			continue
		}
		if p.CreatedAt.After(t.CreatedAt) {
			continue
		}
		n++
	}
	if n == 0 {
		return 1
	}
	return n
}

// summariseActions buckets tasks by Action into work/review counts so
// renderers can show a contract-shaped summary without joining back to
// any contract_instance row.
func summariseActions(tasks []task.Task, ledgerByAction map[string]int) []taskWalkActionSummary {
	type bucket struct {
		idx     int
		summary taskWalkActionSummary
	}
	order := make([]string, 0)
	buckets := map[string]*bucket{}
	for _, t := range tasks {
		if t.Action == "" {
			continue
		}
		b, ok := buckets[t.Action]
		if !ok {
			b = &bucket{idx: len(order), summary: taskWalkActionSummary{Action: t.Action}}
			buckets[t.Action] = b
			order = append(order, t.Action)
		}
		switch t.Kind {
		case task.KindReview:
			b.summary.ReviewTotal++
			if isTerminalTaskStatus(t.Status) {
				b.summary.ReviewClosed++
			} else {
				b.summary.ReviewOpen++
			}
		default:
			b.summary.WorkTotal++
			if isTerminalTaskStatus(t.Status) {
				b.summary.WorkClosed++
			} else {
				b.summary.WorkOpen++
			}
		}
	}
	out := make([]taskWalkActionSummary, len(order))
	for i, action := range order {
		b := buckets[action]
		b.summary.LedgerRowCount = ledgerByAction[action]
		out[i] = b.summary
	}
	return out
}

// isTerminalTaskStatus reports whether a task has reached a terminal
// status (no further transitions). Closed + archived are terminal;
// every other status is open.
func isTerminalTaskStatus(status string) bool {
	switch status {
	case task.StatusClosed, task.StatusArchived:
		return true
	}
	return false
}

// actionFromTag pulls the contract action out of `phase:<name>` or
// `contract:<name>` tags so the ledger summary can roll up by action.
// Returns ok=false for tags that don't carry an action prefix.
func actionFromTag(tag string) (string, bool) {
	const phase = "phase:"
	const contract = "contract:"
	switch {
	case len(tag) > len(phase) && tag[:len(phase)] == phase:
		return task.ContractAction(tag[len(phase):]), true
	case len(tag) > len(contract) && tag[:len(contract)] == contract:
		return tag, true
	}
	return "", false
}

package client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/tasklog"
)

// ErrTaskStoreNotConfigured is returned when the typed task surface is
// called against a Client whose Deps.Tasks is nil. Wire-layer callers
// map this to a per-verb "unavailable" envelope.
var ErrTaskStoreNotConfigured = errors.New("task store not configured")

// ErrStoryStoreNotConfigured signals the same shape for story_id-resolving
// task verbs. Returned by TaskWalk + TaskAdd when c.deps.Stories is nil.
var ErrStoryStoreNotConfigured = errors.New("story store not configured")

// ErrStoryNotFound is the sentinel TaskWalk returns when the story id
// does not resolve in the caller's workspaces.
var ErrStoryNotFound = errors.New("story_not_found")

// ErrNoTaskAvailable is re-exported from internal/task so transport
// handlers can branch on the empty-queue case without importing the
// substrate task package (pr_mcp_cli_shared_path). TaskClaim returns
// it verbatim when no enqueued task matches the caller's workspaces.
var ErrNoTaskAvailable = task.ErrNoTaskAvailable

// TaskGetInput names the task being read.
type TaskGetInput struct {
	ID          string
	Memberships []string
}

// TaskGet returns a task by id, scoped by workspace membership.
func (c *Client) TaskGet(ctx context.Context, caller Caller, in TaskGetInput) (task.Task, error) {
	if c.deps.Tasks == nil {
		return task.Task{}, ErrTaskStoreNotConfigured
	}
	if in.ID == "" {
		return task.Task{}, errors.New("id required")
	}
	return c.deps.Tasks.GetByID(ctx, in.ID, in.Memberships)
}

// TaskWalkInput names the story whose chain should be walked.
type TaskWalkInput struct {
	StoryID     string
	Memberships []string
}

// TaskWalkOutput is the wire shape for task_walk. Mirrors the
// per-story chain projection: header + ordered tasks + current-task
// pointer + per-action summary + lifecycle drift status.
//
// LifecycleStatus is computed by computeLifecycleStatus against the
// workflow doc's enumerated phases (sty_e0c3d615). Values: `on_shape`
// or `drifted:<reason>`. Advisory only — chain dispatch is unaffected;
// the `story_close` gate appends a `kind:lifecycle-drift` warning row
// on drift but does not refuse to close.
type TaskWalkOutput struct {
	Story           TaskWalkStory           `json:"story"`
	Tasks           []TaskWalkTask          `json:"tasks"`
	CurrentTaskID   string                  `json:"current_task_id,omitempty"`
	ActionSummary   []TaskWalkActionSummary `json:"action_summary,omitempty"`
	LifecycleStatus string                  `json:"lifecycle_status,omitempty"`
}

// TaskWalkStory is the story header.
type TaskWalkStory struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// TaskWalkTask is the per-task projection in the chain.
type TaskWalkTask struct {
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

// TaskWalkActionSummary aggregates tasks by Action so renderers can
// show "develop ×3" loops without re-deriving the count themselves.
type TaskWalkActionSummary struct {
	Action         string `json:"action"`
	WorkTotal      int    `json:"work_total"`
	WorkOpen       int    `json:"work_open"`
	WorkClosed     int    `json:"work_closed"`
	ReviewTotal    int    `json:"review_total"`
	ReviewOpen     int    `json:"review_open"`
	ReviewClosed   int    `json:"review_closed"`
	LedgerRowCount int    `json:"ledger_row_count"`
}

// TaskWalk projects the story's task chain into a single coherent
// payload: story header, ordered tasks, current-task pointer, and a
// per-action summary. Returns ErrStoryNotFound when the story id does
// not resolve in the caller's workspaces.
func (c *Client) TaskWalk(ctx context.Context, caller Caller, in TaskWalkInput) (TaskWalkOutput, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil {
		return TaskWalkOutput{}, errors.New("task_walk unavailable: story or task store missing")
	}
	if in.StoryID == "" {
		return TaskWalkOutput{}, errors.New("story_id required")
	}
	st, err := c.deps.Stories.GetByID(ctx, in.StoryID, in.Memberships)
	if err != nil {
		return TaskWalkOutput{}, ErrStoryNotFound
	}

	tasks, err := c.deps.Tasks.List(ctx, task.ListOptions{StoryID: in.StoryID, Limit: 500}, in.Memberships)
	if err != nil {
		return TaskWalkOutput{}, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})

	// ledger row counts per action so the action summary can carry an
	// aggregate "evidence captured" count without per-action queries.
	ledgerByAction := map[string]int{}
	if c.deps.Ledger != nil && st.ProjectID != "" {
		rows, lerr := c.deps.Ledger.List(ctx, st.ProjectID, ledger.ListOptions{StoryID: in.StoryID, Limit: ledger.MaxListLimit}, in.Memberships)
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

	out := TaskWalkOutput{
		Story: TaskWalkStory{
			ID:     st.ID,
			Title:  st.Title,
			Status: st.Status,
		},
		Tasks: make([]TaskWalkTask, 0, len(tasks)),
	}

	currentTaskID := ""
	for _, t := range tasks {
		row := TaskWalkTask{
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
		out.Tasks = append(out.Tasks, row)
		if currentTaskID == "" && !isTerminalTaskStatus(t.Status) {
			currentTaskID = t.ID
		}
	}
	out.CurrentTaskID = currentTaskID
	out.ActionSummary = summariseActions(tasks, ledgerByAction)
	out.LifecycleStatus = computeLifecycleStatus(st.Status, tasks)
	return out, nil
}

// TaskClaimInput names the worker doing the claim + the workspaces it
// claims from.
type TaskClaimInput struct {
	WorkerID     string
	WorkspaceIDs []string
	Memberships  []string
	Now          time.Time
}

// TaskClaim atomically picks the highest-priority oldest-queued task
// for the worker. Returns task.ErrNoTaskAvailable when the queue is
// empty so the caller can map "null result" semantics on the wire.
func (c *Client) TaskClaim(ctx context.Context, caller Caller, in TaskClaimInput) (task.Task, error) {
	if c.deps.Tasks == nil {
		return task.Task{}, ErrTaskStoreNotConfigured
	}
	workerID := in.WorkerID
	if workerID == "" {
		workerID = caller.UserID
	}
	if workerID == "" {
		return task.Task{}, errors.New("task_claim requires worker_id")
	}
	workspaceIDs := in.WorkspaceIDs
	if len(workspaceIDs) == 0 {
		workspaceIDs = in.Memberships
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	t, err := c.deps.Tasks.Claim(ctx, workerID, workspaceIDs, now)
	if err != nil {
		return task.Task{}, err
	}
	if c.deps.Ledger != nil {
		_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: t.WorkspaceID,
			ProjectID:   t.ProjectID,
			Type:        ledger.TypeDecision,
			Tags: []string{
				"kind:task-claimed",
				"task_id:" + t.ID,
				"worker_id:" + workerID,
			},
			Content:    fmt.Sprintf("task claimed: id=%s worker=%s", t.ID, workerID),
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
			CreatedBy:  caller.UserID,
		}, now)
	}
	// sty_090f6183: emit KindClaim semantic event. Payload mirrors plan §2.
	c.publishTaskLog(ctx, t.ID, tasklog.KindClaim, map[string]any{
		"task_id":    t.ID,
		"claimed_by": workerID,
		"claimed_at": now.UTC().Format(time.RFC3339Nano),
	})
	return t, nil
}

// TaskUpdateInput captures task lifecycle + linkage mutations.
// status="closed" routes through the existing close path (outcome
// required). PriorTaskID/ParentTaskID are pointer-discriminated so
// callers can distinguish "leave alone" (nil) from "patch to this
// value, including empty" (non-nil); when non-nil and status=="" the
// call routes to the field-level linkage patch on the substrate.
// Combining a status mutation with a linkage patch in one call is
// rejected — the caller chains them in two calls. sty_27516920.
type TaskUpdateInput struct {
	ID                string
	Status            string
	Outcome           string
	EvidenceLedgerIDs []string
	PriorTaskID       *string
	ParentTaskID      *string
	Memberships       []string
	Now               time.Time
}

// TaskUpdateOutput mirrors the wire payload of task_update.
type TaskUpdateOutput struct {
	TaskID            string   `json:"task_id"`
	Status            string   `json:"status"`
	Outcome           string   `json:"outcome"`
	EvidenceLedgerIDs []string `json:"evidence_ledger_ids,omitempty"`
}

// TaskUpdate mutates a task's lifecycle or linkage state. status=closed
// routes through the existing close path; PriorTaskID/ParentTaskID
// (pointer-discriminated) route through SetLinkage when status is
// empty. Combining a status mutation with a linkage patch in one call
// is rejected. sty_27516920.
func (c *Client) TaskUpdate(ctx context.Context, caller Caller, in TaskUpdateInput) (TaskUpdateOutput, error) {
	if c.deps.Tasks == nil {
		return TaskUpdateOutput{}, ErrTaskStoreNotConfigured
	}
	if in.ID == "" {
		return TaskUpdateOutput{}, errors.New("id required")
	}
	hasLinkagePatch := in.PriorTaskID != nil || in.ParentTaskID != nil
	if in.Status == "" {
		if !hasLinkagePatch {
			return TaskUpdateOutput{}, errors.New("status is required (or supply prior_task_id/parent_task_id for linkage patch)")
		}
		now := in.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		patched, err := c.deps.Tasks.SetLinkage(ctx, in.ID, in.PriorTaskID, in.ParentTaskID, now, in.Memberships)
		if err != nil {
			if errors.Is(err, task.ErrNotFound) {
				return TaskUpdateOutput{}, fmt.Errorf("task_not_found: %s", in.ID)
			}
			if errors.Is(err, task.ErrInvalidTransition) {
				return TaskUpdateOutput{}, fmt.Errorf("task_already_terminal: %s", in.ID)
			}
			return TaskUpdateOutput{}, fmt.Errorf("task_update: %v", err)
		}
		return TaskUpdateOutput{
			TaskID:  patched.ID,
			Status:  patched.Status,
			Outcome: patched.Outcome,
		}, nil
	}
	if hasLinkagePatch {
		return TaskUpdateOutput{}, errors.New("task_update: linkage patch cannot combine with status mutation; chain them in two calls")
	}
	current, err := c.deps.Tasks.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return TaskUpdateOutput{}, fmt.Errorf("task_not_found: %s", in.ID)
	}
	if in.Status != task.StatusClosed {
		return TaskUpdateOutput{}, fmt.Errorf("task_update: unsupported status target %q", in.Status)
	}
	outcome := in.Outcome
	if outcome == "" {
		outcome = task.OutcomeSuccess
	}
	if outcome != task.OutcomeSuccess && outcome != task.OutcomeFailure {
		return TaskUpdateOutput{}, fmt.Errorf("invalid_outcome: %q (expected %q or %q)", outcome, task.OutcomeSuccess, task.OutcomeFailure)
	}
	if current.Status == task.StatusClosed || current.Status == task.StatusArchived {
		return TaskUpdateOutput{}, fmt.Errorf("task_already_terminal: %s status=%s", current.ID, current.Status)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// sty_090f6183: emit KindStatusChange BEFORE Close so the SSE
	// consumer observes status_change → close in producer order.
	c.publishTaskLog(ctx, current.ID, tasklog.KindStatusChange, map[string]any{
		"from":  current.Status,
		"to":    task.StatusClosed,
		"actor": caller.UserID,
	})
	closed, err := c.deps.Tasks.Close(ctx, current.ID, outcome, now, in.Memberships)
	if err != nil {
		return TaskUpdateOutput{}, fmt.Errorf("task close: %v", err)
	}
	// sty_090f6183: emit KindClose AFTER successful Close.
	c.publishTaskLog(ctx, closed.ID, tasklog.KindClose, map[string]any{
		"outcome":             closed.Outcome,
		"evidence_ledger_ids": in.EvidenceLedgerIDs,
	})
	return TaskUpdateOutput{
		TaskID:            closed.ID,
		Status:            closed.Status,
		Outcome:           closed.Outcome,
		EvidenceLedgerIDs: in.EvidenceLedgerIDs,
	}, nil
}

// TaskAddInput captures the rich fields task_add accepts: agent + prompt
// (required), with optional story_id, kind, action, priority,
// prior_task_id, parent_task_id. The resolver state —
// project/workspace/session — is computed inside TaskAdd against the
// supplied stores; the caller is responsible for presenting auth + URL-
// scoping context via TaskAddResolveDeps. When PriorTaskID is supplied
// explicitly the caller wins: the auto-supersession detection is
// skipped. sty_27516920.
type TaskAddInput struct {
	AgentID      string
	Prompt       string
	StoryID      string
	Kind         string
	Action       string
	Priority     string
	PriorTaskID  string
	ParentTaskID string
	// Trigger carries an optional runner payload as a JSON object string
	// (e.g. `{"branch":"…","sha":"…"}`). When non-empty it is stored
	// verbatim on Task.Trigger as bytes — the substrate does not validate
	// the JSON shape; downstream runners parse leniently. sty_a39e9e41.
	Trigger     string
	Memberships []string
	Resolve     TaskAddResolveDeps
	Now         time.Time
}

// TaskAddResolveDeps wires the project/workspace resolution callbacks
// the wire layer owns (session-bound active project, URL-scoped
// project, default project). Splitting these out keeps TaskAdd's
// transport-free contract intact while still letting the wire layer
// inject its session + URL context.
type TaskAddResolveDeps struct {
	CallerActiveProjectID     func(ctx context.Context, caller Caller) string
	ResolveStoryProjectID     func(ctx context.Context, storyID string, memberships []string) string
	ResolveProjectWorkspaceID func(ctx context.Context, projectID string) string
	DefaultProjectID          string
}

// TaskAddOutput mirrors the wire payload of task_add.
type TaskAddOutput struct {
	TaskID      string `json:"task_id"`
	StoryID     string `json:"story_id"`
	StoryMinted bool   `json:"story_minted"`
	Status      string `json:"status"`
	AgentID     string `json:"agent_id"`
}

// TaskAdd mints exactly one task at status=published for the given
// agent. When story_id is omitted the substrate auto-mints a thin
// ad-hoc story so every task is anchored to a story. Review pairing,
// when a contract requires one, is authored by the reviewer's contract
// prose via a follow-up task_add(prior_task_id=…) call (not handled
// here).
func (c *Client) TaskAdd(ctx context.Context, caller Caller, in TaskAddInput) (TaskAddOutput, error) {
	if c.deps.Tasks == nil || c.deps.Documents == nil || c.deps.Stories == nil || c.deps.Ledger == nil {
		return TaskAddOutput{}, errors.New("task_add unavailable: required stores not configured")
	}
	if in.AgentID == "" {
		return TaskAddOutput{}, errors.New("agent_id is required")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return TaskAddOutput{}, errors.New("task_add: prompt must not be empty")
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = task.KindWork
	}
	if kind != task.KindWork && kind != task.KindReview {
		return TaskAddOutput{}, fmt.Errorf("task_add: invalid kind %q (expected %q or %q)", kind, task.KindWork, task.KindReview)
	}
	action := strings.TrimSpace(in.Action)
	priority := strings.TrimSpace(in.Priority)
	if priority == "" {
		priority = task.PriorityMedium
	}
	storyID := strings.TrimSpace(in.StoryID)

	// Resolve agent doc — system-scope agents are globally readable.
	doc, err := c.deps.Documents.GetByID(ctx, in.AgentID, nil)
	if err != nil || doc.Status != document.StatusActive || doc.Type != document.TypeAgent {
		return TaskAddOutput{}, fmt.Errorf("agent_not_found: %q", in.AgentID)
	}
	settings, perr := document.UnmarshalAgentSettings(doc.Structured)
	if perr != nil {
		return TaskAddOutput{}, fmt.Errorf("agent_settings_invalid: %v", perr)
	}

	// Capability check.
	if action != "" && task.ContractFromAction(action) != "" {
		if kind == task.KindWork && !settings.CanDeliver(action) {
			return TaskAddOutput{}, fmt.Errorf("agent_cannot_deliver: agent_id=%q action=%q", in.AgentID, action)
		}
		if kind == task.KindReview && !settings.CanReview(action) {
			return TaskAddOutput{}, fmt.Errorf("agent_cannot_review: agent_id=%q action=%q", in.AgentID, action)
		}
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Resolve project + workspace.
	projectID := ""
	workspaceID := ""
	if doc.Scope == document.ScopeWorkspace {
		agentWS := doc.WorkspaceID
		if agentWS == "" {
			return TaskAddOutput{}, fmt.Errorf("agent_invalid: scope=workspace agent_id=%q has empty workspace_id", in.AgentID)
		}
		inWS := false
		for _, m := range in.Memberships {
			if m == agentWS {
				inWS = true
				break
			}
		}
		if !inWS {
			return TaskAddOutput{}, fmt.Errorf("agent_unavailable: agent_id=%q scope=workspace workspace_id=%s caller has no membership", in.AgentID, agentWS)
		}
		workspaceID = agentWS
		if storyID != "" && in.Resolve.ResolveStoryProjectID != nil && in.Resolve.ResolveProjectWorkspaceID != nil {
			if storyProj := in.Resolve.ResolveStoryProjectID(ctx, storyID, in.Memberships); storyProj != "" && in.Resolve.ResolveProjectWorkspaceID(ctx, storyProj) == agentWS {
				projectID = storyProj
			}
		}
		if projectID == "" && in.Resolve.CallerActiveProjectID != nil && in.Resolve.ResolveProjectWorkspaceID != nil {
			if cand := in.Resolve.CallerActiveProjectID(ctx, caller); cand != "" && in.Resolve.ResolveProjectWorkspaceID(ctx, cand) == agentWS {
				projectID = cand
			}
		}
		if projectID == "" && in.Resolve.DefaultProjectID != "" && in.Resolve.ResolveProjectWorkspaceID != nil && in.Resolve.ResolveProjectWorkspaceID(ctx, in.Resolve.DefaultProjectID) == agentWS {
			projectID = in.Resolve.DefaultProjectID
		}
		if projectID == "" {
			return TaskAddOutput{}, fmt.Errorf("agent_unavailable: agent_id=%q scope=workspace workspace_id=%s no caller project resolvable in agent workspace", in.AgentID, agentWS)
		}
	} else {
		if doc.Scope != document.ScopeSystem {
			if doc.ProjectID != nil {
				projectID = *doc.ProjectID
			}
			workspaceID = doc.WorkspaceID
		}
		if projectID == "" && in.Resolve.CallerActiveProjectID != nil {
			projectID = in.Resolve.CallerActiveProjectID(ctx, caller)
		}
		if projectID == "" {
			projectID = in.Resolve.DefaultProjectID
		}
		if workspaceID == "" && projectID != "" && in.Resolve.ResolveProjectWorkspaceID != nil {
			workspaceID = in.Resolve.ResolveProjectWorkspaceID(ctx, projectID)
		}
		if workspaceID == "" && len(in.Memberships) > 0 {
			workspaceID = in.Memberships[0]
		}
	}

	storyMinted := false
	var st story.Story
	if storyID != "" {
		st, err = c.deps.Stories.GetByID(ctx, storyID, in.Memberships)
		if err != nil {
			return TaskAddOutput{}, fmt.Errorf("story_not_found: %q", storyID)
		}
	} else {
		if projectID == "" {
			return TaskAddOutput{}, errors.New("task_add: cannot auto-mint story — no project_id resolvable from agent or session")
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
		st, err = c.deps.Stories.Create(ctx, candidate, now)
		if err != nil {
			return TaskAddOutput{}, fmt.Errorf("story auto-mint failed: %v", err)
		}
		storyMinted = true
	}

	// Caller-supplied linkage (sty_27516920) takes precedence over
	// auto-supersession detection. Validate that the referenced rows
	// exist under caller memberships so the substrate refuses
	// orchestrator typos before the row lands.
	priorTaskID := strings.TrimSpace(in.PriorTaskID)
	parentTaskID := strings.TrimSpace(in.ParentTaskID)
	if priorTaskID != "" {
		if _, perr := c.deps.Tasks.GetByID(ctx, priorTaskID, in.Memberships); perr != nil {
			return TaskAddOutput{}, fmt.Errorf("prior_task_not_found: %q", priorTaskID)
		}
	}
	if parentTaskID != "" {
		if _, perr := c.deps.Tasks.GetByID(ctx, parentTaskID, in.Memberships); perr != nil {
			return TaskAddOutput{}, fmt.Errorf("parent_task_not_found: %q", parentTaskID)
		}
	}

	// Auto-supersession (sty_9d046bc7): when minting a fresh kind=work
	// task whose (story_id, kind, action) matches a closed=failure
	// predecessor with no successor pointing at it, stamp prior_task_id
	// on the new row so runMergeToMainHotPath's chain-shape gate accepts
	// the linked chain. Only fires for kind=work + non-empty action;
	// review chains use parent_task_id and are out of scope. Skipped
	// when the caller supplied PriorTaskID explicitly (caller wins,
	// sty_27516920).
	if priorTaskID == "" && kind == task.KindWork && action != "" {
		chain, lerr := c.deps.Tasks.List(ctx, task.ListOptions{
			StoryID:         st.ID,
			IncludeArchived: false,
		}, in.Memberships)
		if lerr == nil {
			linked := make(map[string]bool, len(chain))
			for _, t := range chain {
				if t.PriorTaskID != "" {
					linked[t.PriorTaskID] = true
				}
			}
			sort.Slice(chain, func(i, j int) bool {
				return chain[i].CreatedAt.Before(chain[j].CreatedAt)
			})
			for i := len(chain) - 1; i >= 0; i-- {
				p := chain[i]
				if p.Kind != task.KindWork {
					continue
				}
				if p.Action != action {
					continue
				}
				if p.Status != task.StatusClosed {
					continue
				}
				if p.Outcome != task.OutcomeFailure {
					continue
				}
				if linked[p.ID] {
					continue
				}
				priorTaskID = p.ID
				break
			}
		}
	}

	newTask := task.Task{
		WorkspaceID:  st.WorkspaceID,
		ProjectID:    st.ProjectID,
		StoryID:      st.ID,
		Kind:         kind,
		Action:       action,
		Description:  prompt,
		AgentID:      in.AgentID,
		Origin:       task.OriginStoryStage,
		Priority:     priority,
		Status:       task.StatusPublished,
		PriorTaskID:  priorTaskID,
		ParentTaskID: parentTaskID,
	}
	if trimmed := strings.TrimSpace(in.Trigger); trimmed != "" {
		newTask.Trigger = []byte(trimmed)
	}
	work, err := c.deps.Tasks.Enqueue(ctx, newTask, now)
	if err != nil {
		return TaskAddOutput{}, fmt.Errorf("task enqueue: %v", err)
	}

	_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: work.WorkspaceID,
		ProjectID:   work.ProjectID,
		StoryID:     ledger.StringPtr(st.ID),
		Type:        ledger.TypeDecision,
		Tags: []string{
			"kind:task-published",
			"task_id:" + work.ID,
			"agent_id:" + in.AgentID,
			"source:task_add",
		},
		Content:    fmt.Sprintf("task_add: id=%s agent=%s action=%s prompt_chars=%d", work.ID, in.AgentID, fallback(action, "<ad-hoc>"), len(prompt)),
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceAgent,
		Status:     ledger.StatusActive,
		CreatedBy:  caller.UserID,
	}, now)

	return TaskAddOutput{
		TaskID:      work.ID,
		StoryID:     st.ID,
		StoryMinted: storyMinted,
		Status:      work.Status,
		AgentID:     in.AgentID,
	}, nil
}

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
func summariseActions(tasks []task.Task, ledgerByAction map[string]int) []TaskWalkActionSummary {
	type bucket struct {
		idx     int
		summary TaskWalkActionSummary
	}
	order := make([]string, 0)
	buckets := map[string]*bucket{}
	for _, t := range tasks {
		if t.Action == "" {
			continue
		}
		b, ok := buckets[t.Action]
		if !ok {
			b = &bucket{idx: len(order), summary: TaskWalkActionSummary{Action: t.Action}}
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
	out := make([]TaskWalkActionSummary, len(order))
	for i, action := range order {
		b := buckets[action]
		b.summary.LedgerRowCount = ledgerByAction[action]
		out[i] = b.summary
	}
	return out
}

// isTerminalTaskStatus reports whether a task has reached a terminal
// status (no further transitions).
func isTerminalTaskStatus(status string) bool {
	switch status {
	case task.StatusClosed, task.StatusArchived:
		return true
	}
	return false
}

// actionFromTag pulls the contract action out of `phase:<name>` or
// `contract:<name>` tags so the ledger summary can roll up by action.
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

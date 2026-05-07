// Package task is the satellites-v4 task queue primitive per
// docs/architecture.md §4. A Task is a unit of orchestration work with
// a single origin (story_stage, scheduled, story_producing, event), a
// lifecycle (enqueued → claimed → in_flight → closed), and a
// workspace-scoped audit trail.
//
// Principles: pr_75826278 (Tasks are the orchestration queue),
// pr_c25cc661 (Tasks are one of five primitives), pr_0779e5af
// (Workspace is the multi-tenant isolation primitive).
package task

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Origin enum values per §4 "Origins" table.
const (
	OriginStoryStage     = "story_stage"
	OriginScheduled      = "scheduled"
	OriginStoryProducing = "story_producing"
	OriginEvent          = "event"
)

// Status enum values. Defaults; the canonical set + transition matrix
// is loaded at boot from config/seed/lifecycles/task.md (sty_c1200f75)
// via RegisterLifecycle. The constants below are kept so callers can
// reference statuses by name in code without string-literal sprawl,
// and so the runtime has safe defaults if the seed is missing.
//
// StatusPlanned (sty_c1200f75) is the agent-local drafting state. Rows
// at this status are persisted but invisible to subscribers — the
// orchestrator must call task_publish to flip them to StatusPublished
// before any worker can claim them.
//
// StatusPublished (sty_c1200f75) is the committed-queue state. The
// pre-c1200f75 substrate used StatusEnqueued for the same purpose; on
// next boot, existing enqueued rows are migrated to published.
//
// StatusArchived is the post-retention state introduced by sty_dc2998c5.
const (
	StatusPlanned   = "planned"
	StatusPublished = "published"
	StatusEnqueued  = "enqueued" // legacy alias; migrated to published on boot
	StatusClaimed   = "claimed"
	StatusInFlight  = "in_flight"
	StatusClosed    = "closed"
	StatusArchived  = "archived"
)

// Priority enum values — dispatcher pulls critical before high, etc.
const (
	PriorityCritical = "critical"
	PriorityHigh     = "high"
	PriorityMedium   = "medium"
	PriorityLow      = "low"
)

// Outcome enum values — empty until Status=closed.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeTimeout = "timeout"
)

// Kind enum values. Empty defaults to KindWork.
const (
	KindWork   = "work"
	KindReview = "review"
)

// ActionContractPrefix is the canonical prefix for an Action that
// names a contract delivery — e.g. `contract:plan`, `contract:develop`.
// sty_c6d76a5b: Action separates "what does this task deliver" from
// "what type of activity is this" (Kind).
const ActionContractPrefix = "contract:"

// ContractAction returns the canonical Action string for a contract
// name (e.g. "plan" → "contract:plan"). Helper for callers that
// build Task rows.
func ContractAction(contractName string) string {
	return ActionContractPrefix + contractName
}

// ContractFromAction returns the contract name embedded in an Action,
// or "" when the Action is empty or doesn't reference a contract.
func ContractFromAction(action string) string {
	if len(action) <= len(ActionContractPrefix) || action[:len(ActionContractPrefix)] != ActionContractPrefix {
		return ""
	}
	return action[len(ActionContractPrefix):]
}

var validOrigins = map[string]struct{}{
	OriginStoryStage: {}, OriginScheduled: {}, OriginStoryProducing: {},
	OriginEvent: {},
}

var validStatuses = map[string]struct{}{
	StatusPlanned: {}, StatusPublished: {}, StatusEnqueued: {},
	StatusClaimed: {}, StatusInFlight: {}, StatusClosed: {}, StatusArchived: {},
}

var validPriorities = map[string]struct{}{
	PriorityCritical: {}, PriorityHigh: {}, PriorityMedium: {}, PriorityLow: {},
}

var validOutcomes = map[string]struct{}{
	OutcomeSuccess: {}, OutcomeFailure: {}, OutcomeTimeout: {},
}

// priorityRank orders priorities so Claim can pick critical first.
var priorityRank = map[string]int{
	PriorityCritical: 0,
	PriorityHigh:     1,
	PriorityMedium:   2,
	PriorityLow:      3,
}

// PriorityRank exposes the dispatcher ordering to callers who need to
// sort task slices themselves (e.g. reporting). Unknown priorities sort
// last.
func PriorityRank(p string) int {
	if r, ok := priorityRank[p]; ok {
		return r
	}
	return 999
}

// Task is one orchestration row. Fields match docs/architecture.md §4
// verbatim. Trigger is a JSON-encoded raw byte slice so the store
// layer doesn't require compile-time knowledge of every origin's shape.
// sty_509a46fa retired the Payload blob; tasks are thin and any
// artifact content lives on linked ledger rows.
//
// Iteration is the lap number for tasks of the same Action on the same
// story (the same-slot retry chain). First work-task on (story_id,
// action) gets iteration=1; successor work tasks minted via
// task_add(prior_task_id=…) by the reviewer's contract prose bump it.
// Surfaced on task_list / task_walk so renderers can show "develop #2"
// without joining anywhere. sty_c6d76a5b.
type Task struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id,omitempty"`
	// StoryID links the task to its story directly. sty_c6d76a5b's
	// model treats `tasks where story_id=X` as the conversation log
	// — the story IS the task chain. Set explicitly by
	// task_submit; legacy tasks created via the CI path can
	// project StoryID from their CI.
	StoryID string `json:"story_id,omitempty"`
	// Kind classifies the task by its purpose so subscribers can filter
	// the queue without inspecting payloads. Today: "review" (consumed
	// by the embedded reviewer service) vs "" / "work" (everything
	// else). sty_c6d76a5b promotes "work" → "implement" semantics: implement
	// tasks are claimed by the orchestrator; review tasks are claimed by
	// the embedded reviewer subscriber. The pair (implement, review)
	// per CI is the unit of progress.
	Kind      string `json:"kind,omitempty"`
	Iteration int    `json:"iteration,omitempty"`
	// AgentID stamps the per-task agent doc this task is bound to
	// (sty_c6d76a5b). The orchestrator's task_claim verb verifies that
	// the caller-supplied agent_id matches this stamped value. For
	// implement tasks: copied from the parent CI's agent_id at compose
	// time (set by mintTaskAgentForContract per sty_e8d49554). For
	// review tasks: the persistent reviewer agent's id.
	AgentID string `json:"agent_id,omitempty"`
	// PriorTaskID links a fresh implement task to the prior implement
	// task it succeeds in the same-slot retry chain authored by the
	// reviewer's contract prose via task_add(prior_task_id=…)
	// (sty_c6d76a5b). Empty for the first attempt.
	PriorTaskID string `json:"prior_task_id,omitempty"`
	// ParentTaskID anchors this task to the conversation thread it
	// extends — typically the implement task whose close emitted this
	// successor (review verdict task → fresh implement task; reviewer
	// proposal → orchestrator response; etc.). Together with the
	// per-story (sequence, created_at) ordering, the parent_task_id
	// chain reconstructs the conversation log. Empty when the task
	// is a thread root (e.g. the initial implement task per CI).
	// sty_c6d76a5b.
	ParentTaskID string `json:"parent_task_id,omitempty"`
	// Action names what the task delivers, in canonical form
	// `contract:<name>` (e.g. `contract:plan`, `contract:develop`). Set
	// by the orchestrator at task_submit time and validated by
	// the substrate against the project's contract markdown set
	// (sty_c6d76a5b). For Kind=KindReview, the action is the contract
	// being reviewed (the review task is the sibling of a Kind=KindWork
	// task with the same Action). Empty for legacy tasks predating
	// sty_c6d76a5b's plan-author flow.
	Action           string        `json:"action,omitempty"`
	Description      string        `json:"description,omitempty"`
	Origin           string        `json:"origin"`
	Trigger          []byte        `json:"trigger,omitempty"`
	Status           string        `json:"status"`
	Priority         string        `json:"priority"`
	ClaimedBy        string        `json:"claimed_by,omitempty"`
	ClaimedAt        *time.Time    `json:"claimed_at,omitempty"`
	CompletedAt      *time.Time    `json:"completed_at,omitempty"`
	Outcome          string        `json:"outcome,omitempty"`
	LedgerRootID     string        `json:"ledger_root_id,omitempty"`
	ExpectedDuration time.Duration `json:"expected_duration,omitempty"`
	ReclaimCount     int           `json:"reclaim_count,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
}

// NewID returns a fresh task id in the canonical `task_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("task_%s", uuid.NewString()[:8])
}

// Validate returns the first invariant violation on t, or nil if t is
// well-formed. Used at the store layer before every write.
func (t Task) Validate() error {
	if t.WorkspaceID == "" {
		return errors.New("task: workspace_id required")
	}
	if _, ok := validOrigins[t.Origin]; !ok {
		return fmt.Errorf("task: invalid origin %q", t.Origin)
	}
	if _, ok := validStatuses[t.Status]; !ok {
		return fmt.Errorf("task: invalid status %q", t.Status)
	}
	if _, ok := validPriorities[t.Priority]; !ok {
		return fmt.Errorf("task: invalid priority %q", t.Priority)
	}
	if t.Outcome != "" {
		if _, ok := validOutcomes[t.Outcome]; !ok {
			return fmt.Errorf("task: invalid outcome %q", t.Outcome)
		}
		if t.Status != StatusClosed {
			return errors.New("task: outcome may only be set when status=closed")
		}
	}
	if t.Status == StatusClosed && t.Outcome == "" {
		return errors.New("task: outcome required when status=closed")
	}
	if t.Status == StatusClaimed || t.Status == StatusInFlight {
		if t.ClaimedBy == "" {
			return errors.New("task: claimed_by required when status=claimed|in_flight")
		}
	}
	return nil
}

// ValidTransition returns true when moving from → to is a legal Status
// transition.
//
// Matrix (sty_c1200f75):
//   - planned   → published | closed
//   - published → claimed   | closed
//   - enqueued  → published | claimed | closed   (legacy compat)
//   - claimed   → in_flight | closed | published
//   - in_flight → closed
//   - closed    → archived
//
// Archived is terminal — the row drops out of the default task_list
// query but the ledger anchors persist for audit.
func ValidTransition(from, to string) bool {
	switch from {
	case StatusPlanned:
		return to == StatusPublished || to == StatusClosed
	case StatusPublished:
		return to == StatusClaimed || to == StatusClosed
	case StatusEnqueued:
		return to == StatusPublished || to == StatusClaimed || to == StatusClosed
	case StatusClaimed:
		return to == StatusInFlight || to == StatusClosed || to == StatusPublished || to == StatusEnqueued
	case StatusInFlight:
		return to == StatusClosed
	case StatusClosed:
		return to == StatusArchived
	default:
		return false
	}
}

// SubscriberVisibleStatuses returns the set of statuses subscribers
// (reviewer service, future task-claim workers) may see. Planned rows
// are agent-local — never visible to subscribers.
func SubscriberVisibleStatuses() map[string]struct{} {
	return map[string]struct{}{
		StatusPublished: {},
		StatusEnqueued:  {}, // legacy compat — pre-migration rows
		StatusClaimed:   {},
		StatusInFlight:  {},
	}
}

// IsSubscriberVisible reports whether a worker subscribed to the task
// queue should see a row at this status.
func IsSubscriberVisible(status string) bool {
	_, ok := SubscriberVisibleStatuses()[status]
	return ok
}

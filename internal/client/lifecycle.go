// Package client — lifecycle_status helper (sty_e0c3d615).
//
// computeLifecycleStatus walks the closed-task chain newest-first
// against the phase ordering enumerated by the workspace-scope
// `default_lifecycle` workflow document and returns one of:
//
//   - `on_shape`                         — chain matches the workflow
//   - `drifted:plan_absent`              — no contract:plan work task
//   - `drifted:review_skipped`           — terminal story with a closed
//                                          contract:develop work task
//                                          but no closed paired review
//   - `drifted:close_before_push`        — terminal story but no
//                                          contract:merge_to_main work
//   - `drifted:phase_unknown:<action>`   — closed work task whose
//                                          action this workflow does
//                                          not enumerate
//
// Advisory only — the `story_close` gate does NOT refuse to close on
// drift; it appends one `kind:lifecycle-drift` warning row (idempotent
// per reason).
//
// The phase enumeration mirrors `canonicalPhases` in chain.go and the
// `default_lifecycle.md` workflow doc prose. Collapsing the two
// sources into the workflow doc is a follow-up once the drift signal
// becomes load-bearing (gating, not advisory).

package client

import (
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// Lifecycle status values returned by computeLifecycleStatus.
const (
	LifecycleOnShape           = "on_shape"
	LifecyclePlanAbsent        = "drifted:plan_absent"
	LifecycleReviewSkipped     = "drifted:review_skipped"
	LifecycleCloseBeforePush   = "drifted:close_before_push"
	LifecyclePhaseUnknownPfx   = "drifted:phase_unknown:"
)

// lifecyclePhase is one slot in the canonical chain order. The list
// below mirrors canonicalPhases (chain.go) and the default_lifecycle
// workflow doc body. Plan absence is asserted separately because it is
// the workflow's anchor.
type lifecyclePhase struct {
	Action string
	Kind   string
}

// lifecyclePhases enumerates the actions+kinds the workflow accepts as
// part of the on-shape chain. Order is the canonical lifecycle ordering;
// review pairs follow their work siblings.
var lifecyclePhases = []lifecyclePhase{
	{Action: "contract:plan", Kind: task.KindWork},
	{Action: "contract:plan", Kind: task.KindReview},
	{Action: "contract:develop", Kind: task.KindWork},
	{Action: "contract:develop", Kind: task.KindReview},
	{Action: "contract:story_review", Kind: task.KindWork},
	{Action: "contract:story_review", Kind: task.KindReview},
	{Action: "contract:commit", Kind: task.KindWork},
	{Action: "contract:merge_to_main", Kind: task.KindWork},
}

// terminalStoryStatus reports whether the story has reached a terminal
// state (done|cancelled). Drift checks that compare "what shipped"
// against the lifecycle ordering only fire on terminal stories; mid-
// flight chains are normal.
func terminalStoryStatus(s string) bool {
	return s == story.StatusDone || s == story.StatusCancelled
}

// computeLifecycleStatus walks the closed-task chain against the
// enumerated lifecycle phases and returns the drift status. The signal
// is advisory — the gate that emits a kind:lifecycle-drift warning row
// reads this same value via story_close.
//
// Algorithm:
//
//  1. Empty chain or no closed contract:plan work task → plan_absent.
//  2. Any closed work task whose action is not in lifecyclePhases →
//     phase_unknown:<action>.
//  3. Story status is terminal AND a closed contract:develop work task
//     exists AND no closed contract:develop review task exists →
//     review_skipped.
//  4. Story status is terminal AND no contract:merge_to_main work task
//     present → close_before_push.
//  5. Otherwise → on_shape.
func computeLifecycleStatus(storyStatus string, tasks []task.Task) string {
	closedByActionKind := map[string]map[string]int{}
	for _, t := range tasks {
		if t.Status != task.StatusClosed {
			continue
		}
		if t.Action == "" {
			continue
		}
		if _, ok := closedByActionKind[t.Action]; !ok {
			closedByActionKind[t.Action] = map[string]int{}
		}
		closedByActionKind[t.Action][t.Kind]++
	}

	if closedByActionKind["contract:plan"][task.KindWork] == 0 {
		return LifecyclePlanAbsent
	}

	known := map[string]map[string]struct{}{}
	for _, p := range lifecyclePhases {
		if _, ok := known[p.Action]; !ok {
			known[p.Action] = map[string]struct{}{}
		}
		known[p.Action][p.Kind] = struct{}{}
	}
	for _, t := range tasks {
		if t.Status != task.StatusClosed {
			continue
		}
		if t.Action == "" {
			continue
		}
		if t.Kind != task.KindWork {
			continue
		}
		if _, ok := known[t.Action]; !ok {
			return LifecyclePhaseUnknownPfx + t.Action
		}
	}

	if terminalStoryStatus(storyStatus) {
		developClosed := closedByActionKind["contract:develop"][task.KindWork] > 0
		developReviewed := closedByActionKind["contract:develop"][task.KindReview] > 0
		if developClosed && !developReviewed {
			return LifecycleReviewSkipped
		}
		if !hasTaskForAction(tasks, "contract:merge_to_main") {
			return LifecycleCloseBeforePush
		}
	}

	return LifecycleOnShape
}

// hasTaskForAction reports whether any task on the chain (regardless
// of status) carries the given action + kind=work. Used by
// close_before_push: the absence of a merge_to_main work task on a
// terminal chain is the drift signal.
func hasTaskForAction(tasks []task.Task, action string) bool {
	for _, t := range tasks {
		if t.Action == action && t.Kind == task.KindWork {
			return true
		}
	}
	return false
}

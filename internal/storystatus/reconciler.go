// Package storystatus is the system-level reconciler that derives a
// story's lifecycle status from observed task transitions
// (sty_051bd266). The substrate's plumbing for derivation lives in
// story.Store.UpdateStatusDerived; this package supplies the missing
// task → story listener that calls it.
//
// Today the rule set is intentionally small: any task in a non-terminal
// status (published / claimed / in_flight) for a story currently at
// backlog or ready flips that story to in_progress. The reverse (no
// open tasks → demote to ready) is out of scope until the operator
// confirms the policy. Terminal stories (done / cancelled) are not
// touched — `UpdateStatusDerived` is exempt from the forward-only
// guard but remains bounded by `IsKnownStatus`.
//
// Wiring: cmd/satellites attaches an instance via
// task.Store.AddListener at boot. Listener panics are recovered by
// task/listener.go's fanoutListeners; a buggy reconciler cannot abort
// the writer.
package storystatus

import (
	"context"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// Reconciler implements task.Listener. It owns no state beyond
// pointers to the story.Store and the logger; the work happens
// per-event in OnEmit.
type Reconciler struct {
	stories story.Store
	logger  arbor.ILogger
}

// New returns a Reconciler. logger may be nil — log lines are
// dropped when so.
func New(stories story.Store, logger arbor.ILogger) *Reconciler {
	return &Reconciler{stories: stories, logger: logger}
}

// OnEmit implements task.Listener. It runs after every task status
// transition; it is cheap to call on every event and only triggers a
// substrate write when the task's transition actually requires the
// story to advance.
func (r *Reconciler) OnEmit(ctx context.Context, t task.Task) {
	if r == nil || r.stories == nil {
		return
	}
	if t.StoryID == "" {
		return
	}
	if !isOpenTaskStatus(t.Status) {
		return
	}
	// nil memberships → unscoped lookup. The reconciler runs as the
	// substrate, not as a workspace member.
	s, err := r.stories.GetByID(ctx, t.StoryID, nil)
	if err != nil {
		if r.logger != nil {
			r.logger.Debug().Str("story_id", t.StoryID).Str("error", err.Error()).
				Msg("storystatus: get story failed")
		}
		return
	}
	if s.Status != story.StatusBacklog && s.Status != story.StatusReady {
		return
	}
	if _, err := r.stories.UpdateStatusDerived(ctx, t.StoryID, story.StatusInProgress, time.Now().UTC(), nil); err != nil {
		if r.logger != nil {
			r.logger.Warn().Str("story_id", t.StoryID).Str("from", s.Status).
				Str("to", story.StatusInProgress).Str("error", err.Error()).
				Msg("storystatus: derived flip failed")
		}
		return
	}
	if r.logger != nil {
		r.logger.Info().Str("story_id", t.StoryID).Str("task_id", t.ID).
			Str("from", s.Status).Str("to", story.StatusInProgress).
			Msg("storystatus: story flipped to in_progress on first open task")
	}
}

// isOpenTaskStatus reports whether t.Status is the kind of transition
// that should advance a backlog/ready story to in_progress. The set is
// every non-terminal status; closed/archived rows have already
// contributed whatever derivation they were going to.
func isOpenTaskStatus(s string) bool {
	switch s {
	case task.StatusPublished, task.StatusEnqueued, task.StatusClaimed, task.StatusInFlight:
		return true
	}
	return false
}

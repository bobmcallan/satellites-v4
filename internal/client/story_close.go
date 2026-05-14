// Package client — `story_close` mechanical close verb (sty_b97dda00 slice 1).
//
// Structural-only verb. Reads the story's task chain + the story_review
// verdict ledger row + the resolved category template's `done` transition
// hooks, returns the gap list on failure, and on PASS appends a
// `kind:close-evidence` ledger row + walks the story to `done` via
// UpdateStatusDerived.
//
// The verb performs no LLM call, no shell-out, no agent dispatch — same
// tier as pr_story_terminal_gate's in-Go enforcement in
// internal/story/store.go.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// StoryCloseInput names the story to close. ResolutionCode is the slot
// stored on the kind:close-evidence ledger row; empty falls back to
// "delivered" per the story_review contract's evidence_required prose.
type StoryCloseInput struct {
	StoryID        string
	ResolutionCode string
	Memberships    []string
	Now            time.Time
}

// StoryCloseGap is one cited gap the gate refused on. Code is the
// machine-readable category (story_review:absent | story_review:open |
// story_review:fail | chain:open_work | template:<field>:missing);
// Detail carries the row id(s), task id(s), or field name the gap
// resolves to.
type StoryCloseGap struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// StoryCloseOutput is the response envelope. Status is "pass" or
// "fail"; on "pass", the story walked to done and StoryStatus is the
// post-transition status; on "fail", Gaps is non-empty and no
// mutation occurred (StoryStatus matches pre-call state).
type StoryCloseOutput struct {
	Status      string          `json:"status"`
	StoryID     string          `json:"story_id"`
	StoryStatus string          `json:"story_status"`
	EvidenceID  string          `json:"evidence_id,omitempty"`
	Gaps        []StoryCloseGap `json:"gaps,omitempty"`
}

// Sentinel errors mapped by wire handlers to per-envelope shapes.
var (
	ErrStoryCloseStoryIDRequired = errors.New("story_id is required")
	ErrStoryCloseStoryNotFound   = errors.New("story not found")
)

// resolutionCodeDefault is the slot recorded on the close-evidence row
// when the caller does not supply ResolutionCode.
const resolutionCodeDefault = "delivered"

// StoryClose is the mechanical close gate. On gap-free chain it appends
// a kind:close-evidence ledger row and walks the story to done via
// UpdateStatusDerived; on any gap it returns {status:"fail", gaps:[…]}
// without mutation.
func (c *Client) StoryClose(ctx context.Context, caller Caller, in StoryCloseInput) (StoryCloseOutput, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil || c.deps.Ledger == nil {
		return StoryCloseOutput{}, errors.New("story_close unavailable: required stores not configured")
	}
	if in.StoryID == "" {
		return StoryCloseOutput{}, ErrStoryCloseStoryIDRequired
	}
	memberships := in.Memberships
	if memberships == nil {
		memberships = c.ResolveCallerMemberships(ctx, caller)
	}
	st, err := c.deps.Stories.GetByID(ctx, in.StoryID, memberships)
	if err != nil {
		return StoryCloseOutput{}, ErrStoryCloseStoryNotFound
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if st.Status == story.StatusDone {
		return StoryCloseOutput{
			Status:      "fail",
			StoryID:     st.ID,
			StoryStatus: st.Status,
			Gaps:        []StoryCloseGap{{Code: "story:already_done"}},
		}, nil
	}

	tasks, err := c.deps.Tasks.List(ctx, task.ListOptions{StoryID: st.ID, Limit: 500}, memberships)
	if err != nil {
		return StoryCloseOutput{}, err
	}
	rows, err := c.deps.Ledger.List(ctx, st.ProjectID, ledger.ListOptions{StoryID: st.ID, Limit: ledger.MaxListLimit}, memberships)
	if err != nil {
		return StoryCloseOutput{}, err
	}

	var gaps []StoryCloseGap

	// AC3(c) — no open kind=work task on chain.
	var openWork []string
	for _, t := range tasks {
		if t.Kind != task.KindWork {
			continue
		}
		switch t.Status {
		case task.StatusPlanned, task.StatusPublished, task.StatusEnqueued, task.StatusClaimed, task.StatusInFlight:
			openWork = append(openWork, t.ID)
		}
	}
	if len(openWork) > 0 {
		gaps = append(gaps, StoryCloseGap{Code: "chain:open_work", Detail: strings.Join(openWork, ",")})
	}

	// AC3(a,b,e) — story_review task + verdict.
	reviewTask := latestStoryReviewTask(tasks)
	var verdictRow *ledger.LedgerEntry
	switch {
	case reviewTask == nil:
		gaps = append(gaps, StoryCloseGap{Code: "story_review:absent"})
	case reviewTask.Status != task.StatusClosed:
		gaps = append(gaps, StoryCloseGap{Code: "story_review:open", Detail: reviewTask.ID})
	default:
		verdictRow = latestVerdictRow(rows, reviewTask.ID)
		if verdictRow == nil {
			gaps = append(gaps, StoryCloseGap{Code: "story_review:fail", Detail: "no verdict row"})
		} else if !hasTag(verdictRow.Tags, "verdict:pass") {
			gaps = append(gaps, StoryCloseGap{Code: "story_review:fail", Detail: reviewTask.ID})
		}
	}

	// AC3(d) — template fields per the resolved infrastructure template.
	if tmpl, ok := c.loadStoryTemplate(ctx, st.Category); ok {
		ev := story.EvaluationContext{
			LedgerEntriesForStory: func(ctx context.Context, storyID string) ([]ledger.LedgerEntry, error) {
				return rows, nil
			},
		}
		for _, failure := range tmpl.EvaluateTransition(ctx, story.StatusDone, st, ev) {
			gaps = append(gaps, StoryCloseGap{Code: "template:" + fieldFromTemplateFailure(failure) + ":missing", Detail: failure})
		}
	}

	if len(gaps) > 0 {
		return StoryCloseOutput{
			Status:      "fail",
			StoryID:     st.ID,
			StoryStatus: st.Status,
			Gaps:        gaps,
		}, nil
	}

	resolution := strings.TrimSpace(in.ResolutionCode)
	if resolution == "" {
		resolution = resolutionCodeDefault
	}
	payload := map[string]any{
		"resolution":      resolution,
		"review_task_id":  reviewTask.ID,
		"verdict_row_id":  verdictRow.ID,
		"chain_size":      len(tasks),
	}
	body, _ := json.Marshal(payload)
	evidence, err := c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: st.WorkspaceID,
		ProjectID:   st.ProjectID,
		StoryID:     ledger.StringPtr(st.ID),
		Type:        ledger.TypeEvidence,
		Tags:        []string{"kind:close-evidence", "resolution:" + resolution},
		Content:     string(body),
		Durability:  ledger.DurabilityDurable,
		SourceType:  ledger.SourceSystem,
		Status:      ledger.StatusActive,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return StoryCloseOutput{}, err
	}
	updated, err := c.deps.Stories.UpdateStatusDerived(ctx, st.ID, story.StatusDone, now, memberships)
	if err != nil {
		return StoryCloseOutput{}, err
	}
	return StoryCloseOutput{
		Status:      "pass",
		StoryID:     updated.ID,
		StoryStatus: updated.Status,
		EvidenceID:  evidence.ID,
	}, nil
}

// latestStoryReviewTask returns the most recently created
// contract:story_review task on the chain regardless of Kind (sty_87148b8b).
//
// The canonical post-sty_b97dda00 workflow mints the story_review task as
// kind=work (workflow doc + story_reviewer.delivers both declare it). The
// historical tactical replay used during sty_8f99ef39 minted the same task
// as kind=review. Either shape is now valid for the close gate — the
// invariant the handler enforces is "the chain carries a closed
// contract:story_review task whose verdict row is verdict:pass", not the
// task's kind. The narrower kind=review-only filter that previously lived
// here is gone (no compat alias).
func latestStoryReviewTask(tasks []task.Task) *task.Task {
	var picked *task.Task
	for i := range tasks {
		t := &tasks[i]
		if t.Action != "contract:story_review" {
			continue
		}
		if picked == nil || t.CreatedAt.After(picked.CreatedAt) {
			picked = t
		}
	}
	return picked
}

// latestVerdictRow finds the latest kind:verdict ledger row tagged with
// task_id:<reviewTaskID>; nil when no such row exists.
func latestVerdictRow(rows []ledger.LedgerEntry, reviewTaskID string) *ledger.LedgerEntry {
	taskTag := "task_id:" + reviewTaskID
	var picked *ledger.LedgerEntry
	for i := range rows {
		r := &rows[i]
		if !hasTag(r.Tags, "kind:verdict") || !hasTag(r.Tags, taskTag) {
			continue
		}
		if picked == nil || r.CreatedAt.After(picked.CreatedAt) {
			picked = r
		}
	}
	return picked
}

// hasTag returns true when tags contains the literal tag value.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// fieldFromTemplateFailure extracts the field name from a template's
// EvaluateTransition failure string. The hook's message format is
// `required field "scope" is empty — set it via story_update …`; we
// pick the first quoted token. Falls back to "unknown" so the gap
// code is still well-formed when the message changes.
func fieldFromTemplateFailure(msg string) string {
	open := strings.Index(msg, "\"")
	if open < 0 {
		return "unknown"
	}
	rest := msg[open+1:]
	close := strings.Index(rest, "\"")
	if close <= 0 {
		return "unknown"
	}
	return rest[:close]
}

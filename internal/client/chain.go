// Package client — chain.go (sty_4fb2d985).
//
// `chain_*` typed methods replace the operator-side
// `.satellites/route_epic.sh` bash helper with a first-class router
// living on *Client. MCP, HTTP, and CLI are thin transport adapters
// per pr_mcp_cli_shared_path: the canonical phase order, the
// supersession-aware live-task selector, and the loop body all live
// here.
//
// DispatchHook is the seam that keeps the router transport-free. The
// CLI wraps `postEnqueue` (the unix-socket helper in
// cmd/satellites-client/task_run_async.go) and passes it in. The MCP
// adapter passes nil — `chain_advance` over MCP returns the
// next_task_id and lets the caller dispatch.

package client

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// DispatchAck is the wire payload the daemon's enqueue helper returns.
// Mirrors the fields of clientdaemon.EnqueueResponse — captured here
// so internal/client carries no dependency on the daemon package
// (pr_mcp_cli_shared_path: substrate-typed surface, no transport
// imports).
type DispatchAck struct {
	Dispatched    bool   `json:"dispatched"`
	DaemonPID     int    `json:"daemon_pid,omitempty"`
	QueuePosition int    `json:"queue_position,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// DispatchHook is the wire adapter callback. Returns a DispatchAck on
// success; an error short-circuits the router with the error
// propagated to the caller. Nil hook means "return next_task_id only"
// — the MCP variant.
type DispatchHook func(ctx context.Context, taskID string) (DispatchAck, error)

// ChainStatusInput names the story whose chain should be inspected.
type ChainStatusInput struct {
	StoryID     string
	Memberships []string
	Now         time.Time
}

// ChainStatusOutput is the read-only snapshot of the story's chain.
type ChainStatusOutput struct {
	StoryID     string       `json:"story_id"`
	StoryStatus string       `json:"story_status"`
	Phases      []ChainPhase `json:"phases"`
	NextTaskID  string       `json:"next_task_id,omitempty"`
	Terminal    bool         `json:"terminal"`
	Anomalies   []string     `json:"anomalies,omitempty"`
}

// ChainPhase is the per-phase projection: which (action, kind) cell
// it represents and the live task selected from that bucket.
type ChainPhase struct {
	Action      string `json:"action"`
	Kind        string `json:"kind"`
	LiveTaskID  string `json:"live_task_id,omitempty"`
	LiveStatus  string `json:"live_status,omitempty"`
	LiveOutcome string `json:"live_outcome,omitempty"`
	Iterations  int    `json:"iterations,omitempty"`
}

// ChainAdvanceInput names the chain to advance plus the optional
// DispatchHook the wire adapter injects.
type ChainAdvanceInput struct {
	StoryID      string
	Memberships  []string
	Now          time.Time
	DispatchHook DispatchHook
}

// ChainAdvanceOutput names the next dispatchable task id, whether
// the hook fired, and the terminal flag.
type ChainAdvanceOutput struct {
	StoryID    string      `json:"story_id"`
	NextTaskID string      `json:"next_task_id,omitempty"`
	Dispatched bool        `json:"dispatched"`
	Ack        DispatchAck `json:"ack,omitempty"`
	Terminal   bool        `json:"terminal"`
	Detail     string      `json:"detail,omitempty"`
}

// ChainRunInput loops ChainAdvance + poll until terminal.
type ChainRunInput struct {
	StoryID      string
	Memberships  []string
	PollInterval time.Duration
	Timeout      time.Duration
	DispatchHook DispatchHook
	Now          func() time.Time
	// Heartbeat is invoked once per loop iteration with the latest
	// advance result so callers (CLI `chain run`) can emit a JSON
	// heartbeat line for tail-able observability. Optional.
	Heartbeat func(ChainAdvanceOutput)
}

// ChainRunOutput summarises the loop's verdict.
type ChainRunOutput struct {
	StoryID       string   `json:"story_id"`
	Dispatched    []string `json:"dispatched_task_ids,omitempty"`
	TerminalState string   `json:"terminal_state"`
	Iterations    int      `json:"iterations"`
}

// chainPhaseSpec captures one row of the canonical chain order plus
// its predecessor requirement.
type chainPhaseSpec struct {
	Action string
	Kind   string
	Pred   *phaseRef
}

// phaseRef points at a predecessor phase. Kind=="" is the wildcard
// "work or review" the bash helper uses for the commit phase.
type phaseRef struct {
	Action string
	Kind   string
}

// canonicalPhases is the declarative chain order the router walks.
// Mirrors `.satellites/route_epic.sh` lines 23-31 so the substrate's
// view matches the operator's mental model post-sty_9d046bc7.
var canonicalPhases = []chainPhaseSpec{
	{Action: "contract:plan", Kind: task.KindWork, Pred: nil},
	{Action: "contract:develop", Kind: task.KindWork, Pred: &phaseRef{Action: "contract:plan", Kind: task.KindWork}},
	{Action: "contract:develop", Kind: task.KindReview, Pred: &phaseRef{Action: "contract:develop", Kind: task.KindWork}},
	{Action: "contract:story_review", Kind: task.KindWork, Pred: &phaseRef{Action: "contract:develop", Kind: task.KindReview}},
	{Action: "contract:story_review", Kind: task.KindReview, Pred: &phaseRef{Action: "contract:develop", Kind: task.KindReview}},
	{Action: "contract:commit", Kind: task.KindWork, Pred: &phaseRef{Action: "contract:story_review", Kind: ""}},
	{Action: "contract:merge_to_main", Kind: task.KindWork, Pred: &phaseRef{Action: "contract:commit", Kind: task.KindWork}},
	{Action: "contract:deploy", Kind: task.KindWork, Pred: &phaseRef{Action: "contract:merge_to_main", Kind: task.KindWork}},
}

// DefaultChainPollInterval is the cadence the chain run loop uses
// when ChainRunInput.PollInterval is zero.
const DefaultChainPollInterval = 30 * time.Second

// TerminalStateStoryClosed indicates the story has reached status=done.
const TerminalStateStoryClosed = "story-closed"

// TerminalStateNoDispatchable indicates no phase is dispatchable
// (a closed/failure leaf with no successor — the "jammed" signal).
const TerminalStateNoDispatchable = "no-dispatchable"

// TerminalStateTimeout indicates the Timeout deadline elapsed.
const TerminalStateTimeout = "timeout"

// ChainStatus returns a read-only snapshot of the story's chain.
func (c *Client) ChainStatus(ctx context.Context, caller Caller, in ChainStatusInput) (ChainStatusOutput, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil {
		return ChainStatusOutput{}, errors.New("chain_status unavailable: story or task store missing")
	}
	if in.StoryID == "" {
		return ChainStatusOutput{}, errors.New("story_id required")
	}
	walk, err := c.TaskWalk(ctx, caller, TaskWalkInput{StoryID: in.StoryID, Memberships: in.Memberships})
	if err != nil {
		return ChainStatusOutput{}, err
	}
	return computeChainStatus(walk), nil
}

// ChainAdvance computes the next dispatchable task id per the
// canonical phase order with auto-supersession-aware live selection.
// When DispatchHook is non-nil and the chain is not terminal AND the
// selected live task is `published`, the hook is invoked and the ack
// is mirrored to the output.
func (c *Client) ChainAdvance(ctx context.Context, caller Caller, in ChainAdvanceInput) (ChainAdvanceOutput, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil {
		return ChainAdvanceOutput{}, errors.New("chain_advance unavailable: story or task store missing")
	}
	if in.StoryID == "" {
		return ChainAdvanceOutput{}, errors.New("story_id required")
	}
	walk, err := c.TaskWalk(ctx, caller, TaskWalkInput{StoryID: in.StoryID, Memberships: in.Memberships})
	if err != nil {
		return ChainAdvanceOutput{}, err
	}
	status := computeChainStatus(walk)
	out := ChainAdvanceOutput{
		StoryID:    in.StoryID,
		NextTaskID: status.NextTaskID,
		Terminal:   status.Terminal,
	}
	if status.NextTaskID == "" || status.Terminal {
		return out, nil
	}
	// Determine the live status of the selected next task. If it's
	// not `published` the router is idempotent — the next call sees
	// the same row, and the caller decides whether to wait.
	live := findTaskByID(walk.Tasks, status.NextTaskID)
	if live == nil {
		return out, nil
	}
	if live.Status != task.StatusPublished {
		out.Detail = "already in flight"
		return out, nil
	}
	if in.DispatchHook == nil {
		// MCP variant — substrate never owns the daemon socket. The
		// caller dispatches via task_run.
		return out, nil
	}
	ack, herr := in.DispatchHook(ctx, status.NextTaskID)
	if herr != nil {
		return out, herr
	}
	out.Dispatched = ack.Dispatched
	out.Ack = ack
	return out, nil
}

// ChainRun loops ChainAdvance + sleep until terminal or timeout.
func (c *Client) ChainRun(ctx context.Context, caller Caller, in ChainRunInput) (ChainRunOutput, error) {
	if in.StoryID == "" {
		return ChainRunOutput{}, errors.New("story_id required")
	}
	poll := in.PollInterval
	if poll <= 0 {
		poll = DefaultChainPollInterval
	}
	clock := in.Now
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	var deadline time.Time
	if in.Timeout > 0 {
		deadline = clock().Add(in.Timeout)
	}
	out := ChainRunOutput{StoryID: in.StoryID}
	for {
		if !deadline.IsZero() && !clock().Before(deadline) {
			out.TerminalState = TerminalStateTimeout
			return out, nil
		}
		out.Iterations++
		adv, err := c.ChainAdvance(ctx, caller, ChainAdvanceInput{
			StoryID:      in.StoryID,
			Memberships:  in.Memberships,
			Now:          clock(),
			DispatchHook: in.DispatchHook,
		})
		if err != nil {
			return out, err
		}
		if in.Heartbeat != nil {
			in.Heartbeat(adv)
		}
		if adv.Dispatched && adv.NextTaskID != "" {
			out.Dispatched = append(out.Dispatched, adv.NextTaskID)
		}
		if adv.Terminal {
			// Distinguish "story closed" from "no-dispatchable jam"
			// by re-reading the chain status (cheap — one extra
			// query on the terminal iteration).
			snap, serr := c.ChainStatus(ctx, caller, ChainStatusInput{StoryID: in.StoryID, Memberships: in.Memberships})
			if serr != nil {
				return out, serr
			}
			if snap.StoryStatus == story.StatusDone {
				out.TerminalState = TerminalStateStoryClosed
			} else {
				out.TerminalState = TerminalStateNoDispatchable
			}
			return out, nil
		}
		// Respect ctx cancellation while sleeping.
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// computeChainStatus is the pure projection from a TaskWalkOutput onto
// a ChainStatusOutput. Extracted so the ChainAdvance + ChainStatus +
// table-driven tests share one body.
func computeChainStatus(walk TaskWalkOutput) ChainStatusOutput {
	out := ChainStatusOutput{
		StoryID:     walk.Story.ID,
		StoryStatus: walk.Story.Status,
	}
	// Bucket tasks by (Action, Kind). Each bucket is sorted ASC by
	// CreatedAt — selectLiveTask scans newest-first via index walks.
	type bucketKey struct {
		Action string
		Kind   string
	}
	buckets := make(map[bucketKey][]TaskWalkTask, len(canonicalPhases))
	for _, t := range walk.Tasks {
		if t.Action == "" {
			continue
		}
		key := bucketKey{Action: t.Action, Kind: t.Kind}
		buckets[key] = append(buckets[key], t)
	}
	for k := range buckets {
		rows := buckets[k]
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		})
		buckets[k] = rows
	}

	live := func(action, kind string) *TaskWalkTask {
		return selectLiveTask(buckets[bucketKey{Action: action, Kind: kind}])
	}

	out.Phases = make([]ChainPhase, 0, len(canonicalPhases))
	nextTaskID := ""
	for _, spec := range canonicalPhases {
		row := live(spec.Action, spec.Kind)
		ph := ChainPhase{Action: spec.Action, Kind: spec.Kind}
		if row != nil {
			ph.LiveTaskID = row.ID
			ph.LiveStatus = row.Status
			ph.LiveOutcome = row.Outcome
			ph.Iterations = len(buckets[bucketKey{Action: spec.Action, Kind: spec.Kind}])
		}
		out.Phases = append(out.Phases, ph)

		if nextTaskID != "" {
			continue
		}
		if row == nil {
			continue
		}
		// Eligibility: predecessor closed=success AND live is dispatchable.
		if !isDispatchable(row) {
			continue
		}
		if !predecessorSatisfied(spec, live) {
			continue
		}
		nextTaskID = row.ID
	}
	out.NextTaskID = nextTaskID
	if nextTaskID == "" {
		out.Terminal = true
	}
	// Anomaly: a non-terminal-story chain where the latest row of
	// some phase is closed=failure with no published successor.
	if out.StoryStatus != story.StatusDone {
		for _, spec := range canonicalPhases {
			rows := buckets[bucketKey{Action: spec.Action, Kind: spec.Kind}]
			if len(rows) == 0 {
				continue
			}
			latest := rows[len(rows)-1]
			if latest.Status == task.StatusClosed && latest.Outcome == task.OutcomeFailure {
				if !hasPublishedSuccessor(rows) {
					out.Anomalies = append(out.Anomalies, "phase_blocked:"+spec.Action+":"+spec.Kind+":"+latest.ID)
				}
			}
		}
	}
	return out
}

// selectLiveTask is the bucket-level picker. Mirrors
// `.satellites/route_epic.sh:36-47`:
//
//  1. Latest closed=success row — the canonical success.
//  2. Else latest in {published, claimed, in_flight} — the live attempt.
//  3. Else latest row (which will be closed/failure).
//
// rows is assumed CreatedAt ASC.
func selectLiveTask(rows []TaskWalkTask) *TaskWalkTask {
	if len(rows) == 0 {
		return nil
	}
	// Closed=success preferred — newest first.
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Status == task.StatusClosed && r.Outcome == task.OutcomeSuccess {
			return &rows[i]
		}
	}
	// Else newest of {published, claimed, in_flight}.
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		switch r.Status {
		case task.StatusPublished, task.StatusClaimed, task.StatusInFlight:
			return &rows[i]
		}
	}
	// Else fall-back: newest row (typically closed/failure).
	last := rows[len(rows)-1]
	return &last
}

// isDispatchable reports whether the live task is the one the router
// should hand to the daemon — i.e. `published`. Claimed/in_flight
// rows are already underway; ChainAdvance returns Dispatched=false and
// the run loop polls again next iteration.
func isDispatchable(row *TaskWalkTask) bool {
	if row == nil {
		return false
	}
	switch row.Status {
	case task.StatusPublished, task.StatusClaimed, task.StatusInFlight:
		return true
	}
	return false
}

// predecessorSatisfied implements the per-phase predecessor check.
// Pred==nil means "no predecessor required" (plan/work). Pred.Kind==""
// is the work-or-review wildcard the bash helper uses for the commit
// phase.
func predecessorSatisfied(spec chainPhaseSpec, live func(string, string) *TaskWalkTask) bool {
	if spec.Pred == nil {
		return true
	}
	if spec.Pred.Kind == "" {
		pw := live(spec.Pred.Action, task.KindWork)
		pr := live(spec.Pred.Action, task.KindReview)
		return isClosedSuccess(pw) || isClosedSuccess(pr)
	}
	return isClosedSuccess(live(spec.Pred.Action, spec.Pred.Kind))
}

// isClosedSuccess reports the closed=success terminal.
func isClosedSuccess(row *TaskWalkTask) bool {
	if row == nil {
		return false
	}
	return row.Status == task.StatusClosed && row.Outcome == task.OutcomeSuccess
}

// hasPublishedSuccessor reports whether the bucket has any
// non-closed-failure successor newer than its latest closed-failure
// row — the supersession sibling.
func hasPublishedSuccessor(rows []TaskWalkTask) bool {
	// Find the latest closed=failure index; check that any newer row
	// is non-terminal or closed=success.
	lastFailIdx := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Status == task.StatusClosed && rows[i].Outcome == task.OutcomeFailure {
			lastFailIdx = i
			break
		}
	}
	if lastFailIdx < 0 {
		return true
	}
	for i := lastFailIdx + 1; i < len(rows); i++ {
		switch rows[i].Status {
		case task.StatusPublished, task.StatusClaimed, task.StatusInFlight:
			return true
		case task.StatusClosed:
			if rows[i].Outcome == task.OutcomeSuccess {
				return true
			}
		}
	}
	return false
}

// findTaskByID is a small lookup helper for ChainAdvance's
// dispatch-eligibility branch.
func findTaskByID(rows []TaskWalkTask, id string) *TaskWalkTask {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

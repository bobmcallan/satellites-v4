package story

import "errors"

// Status values a Story can occupy. Terminal states (done, cancelled) do
// not transition out.
//
// Blocked is a non-terminal escalation state introduced by
// epic:v4-lifecycle-refactor (sty_bbe732af). When the review-iteration
// cap is hit on a contract, the retry chain authored by the reviewer's
// contract prose flips the story to blocked instead of appending
// another retry — the user must resolve the impasse before work
// resumes (typically by relaxing the AC, splitting the story, or
// amending the plan). Blocked → in_progress resumes; blocked →
// cancelled gives up.
const (
	StatusBacklog    = "backlog"
	StatusReady      = "ready"
	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusDone       = "done"
	StatusCancelled  = "cancelled"
)

// ErrInvalidTransition is returned when UpdateStatus is asked to move a
// story into an unreachable target state.
var ErrInvalidTransition = errors.New("story: invalid status transition")

// ValidTransition reports whether a story may move from → to. V3
// parity (sty_01f75142): operators may change a non-terminal story's
// status to any other non-self status. Self-transitions are rejected
// (no-op writes are a caller bug) and terminal states (done,
// cancelled) refuse further transitions.
//
// Matrix:
//
//	from \ to   backlog ready in_progress blocked done cancelled
//	backlog       -      ✓       ✓         ✓       ✓       ✓
//	ready         ✓      -       ✓         ✓       ✓       ✓
//	in_progress   ✓      ✓       -         ✓       ✓       ✓
//	blocked       ✓      ✓       ✓         -       ✓       ✓
//	done          -      -       -         -       -       -
//	cancelled     -      -       -         -       -       -
func ValidTransition(from, to string) bool {
	if from == to {
		return false
	}
	switch from {
	case StatusBacklog, StatusReady, StatusInProgress, StatusBlocked:
		return IsKnownStatus(to)
	case StatusDone, StatusCancelled:
		return false
	default:
		return false
	}
}

// IsKnownStatus reports whether s is one of the declared status strings.
// Used by Store.Create to validate the initial status.
func IsKnownStatus(s string) bool {
	switch s {
	case StatusBacklog, StatusReady, StatusInProgress, StatusBlocked, StatusDone, StatusCancelled:
		return true
	}
	return false
}

// isTerminalStatus reports whether s is one of the immutable terminal
// states the open-tasks gate guards against. Sty_0233fabd.
func isTerminalStatus(s string) bool {
	return s == StatusDone || s == StatusCancelled
}

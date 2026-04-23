package story

import "errors"

// Status values a Story can occupy. Terminal states (done, cancelled) do
// not transition out.
const (
	StatusBacklog    = "backlog"
	StatusReady      = "ready"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusCancelled  = "cancelled"
)

// ErrInvalidTransition is returned when UpdateStatus is asked to move a
// story into an unreachable target state.
var ErrInvalidTransition = errors.New("story: invalid status transition")

// ValidTransition reports whether a story may move from → to. Self-
// transitions are rejected (no-op writes are a caller bug) and every
// terminal state refuses further transitions.
//
// Matrix:
//
//	from \ to   backlog ready in_progress done cancelled
//	backlog       -      ✓       -         -       ✓
//	ready         -      -       ✓         -       ✓
//	in_progress   -      -       -         ✓       ✓
//	done          -      -       -         -       -
//	cancelled     -      -       -         -       -
func ValidTransition(from, to string) bool {
	switch from {
	case StatusBacklog:
		return to == StatusReady || to == StatusCancelled
	case StatusReady:
		return to == StatusInProgress || to == StatusCancelled
	case StatusInProgress:
		return to == StatusDone || to == StatusCancelled
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
	case StatusBacklog, StatusReady, StatusInProgress, StatusDone, StatusCancelled:
		return true
	}
	return false
}

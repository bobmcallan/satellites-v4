// Package session is the satellites-v4 session registry — the record of
// which Claude Code harness chat UUIDs the SessionStart hook has
// registered for which users. The process-order gate in
// `story_contract_claim` (slice 8.3) reads this registry to decide
// whether an incoming claim's session_id is one the server knows about
// and has seen recently enough to act on.
package session

import "time"

// StalenessDefault is the cut-off after which a session is treated as
// "stale" — a claim attempt against a session whose last_seen_at is
// older than this returns session_stale. Overridable via
// SATELLITES_SESSION_STALENESS (see handler).
const StalenessDefault = 30 * time.Minute

// Source enum values. Kept low-cardinality so logs can group them.
const (
	SourceSessionStart = "session_start"
	SourceEnforceHook  = "enforce_hook"
	SourceAPIKey       = "apikey"
)

// Session records a registered harness session. (UserID, SessionID) is
// the primary key. LastSeenAt is refreshed on every claim so an active
// agent keeps its slot alive.
//
// OrchestratorGrantID is populated by the SessionStart path in
// story_7d9c4b1b: when the server has a RoleGrantStore wired and the
// system-scope orchestrator docs (role_orchestrator, agent_claude_orchestrator)
// are seeded, Register mints a role-grant on behalf of the session and
// stores the returned grant id here. Empty when the wiring is absent.
type Session struct {
	UserID              string    `json:"user_id"`
	SessionID           string    `json:"session_id"`
	Source              string    `json:"source"`
	Registered          time.Time `json:"registered_at"`
	LastSeenAt          time.Time `json:"last_seen_at"`
	OrchestratorGrantID string    `json:"orchestrator_grant_id,omitempty"`
}

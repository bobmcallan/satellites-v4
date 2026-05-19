// Package auth — verb allowlist gate (sty_056b68f6).
//
// The gate fires exactly once per verb invocation: once on the MCP
// transport via `addGatedTool`, once on the HTTP transport via the
// `gateHTTPVerbs` middleware. Both call sites pass the verb name
// and the request ctx; the gate reads `CallerIdentity` off ctx via
// `UserFrom` and reads `AllowedVerbs` off the identity.
//
// Bypass rule: when `caller.AllowedVerbs == nil` the gate returns
// nil — un-narrowed callers (project-scoped keys / OAuth / session)
// stay unrestricted. The gate is exclusively for task-scoped keys
// whose `agent_apikey_create(task_id=…)` mint populated the field.
//
// pr_no_unrequested_compat: there is NO env-var bypass, NO per-call
// escalation flag. Operators who genuinely need broader access
// author a fresh story that updates `pr_role_grid` first.
package auth

import (
	"context"
	"encoding/json"
)

// VerbDeniedError is the typed rejection returned by
// AssertVerbAllowed when the caller's task-scoped allowlist does
// not include the requested verb. `Body()` renders the wire envelope
// `{"error":"verb_not_in_allowlist","verb":"<v>","allowed":[…]}`
// the transport layer writes verbatim.
type VerbDeniedError struct {
	Verb    string
	Allowed []string
}

// Error returns a stable machine-readable code (the wire layer
// renders `Body()` for the JSON envelope).
func (e *VerbDeniedError) Error() string { return "verb_not_in_allowlist" }

// Body returns the JSON envelope the transport writes as the
// rejection payload. Fields: error (constant), verb (the rejected
// name), allowed (the caller's resolved allowlist — empty array
// when the caller has an explicit empty list).
func (e *VerbDeniedError) Body() string {
	allowed := e.Allowed
	if allowed == nil {
		allowed = []string{}
	}
	b, _ := json.Marshal(map[string]any{
		"error":   "verb_not_in_allowlist",
		"verb":    e.Verb,
		"allowed": allowed,
	})
	return string(b)
}

// AssertVerbAllowed returns nil when the caller may invoke verb and
// a *VerbDeniedError otherwise. The bypass condition is "the caller
// has no allowlist" — i.e. project-scoped keys / OAuth / session
// callers leave `AllowedVerbs` nil and the gate is a no-op for them.
// Task-scoped callers (TaskID != "" implies AllowedVerbs is
// populated — even an empty slice means "no verbs allowed") run
// through the membership check.
func AssertVerbAllowed(ctx context.Context, verb string) *VerbDeniedError {
	caller, ok := UserFrom(ctx)
	if !ok {
		return nil
	}
	// Un-narrowed caller — bypass the gate.
	if caller.AllowedVerbs == nil {
		return nil
	}
	for _, v := range caller.AllowedVerbs {
		if v == verb {
			return nil
		}
	}
	return &VerbDeniedError{Verb: verb, Allowed: caller.AllowedVerbs}
}

// VerbAllowlistSubset reports whether subset is a (possibly empty)
// subset of role's defaults. Returns the offending verbs (those in
// subset but not in role's defaults). The mint path
// (`AgentAPIKeyCreate` with `allowed_verbs=…`) calls this to enforce
// AC6: explicit allowed_verbs at mint time may SHRINK but never
// EXPAND a role's default.
func VerbAllowlistSubset(role string, subset []string) (ok bool, offending []string) {
	defaults := roleVerbDefaults[role]
	if defaults == nil {
		// Unknown role rejects everything.
		return false, append([]string(nil), subset...)
	}
	for _, v := range subset {
		if _, in := defaults[v]; !in {
			offending = append(offending, v)
		}
	}
	return len(offending) == 0, offending
}

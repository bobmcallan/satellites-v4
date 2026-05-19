// Package auth — role → verb default map (sty_056b68f6).
//
// `pr_role_grid` is the design-time authority for which verbs each
// of the three roles (orchestration / execution / review) may call.
// This file is the runtime expression of that grid: a Go map keyed
// by role name whose value is the closed set of verbs the role may
// invoke. `agent_apikey_create(task_id=…)` derives a task-scoped
// key's `allowed_verbs` from this map via `DefaultsForRole`; the
// transport gate (`AssertVerbAllowed`) reads the populated allowlist
// off `CallerIdentity` and rejects out-of-set calls with the wire
// envelope `{"error":"verb_not_in_allowlist", "verb": "<v>", "allowed": [...]}`.
//
// A configseed lint at `role_verbs_test.go` re-parses the principle
// markdown table at test time and asserts row-for-row equality so
// the Go map cannot drift from the seeded grid silently.
package auth

import "sort"

// Role names — closed set per pr_role_grid. A fourth role is a
// separate story that updates the grid first.
const (
	RoleOrchestration = "orchestration"
	RoleExecution     = "execution"
	RoleReview        = "review"
)

// roleVerbDefaults is the canonical role → verb set, derived from
// the grid in `config/seed/wksp_5b3257d1/principles/role_grid.md`.
// Mutation outside this file is forbidden; the package exports
// DefaultsForRole / IsAllowedForRole as the only read paths.
//
// "inlined" cells from the grid (where the orchestrator pre-renders
// the relevant document into the task prompt) are NOT entries here —
// the executor never calls those verbs itself, so they never appear
// on an execution-role key's allowlist. Reviewers DO call read verbs
// themselves; those cells appear under `review`.
var roleVerbDefaults = map[string]map[string]struct{}{
	RoleOrchestration: setOf(
		// Orchestration owns the full authoring surface.
		"story_add",
		"story_update",
		"story_get",
		"story_list",
		"story_close",
		"task_add",
		"task_update",
		"task_get",
		"task_list",
		"task_claim",
		"task_walk",
		"ledger_append",
		"ledger_list",
		"ledger_get",
		"ledger_search",
		"ledger_recall",
		"ledger_dereference",
		"document_get",
		"document_list",
		"principle_get",
		"principle_list",
		"agent_get",
		"agent_list",
		"contract_get",
		"contract_list",
		"skill_get",
		"skill_list",
		"reviewer_list",
		"role_list",
		"repo_search",
		"repo_search_text",
		"repo_get_file",
		"repo_get_outline",
		"repo_get_symbol_source",
		"repo_add",
		"repo_get",
		"repo_list",
		"kv_get",
		"kv_set",
		"kv_delete",
		"kv_get_resolved",
		"kv_list",
		"agent_apikey_create",
		"agent_apikey_list",
		"agent_apikey_delete",
		"project_set",
		"project_get",
		"project_list",
		"project_add",
		"project_update",
		"project_delete",
		"satellites_info",
		"satellites_init",
		"satellites_help",
		"satellites_exec",
		"system_version",
		"substrate_audit",
		"chain_status",
		"chain_advance",
		"chain_run",
		"task_log_append",
		"task_log_list",
		"session_whoami",
		"session_register",
	),
	RoleExecution: setOf(
		// Execution can close its own task and append to its own
		// task's ledger tag. story_get / task_walk / ledger_list
		// are "inlined" by the orchestrator — not on the wire.
		// Same with principle_list / agent_list / contract_list /
		// document_get — pre-rendered into the prompt.
		"task_update",   // self → closed
		"ledger_append", // own task tag (cross-task is rejected by ledger validation)
		"satellites_info",
		"satellites_init",
		"system_version",
		"task_get",         // self lookups for the runner shell
		"task_log_append",  // own task lifecycle telemetry
		"agent_apikey_list", // see own key's metadata
	),
	RoleReview: setOf(
		// Review can read broadly and append to its own task's
		// ledger tag — but cannot author stories or tasks.
		"story_get",
		"story_list",
		"task_walk",
		"task_get",
		"task_update", // self → closed
		"ledger_list",
		"ledger_get",
		"ledger_search",
		"ledger_recall",
		"ledger_append", // own task tag (verdict authoring)
		"document_get",
		"document_list",
		"principle_get",
		"principle_list",
		"agent_get",
		"agent_list",
		"contract_get",
		"contract_list",
		"skill_get",
		"skill_list",
		"repo_search",
		"repo_search_text",
		"repo_get_file",
		"repo_get_outline",
		"repo_get_symbol_source",
		"satellites_info",
		"satellites_init",
		"system_version",
		"task_log_append",
		"task_log_list",
	),
}

// setOf returns a set-shaped map[string]struct{} from the variadic
// list. Tiny helper to keep the role table declarative.
func setOf(verbs ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(verbs))
	for _, v := range verbs {
		m[v] = struct{}{}
	}
	return m
}

// DefaultsForRole returns the verb allowlist for role, sorted for
// deterministic output. Unknown roles return nil — the caller
// (AgentAPIKeyCreate) treats nil as "reject the mint".
func DefaultsForRole(role string) []string {
	s, ok := roleVerbDefaults[role]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(s))
	for v := range s {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// IsAllowedForRole reports whether verb is in role's default set.
// Unknown role returns false.
func IsAllowedForRole(role, verb string) bool {
	s, ok := roleVerbDefaults[role]
	if !ok {
		return false
	}
	_, ok = s[verb]
	return ok
}

// KnownRoles returns the closed set of role names this package
// recognises. Sorted for deterministic test output.
func KnownRoles() []string {
	return []string{RoleExecution, RoleOrchestration, RoleReview}
}

// Package chaincoverage resolves the workspace-resolved workflow's
// canonical chain contracts against the document store's tier ladder
// (project → workspace → system). Returns one entry per chain
// contract — with the tier it resolved at + its doc id — plus a
// list of names that did not resolve anywhere.
//
// The package is the shared primitive consumed by:
//
//   - `satellites_init` (this story, sty_ab0b47d6) for the
//     orientation-payload `chain_coverage` field; surfaces missing
//     contracts to the operator at boot.
//   - The audit verb planned in sty_2f0db922 (continuous drift
//     detection) — which will call this loop on a schedule and
//     append `kind:audit-evidence` ledger rows when chain coverage
//     regresses.
//
// Locating the loop in a shared package up-front avoids a second
// extraction when the audit verb ships.
package chaincoverage

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/bobmcallan/satellites/internal/document"
)

// CanonicalContracts returns the canonical chain contract names per
// the system-tier `default` workflow's prose. Stable order: the
// chain shape as it executes left-to-right. The list is hard-coded
// here rather than parsed from the workflow body so a consumer
// reading the audit verb's structured output does not need to
// re-parse markdown to know what was checked.
//
// Source of truth: the system-tier workflow body
// (`config/seed/system/workflows/default.md`). When that shape
// changes, this list moves in lockstep.
func CanonicalContracts() []string {
	return []string{
		"plan",
		"develop",
		"commit",
		"merge_to_main",
		"story_review",
	}
}

// ContractEntry is one chain-coverage entry. FoundAt names the tier
// the contract document resolved at (project | workspace | system);
// DocID is the resolved document's id; both are empty when the
// contract did not resolve anywhere.
type ContractEntry struct {
	Name    string `json:"name"`
	FoundAt string `json:"found_at,omitempty"`
	DocID   string `json:"doc_id,omitempty"`
}

// Result is the per-workspace chain-coverage report. WorkflowSource
// names the tier the workspace-resolved `default` workflow doc
// resolved at; Contracts is the canonical chain enumerated with each
// contract's tier + doc id; MissingContracts collects names that did
// not resolve anywhere.
type Result struct {
	WorkflowSource   string          `json:"workflow_source"`
	WorkflowDocID    string          `json:"workflow_doc_id,omitempty"`
	Contracts        []ContractEntry `json:"contracts"`
	MissingContracts []string        `json:"missing_contracts"`
}

// Resolve runs the chain-coverage loop. The supplied document store
// resolves contract names through ResolveByName, walking the tier
// ladder (project → workspace → system) per the substrate's
// precedence rules. Returns a Result the caller embeds in its
// orientation payload.
//
// names defaults to CanonicalContracts when nil.
func Resolve(ctx context.Context, docs document.Store, workspaceID, projectID string, memberships []string, names []string) (Result, error) {
	if names == nil {
		names = CanonicalContracts()
	}
	result := Result{
		Contracts:        make([]ContractEntry, 0, len(names)),
		MissingContracts: []string{},
	}

	// Workflow source resolution: prefer workspace, fall back to
	// system. ResolveByName walks the tier ladder; the returned
	// document's Scope identifies which tier won.
	if doc, err := docs.ResolveByName(ctx, document.TypeWorkflow, "default", workspaceID, projectID, memberships); err == nil {
		result.WorkflowSource = doc.Scope
		result.WorkflowDocID = doc.ID
	}

	// Per-contract resolution: same ResolveByName + tier-ladder walk.
	for _, name := range names {
		entry := ContractEntry{Name: name}
		if doc, err := docs.ResolveByName(ctx, document.TypeContract, name, workspaceID, projectID, memberships); err == nil {
			entry.FoundAt = doc.Scope
			entry.DocID = doc.ID
		} else {
			result.MissingContracts = append(result.MissingContracts, name)
		}
		result.Contracts = append(result.Contracts, entry)
	}

	// Stable order on the missing-contracts list keeps payload diffs
	// readable across boots.
	sort.Strings(result.MissingContracts)
	return result, nil
}

// RecommendationText composes the operator-facing recommendation
// string the satellites_init verb stamps onto its chain_coverage
// field. Empty missing list → "all canonical contracts resolved";
// otherwise the missing names + per-name authoring instructions.
//
// The text is kept here so the audit verb in sty_2f0db922 emits the
// same prose without duplicating it across packages.
func RecommendationText(workspaceID string, result Result) string {
	if len(result.MissingContracts) == 0 {
		return "All canonical chain contracts resolved. No action required."
	}
	wsHint := workspaceID
	if wsHint == "" {
		wsHint = "<workspace>"
	}
	missing := strings.Join(result.MissingContracts, ", ")
	return "Workflow `default` references contracts that do not resolve at any tier: " +
		missing + ". Author each one at config/seed/" + wsHint +
		"/contracts/<name>.md (workspace scope) or at the project/system tier if the contract is broadly applicable, then reseed."
}

// canonicalContractPattern is the regex the audit verb (and tests)
// use to identify canonical-chain contract action strings on task
// chains. Kept alongside CanonicalContracts so the two move in
// lockstep when the chain shape evolves.
//
// TODO(sty_2f0db922): the future audit verb's chain-shape lint
// should consume this pattern + CanonicalContracts to flag chains
// that mix in retired contract names (e.g. "push" after the
// 2026-05-14 commit rename).
var canonicalContractPattern = regexp.MustCompile(`^contract:(plan|develop|commit|merge_to_main|story_review)$`)

// IsCanonicalContractAction reports whether the supplied
// `contract:<name>` action string names a canonical chain contract.
// Used by the audit verb's task-chain lint (sty_2f0db922).
func IsCanonicalContractAction(action string) bool {
	return canonicalContractPattern.MatchString(action)
}

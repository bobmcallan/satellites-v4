// Package wsbus declares the registry of WS event Kinds the substrate
// may emit on the websocket bus. sty_08fc8d20 introduced this package
// as the substrate primitive that prevents prefix-strip drift: every
// Kind a wshandler emits is registered here, and configseed's boot
// lint walks internal/wshandler/translate.go to assert every emit
// literal participates in this registry.
//
// Authority: pr_substrate_model — the registry IS the substrate
// primitive that prevents the next prefix-strip slip; encode it once,
// in code, not in commit messages. pr_root_cause — fixes the *class*
// (no registry → silent corruption when a new event family is added),
// not the symptom (one panel mis-rendering one Kind).
//
// Membership is sourced from the inline string composition in
// internal/wshandler/translate.go and the status constants in
// internal/task/task.go and internal/story/transitions.go. Every
// entry below must be acknowledged by:
//
//   - internal/wshandler/translate.go (emit site)
//   - pages/static/common.js or one of its sibling per-panel views
//     (panel-side dispatch — gated by an explicit-kind switch or a
//     `*_STATUS_KINDS` whitelist Set, never a prefix-strip)
//
// Any change to the registry membership requires the matching
// dispatcher/emit edits or the configseed lint aborts boot.
package wsbus

import (
	"fmt"
	"sort"
	"sync"

	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// registry is the singleton mutable store. Mutation happens once at
// package init and is otherwise frozen — callers that Register at
// runtime past init are misusing the package, so we panic on
// duplicate-register to surface that misuse loudly.
var (
	regMu  sync.RWMutex
	regSet = map[string]struct{}{}
)

// Register adds a Kind to the registry. Panics if the Kind is empty
// or already registered. Package init calls this for the known
// canonical set; tests may Register additional Kinds inside their own
// scope by also calling Unregister on teardown (not exposed
// production-side — Register is the only mutation surface and panics
// on duplicate to flush double-init bugs).
func Register(kind string) {
	if kind == "" {
		panic("wsbus.Register: kind must not be empty")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := regSet[kind]; exists {
		panic(fmt.Sprintf("wsbus.Register: duplicate kind %q", kind))
	}
	regSet[kind] = struct{}{}
}

// IsRegistered reports whether kind is in the registry.
func IsRegistered(kind string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	_, ok := regSet[kind]
	return ok
}

// LookupOrError returns the input Kind unchanged when it is
// registered, or a non-nil error when it is not. Emit sites in
// internal/wshandler/translate.go call this immediately before
// emitting a WireEvent — an unregistered Kind is a coding bug; the
// emit drops and the error surfaces in the caller's log so the next
// boot cycle's configseed lint catches the literal source.
func LookupOrError(kind string) (string, error) {
	if IsRegistered(kind) {
		return kind, nil
	}
	return "", fmt.Errorf("wsbus: kind %q is not registered (sty_08fc8d20 — see internal/wsbus/event_kinds.go)", kind)
}

// Kinds returns a sorted snapshot of every registered Kind. Used by
// the configseed lint and by registry tests; production emit paths
// should use LookupOrError instead so a miss surfaces at the emit
// call site, not at the registry boundary.
func Kinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(regSet))
	for k := range regSet {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() {
	// Task statuses. Sourced from internal/task/task.go const block.
	// `enqueued` is the legacy alias that migrates to `published` on
	// next boot — it stays in the registry so any in-flight emit
	// against an old row at boot transition still validates.
	for _, status := range []string{
		task.StatusPlanned,
		task.StatusPublished,
		task.StatusEnqueued,
		task.StatusClaimed,
		task.StatusInFlight,
		task.StatusClosed,
		task.StatusArchived,
	} {
		Register("task." + status)
	}
	// Story statuses. Sourced from internal/story/transitions.go.
	for _, status := range []string{
		story.StatusBacklog,
		story.StatusReady,
		story.StatusInProgress,
		story.StatusBlocked,
		story.StatusDone,
		story.StatusCancelled,
	} {
		Register("story." + status)
	}
	// Non-status event families. The first surfaces a ledger CREATE
	// scoped to a story (sty_e55f335e). The latter pair is the
	// ledger.append / ledger.dereference stream (sty_9f658001).
	Register("story.activity.append")
	Register("ledger.append")
	Register("ledger.dereference")

	// Document / contract status families. Sourced from
	// sty_bc732746's translateDocument: emits document.<status> on
	// CREATE/UPDATE and contract.<status> when the row's type is
	// "contract". The schema enum at internal/document/document.go
	// declares {active, archived}; sty_bc732746's translator tests
	// also exercise {in_progress, done} as transitional fixtures —
	// both registered so the runtime LookupOrError gate doesn't drop
	// emits the sibling story's tests rely on.
	for _, status := range []string{
		"active",
		"archived",
		"in_progress",
		"done",
	} {
		Register("document." + status)
		Register("contract." + status)
	}
	// Repo / commit event families. Sourced from sty_bc732746's
	// translateRepo + translateCommit. CREATE → repo.created;
	// UPDATE → repo.updated or repo.archived; commits CREATE →
	// repo.commit_appended.
	Register("repo.created")
	Register("repo.updated")
	Register("repo.archived")
	Register("repo.commit_appended")
	// Project event families. Sourced from sty_bc732746's
	// translateProject. UPDATE → project.updated; DELETE →
	// project.deleted.
	Register("project.updated")
	Register("project.deleted")
}

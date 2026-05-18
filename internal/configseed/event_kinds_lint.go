// sty_08fc8d20 — configseed lints the WS event-kind emit surface
// against the wsbus registry at boot. The lint reads
// internal/wshandler/translate.go and extracts every Kind literal
// composed into an emit call, then asserts each is registered in
// internal/wsbus. An unregistered Kind surfaces as a Summary error
// so the runner can abort boot before the substrate emits a frame a
// panel cannot route.
//
// Authority: pr_substrate_model — the registry is THE substrate
// primitive; the lint is what makes it load-bearing. pr_root_cause —
// without the lint, a developer can land an emit literal that
// out-of-band drifts from the registry and silently corrupts a panel
// dispatch the next time a panel adds a prefix-strip handler.

package configseed

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"

	"github.com/bobmcallan/satellites/internal/wsbus"
)

// translateGoSearchPaths returns the candidate relative paths to
// internal/wshandler/translate.go from the configseed package. We
// resolve from the configseed source file location so the lint
// reads the same tree as the running binary (rather than depending
// on the working directory, which varies between tests / boot).
func translateGoSearchPaths() []string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil
	}
	// .../internal/configseed/event_kinds_lint.go → repo root is
	// two parents up.
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	return []string{filepath.Join(root, "internal", "wshandler", "translate.go")}
}

// emitKindLiteralRE matches Kind literals composed in the emit
// helper signature `emitWireEvent("<kind>", ...)`. The Kind is the
// quoted first argument. Composed forms like "task."+status are
// caught by the prefix-token regex below.
var emitKindLiteralRE = regexp.MustCompile(`emitWireEvent\("([^"]+)"`)

// emitKindPrefixRE matches the composed-prefix form
// `emitWireEvent("task."+status, …)` or `emitWireEvent("story."+status, …)`.
// Captures the prefix token; the lint expands the prefix against
// every known status set when checking the registry.
var emitKindPrefixRE = regexp.MustCompile(`emitWireEvent\("([a-z_.]+)"\+\s*status`)

// lintEventKinds reads internal/wshandler/translate.go and asserts
// that every Kind literal composed into emitWireEvent participates
// in the wsbus registry. Returns one ErrorEntry per orphan literal.
// A boot run with a non-empty result aborts before the substrate
// starts emitting frames.
func lintEventKinds() []ErrorEntry {
	candidates := translateGoSearchPaths()
	if len(candidates) == 0 {
		return []ErrorEntry{{Path: "internal/wshandler/translate.go", Reason: "configseed: cannot resolve translate.go path (runtime.Caller failure)"}}
	}
	var source []byte
	var path string
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err == nil {
			source = raw
			path = p
			break
		}
	}
	if source == nil {
		return []ErrorEntry{{Path: candidates[0], Reason: "configseed: translate.go not readable"}}
	}
	return lintEventKindsFromSource(path, source)
}

// lintEventKindsFromSource is the testable core. It walks the source
// text and returns one ErrorEntry per unregistered Kind composition.
// Exported (lower-case) name kept internal to package; the test file
// calls it directly with a synthesized fixture string.
func lintEventKindsFromSource(path string, source []byte) []ErrorEntry {
	registered := map[string]struct{}{}
	for _, k := range wsbus.Kinds() {
		registered[k] = struct{}{}
	}
	var errs []ErrorEntry
	// Literal Kinds — drop the prefix-form pseudo-literal captures
	// (e.g. "task." composed with status) which always end with `.`.
	for _, m := range emitKindLiteralRE.FindAllSubmatch(source, -1) {
		kind := string(m[1])
		if kind == "" {
			continue
		}
		if last := kind[len(kind)-1]; last == '.' {
			// Composed form — handled by the prefix matcher below.
			continue
		}
		if _, ok := registered[kind]; !ok {
			errs = append(errs, ErrorEntry{
				Path:   path,
				Reason: fmt.Sprintf("wsbus: emit literal %q not registered in internal/wsbus/event_kinds.go", kind),
			})
		}
	}
	// Composed prefixes — `emitWireEvent("task."+status, …)`. Resolve
	// the prefix against the registered Kind set: every registered
	// "task.<x>" Kind matches the "task." prefix. The lint asserts the
	// prefix has at least one registered child — an emit composed
	// against an empty prefix-family is the regression we are
	// preventing.
	for _, m := range emitKindPrefixRE.FindAllSubmatch(source, -1) {
		prefix := string(m[1])
		if prefix == "" {
			continue
		}
		found := false
		for k := range registered {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, ErrorEntry{
				Path:   path,
				Reason: fmt.Sprintf("wsbus: emit prefix %q has zero registered children in internal/wsbus/event_kinds.go", prefix),
			})
		}
	}
	// Sort for deterministic test output.
	sort.Slice(errs, func(i, j int) bool { return errs[i].Reason < errs[j].Reason })
	return errs
}

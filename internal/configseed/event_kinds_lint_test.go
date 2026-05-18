package configseed

import (
	"strings"
	"testing"
)

// TestLintEventKinds_RealTranslateGoIsClean asserts the in-tree
// internal/wshandler/translate.go emits only Kinds participating in
// wsbus. Locks the invariant the runner depends on at boot — if this
// test ever fails on main, either the registry or the emit literal
// has drifted.
func TestLintEventKinds_RealTranslateGoIsClean(t *testing.T) {
	t.Parallel()
	errs := lintEventKinds()
	if len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("lintEventKinds: %s: %s", e.Path, e.Reason)
		}
	}
}

// TestLintEventKinds_DetectsUnregisteredLiteral injects a fake
// translate.go fixture containing an unregistered Kind literal and
// asserts the lint catches it.
func TestLintEventKinds_DetectsUnregisteredLiteral(t *testing.T) {
	t.Parallel()
	fixture := []byte(`
package wshandler
func bogus() {
	out, ok := emitWireEvent("task.something.else", "wksp_x", nil)
	_ = out
	_ = ok
}
`)
	errs := lintEventKindsFromSource("fixture.go", fixture)
	if len(errs) == 0 {
		t.Fatalf("lint did not detect unregistered literal")
	}
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Reason, "task.something.else") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("lint errors %v missing the offending literal", errs)
	}
}

// TestLintEventKinds_DetectsOrphanedPrefix injects a fake fixture
// composing against a prefix with no registered children and asserts
// the lint catches it.
func TestLintEventKinds_DetectsOrphanedPrefix(t *testing.T) {
	t.Parallel()
	fixture := []byte(`
package wshandler
func bogus() {
	status := "x"
	out, ok := emitWireEvent("widget."+status, "wksp_x", nil)
	_ = out
	_ = ok
}
`)
	errs := lintEventKindsFromSource("fixture.go", fixture)
	if len(errs) == 0 {
		t.Fatalf("lint did not detect orphaned prefix")
	}
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Reason, "widget.") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("lint errors %v missing the offending prefix", errs)
	}
}

// TestLintEventKinds_AcceptsRegisteredPrefixCompose asserts that
// `emitWireEvent("task."+status, …)` is silently accepted because
// task.<status> Kinds are registered in wsbus.
func TestLintEventKinds_AcceptsRegisteredPrefixCompose(t *testing.T) {
	t.Parallel()
	fixture := []byte(`
package wshandler
func ok() {
	status := "planned"
	out, ok := emitWireEvent("task."+status, "wksp_x", nil)
	_ = out
	_ = ok
}
`)
	errs := lintEventKindsFromSource("fixture.go", fixture)
	if len(errs) != 0 {
		t.Errorf("lint flagged a registered prefix compose: %v", errs)
	}
}

package wsbus

import (
	"sort"
	"strings"
	"testing"
)

// expectedKinds mirrors the registry's init() contents — duplicated
// here so a future edit must touch both sides. Sourced from
// internal/wshandler/translate.go emit literals and the
// internal/task/task.go + internal/story/transitions.go status
// constants.
var expectedKinds = []string{
	"task.planned",
	"task.published",
	"task.enqueued",
	"task.claimed",
	"task.in_flight",
	"task.closed",
	"task.archived",
	"story.backlog",
	"story.ready",
	"story.in_progress",
	"story.blocked",
	"story.done",
	"story.cancelled",
	"story.activity.append",
	"ledger.append",
	"ledger.dereference",
	// sty_bc732746 panel families wired through the registry by the
	// iter-3 rebase resolution. document/contract carry the schema
	// enum {active, archived} plus the test-fixture statuses
	// {in_progress, done} the sibling story's translator tests
	// exercise.
	"document.active",
	"document.archived",
	"document.in_progress",
	"document.done",
	"contract.active",
	"contract.archived",
	"contract.in_progress",
	"contract.done",
	"repo.created",
	"repo.updated",
	"repo.archived",
	"repo.commit_appended",
	"project.updated",
	"project.deleted",
}

func TestKinds_RegistryMatchesExpectedSet(t *testing.T) {
	got := Kinds()
	want := append([]string(nil), expectedKinds...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Kinds length mismatch: got=%d want=%d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Kinds[%d]=%q want %q (full got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

func TestIsRegistered_PositiveAndNegative(t *testing.T) {
	for _, k := range expectedKinds {
		if !IsRegistered(k) {
			t.Errorf("IsRegistered(%q) = false; want true", k)
		}
	}
	for _, k := range []string{
		"task.something.else",
		"story.activity",
		"ledger.somethingelse",
		"",
		"task",
		"story.",
	} {
		if IsRegistered(k) {
			t.Errorf("IsRegistered(%q) = true; want false", k)
		}
	}
}

func TestLookupOrError_PositiveAndNegative(t *testing.T) {
	out, err := LookupOrError("task.planned")
	if err != nil {
		t.Errorf("LookupOrError(task.planned) err = %v; want nil", err)
	}
	if out != "task.planned" {
		t.Errorf("LookupOrError(task.planned) = %q; want %q", out, "task.planned")
	}
	_, err = LookupOrError("task.something.else")
	if err == nil {
		t.Fatalf("LookupOrError(task.something.else) err = nil; want non-nil")
	}
	if !strings.Contains(err.Error(), "task.something.else") {
		t.Errorf("error %v missing the offending kind in its message", err)
	}
	if !strings.Contains(err.Error(), "sty_08fc8d20") {
		t.Errorf("error %v missing the registry-source-of-truth pointer (sty_08fc8d20)", err)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("Register on duplicate kind did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not a string: %T %v", r, r)
		}
		if !strings.Contains(msg, "duplicate kind") {
			t.Errorf("panic message %q missing %q", msg, "duplicate kind")
		}
	}()
	Register("task.planned")
}

func TestRegister_EmptyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("Register(\"\") did not panic")
		}
	}()
	Register("")
}

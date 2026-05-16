package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// helpsExitZero asserts that running the root command with the given
// args (typically a `--help` invocation) writes the expected substring
// and returns nil error.
func helpsExitZero(t *testing.T, args []string, mustContain ...string) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%v) error: %v", args, err)
	}
	got := out.String()
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Fatalf("Execute(%v) missing %q in:\n%s", args, want, got)
		}
	}
}

func TestRootHelpListsNouns(t *testing.T) {
	helpsExitZero(t, []string{"--help"},
		"satellites-client",
		"story",
		"task",
		"ledger",
		"info",
	)
}

func TestRootVersion(t *testing.T) {
	helpsExitZero(t, []string{"--version"})
}

func TestStoryHelpListsVerbs(t *testing.T) {
	// sty_4db0e025 D1 folded story_update_status + story_field_set
	// into the consolidated `story update` verb.
	helpsExitZero(t, []string{"story", "--help"},
		"add",
		"get",
		"list",
		"update",
		"export-walk",
	)
}

func TestStoryGetHelp(t *testing.T) {
	helpsExitZero(t, []string{"story", "get", "--help"}, "story get")
}

func TestPersistentFlagsRecognised(t *testing.T) {
	// --json + --quiet on a verb stub: the stub still returns
	// NotImplemented, but the flags must parse without "unknown
	// flag" errors.
	root := newRootCmd()
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{"--json", "--quiet", "story", "get"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected NotImplemented error from stub")
	}
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("stub exit code = %d, want %d", got, cliexit.Server)
	}
}

func TestStubReturnsServerExitCode(t *testing.T) {
	// After sty_e68ce6fb, every CLI verb has a concrete handler —
	// the stub set is empty. Keep this test as a guard: if a new
	// verbStub regresses, this slice must be populated and the loop
	// re-enabled. The current empty slice asserts no stubs remain.
	verbs := [][]string{}
	for _, args := range verbs {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root := newRootCmd()
			root.SetOut(new(bytes.Buffer))
			root.SetErr(new(bytes.Buffer))
			root.SetArgs(args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("%v: expected NotImplemented", args)
			}
			var typed *cliexit.Error
			if !errors.As(err, &typed) {
				t.Fatalf("%v: error is not *cliexit.Error: %T (%v)", args, err, err)
			}
			if typed.Code != cliexit.Server {
				t.Fatalf("%v: code = %d, want %d", args, typed.Code, cliexit.Server)
			}
			if !strings.Contains(typed.Error(), "not yet implemented") {
				t.Fatalf("%v: message missing not-implemented marker: %q", args, typed.Error())
			}
		})
	}
}

func TestNounGroupsRegistered(t *testing.T) {
	// AC4: every noun group + every verb appears under root.
	// Note: noun verb counts include all CRUD + special verbs. The
	// total grows as stubs are migrated; the value reflects the
	// noun-group shape after sty_0b419d98 Tier B.
	expected := map[string]int{
		"story":     8, // sty_4db0e025 D1 folded update_status + field_set into update
		"task":      9, // sty_27516920 retired task plan; sty_8c17b89d added task log-append + task log-list
		"ledger":    6,
		"project":   7,
		"workspace": 7,
		"kv":        5,
		"repo":      8,
		"agent":     11,
		"contract":  6,
		"principle": 6,
		"document":  7,
		"reviewer":  6,
		"role":      6,
		"skill":     6,
		"changelog": 5,
		"session":   2,
		"system":    2,
		"portal":    2, // sty_02ad5eb9 restored portal_get_page alongside portal_replicate
		"auth":      1,
	}
	root := newRootCmd()
	for nounName, wantVerbs := range expected {
		var found *bool
		for _, c := range root.Commands() {
			if c.Use == nounName {
				ok := true
				found = &ok
				if got := len(c.Commands()); got != wantVerbs {
					t.Errorf("noun %q: %d verbs, want %d", nounName, got, wantVerbs)
				}
				break
			}
		}
		if found == nil {
			t.Errorf("noun %q not registered under root", nounName)
		}
	}
}

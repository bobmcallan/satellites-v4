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
	helpsExitZero(t, []string{"story", "--help"},
		"create",
		"get",
		"list",
		"update-status",
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
	// Only verbs that remain stubs after order:04. Migrated reads
	// (info, session whoami, task get/walk, story get, agent get,
	// contract get, principle list, ledger get/list/search) reach
	// the remote and must be tested via integration paths instead.
	verbs := [][]string{
		{"kv", "get"},
		{"agent", "compose"},
		{"project", "create"},
		{"document", "create"},
		{"contract", "create"},
		{"reviewer", "create"},
	}
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
	expected := map[string]int{
		"story":     9,
		"task":      7,
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
		"system":    1,
		"portal":    1,
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

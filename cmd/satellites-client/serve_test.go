package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestServeNounRegistered asserts the cobra surface for `serve` ships
// the four expected sub-verbs (run / start / stop / status).
func TestServeNounRegistered(t *testing.T) {
	root := &cobra.Command{Use: "satellites-client"}
	registerServeNoun(root)
	serve, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatalf("serve subcommand not found: %v", err)
	}
	wantVerbs := []string{"run", "start", "stop", "status"}
	got := map[string]bool{}
	for _, c := range serve.Commands() {
		got[c.Name()] = true
	}
	for _, v := range wantVerbs {
		if !got[v] {
			t.Errorf("serve missing verb %q", v)
		}
	}
}

// TestServeHelp ensures `serve --help` output names every verb.
func TestServeHelp(t *testing.T) {
	root := &cobra.Command{Use: "satellites-client"}
	registerServeNoun(root)
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"serve", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"run", "start", "stop", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve --help missing verb %q (out=%s)", want, out)
		}
	}
}

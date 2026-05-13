package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

// stubMutateServer records the most recent request so tests can
// assert what the CLI POSTed to the HTTP API. Returns the supplied
// reply body verbatim (must be valid JSON) for any request.
func stubMutateServer(t *testing.T, replyBody string) (*httptest.Server, *captured) {
	t.Helper()
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cap.path = r.URL.Path
		cap.args = args
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(replyBody))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type captured struct {
	path string
	args map[string]any
}

func TestWrites_TaskAddHappy(t *testing.T) {
	resetCLIState(t)
	srv, cap := stubMutateServer(t, `{"task_id":"task_x","status":"published"}`)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json", "task", "add", "--agent-id", "doc_x", "--prompt", "do work"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	args := cap.args
	if args["agent_id"] != "doc_x" {
		t.Fatalf("agent_id not forwarded: %+v", args)
	}
	if args["prompt"] != "do work" {
		t.Fatalf("prompt not forwarded: %+v", args)
	}
}

func TestWrites_TaskAddRequiresPromptOrStdin(t *testing.T) {
	resetCLIState(t)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"task", "add", "--agent-id", "doc_x"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usage error")
	}
	var typed *cliexit.Error
	if !errors.As(err, &typed) || typed.Code != cliexit.Usage {
		t.Fatalf("expected cliexit.Usage, got %v", err)
	}
}

func TestWrites_DryRun(t *testing.T) {
	resetCLIState(t)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--dry-run", "task", "add", "--agent-id", "doc_x", "--prompt", "p"})
	// No httptest server — --dry-run must skip the network. Success
	// = clean exit (printed to stdout, not Cobra's writer).
	if err := root.Execute(); err != nil {
		t.Fatalf("--dry-run Execute: %v", err)
	}
}

func TestWrites_TaskUpdateHappy(t *testing.T) {
	resetCLIState(t)
	srv, cap := stubMutateServer(t, `{"task_id":"task_x","status":"closed","outcome":"success"}`)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json",
		"task", "update", "--id", "task_x", "--status", "closed", "--outcome", "success"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	args := cap.args
	if args["id"] != "task_x" || args["status"] != "closed" {
		t.Fatalf("forwarding wrong: %+v", args)
	}
}

func TestWrites_LedgerAppendHappy(t *testing.T) {
	resetCLIState(t)
	srv, cap := stubMutateServer(t, `{"id":"ldg_x"}`)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json",
		"ledger", "append", "--project-id", "proj_x", "--type", "evidence", "--content", "ok"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	args := cap.args
	if args["project_id"] != "proj_x" || args["type"] != "evidence" || args["content"] != "ok" {
		t.Fatalf("forwarding wrong: %+v", args)
	}
}

// TestWrites_StoryUpdateStatusHappy renamed to
// TestWrites_StoryUpdateStatusFolded after sty_4db0e025 D1 folded
// story_update_status into story_update. The CLI surface is now
// `story update --status <s>` — status rides the same verb.
func TestWrites_StoryUpdateStatusFolded(t *testing.T) {
	resetCLIState(t)
	srv, cap := stubMutateServer(t, `{"id":"sty_x","status":"done"}`)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json",
		"story", "update", "--id", "sty_x", "--status", "done"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	args := cap.args
	if args["status"] != "done" {
		t.Fatalf("status not forwarded: %+v", args)
	}
	if args["id"] != "sty_x" {
		t.Fatalf("id not forwarded: %+v", args)
	}
}

func TestWrites_ProjectSetHappy(t *testing.T) {
	resetCLIState(t)
	srv, cap := stubMutateServer(t, `{"project_id":"proj_x","status":"resolved"}`)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json",
		"project", "set", "--repo-url", "git@github.com:foo/bar.git"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	args := cap.args
	if !strings.Contains(args["repo_url"].(string), "foo/bar") {
		t.Fatalf("repo_url not forwarded: %+v", args)
	}
}

func TestWrites_NotFoundEnvelopeMapsTo3(t *testing.T) {
	resetCLIState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"task not found"}`))
	}))
	t.Cleanup(srv.Close)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake",
		"task", "update", "--id", "task_missing", "--status", "closed"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected NotFound")
	}
	if got := cliexit.Resolve(err); got != cliexit.NotFound {
		t.Fatalf("expected NotFound got %d", got)
	}
}

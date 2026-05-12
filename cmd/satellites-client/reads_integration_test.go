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
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// stubHTTPAPIServer wraps an httptest.Server that responds to
// POST /api/v1/<noun>/<verb> with the payload registered under
// the corresponding tool name. The map key matches the toolName
// passed by the cobra handlers (e.g. "task_get"); the stub maps
// that to the path via cliremote.ToolNameToPath.
func stubHTTPAPIServer(t *testing.T, payloads map[string]any) *httptest.Server {
	t.Helper()
	pathToPayload := map[string]any{}
	for toolName, payload := range payloads {
		pathToPayload["/api/v1"+cliremote.ToolNameToPath(toolName)] = payload
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := pathToPayload[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no payload registered for " + r.URL.Path})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// resetCLIState clears the package-level cliremote / token cache so
// each test boots a fresh CLI session pointed at the test server.
func resetCLIState(t *testing.T) {
	t.Helper()
	pf = PersistentFlags{}
	resolvedToken = ""
	resolvedMode = 0
	remote = nil
}

func TestReads_InfoHappyPath(t *testing.T) {
	resetCLIState(t)
	srv := stubHTTPAPIServer(t, map[string]any{
		"satellites_info": map[string]any{"version": "0.0.0", "user_id": "u_x"},
	})
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json", "info"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestReads_TaskGetHappyPath(t *testing.T) {
	resetCLIState(t)
	srv := stubHTTPAPIServer(t, map[string]any{
		"task_get": map[string]any{"id": "task_x", "kind": "work"},
	})
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json", "task", "get", "task_x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestReads_TaskGetMissingArg(t *testing.T) {
	resetCLIState(t)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"task", "get"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing positional")
	}
	// Cobra emits a plain error for missing positional; cliexit.Resolve
	// maps that to Server. The story_get verb explicitly requires the
	// arg via cobra.ExactArgs.
}

func TestReads_LedgerListMissingProjectID(t *testing.T) {
	resetCLIState(t)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"ledger", "list"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing --project-id")
	}
}

func TestReads_StoryGetCompactProjection(t *testing.T) {
	resetCLIState(t)
	srv := stubHTTPAPIServer(t, map[string]any{
		"story_get": map[string]any{
			"story": map[string]any{
				"id":       "sty_x",
				"title":    "T",
				"status":   "in_progress",
				"priority": "high",
				"tags":     []string{"epic:cli-primary"},
				"ignored":  "noise",
			},
			"project":         map[string]any{"id": "proj_x"},
			"recent_evidence": []any{},
		},
	})
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "--json", "--compact", "story", "get", "sty_x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// stdout (the cli writes to os.Stdout, not Cobra's writer): we
	// can't intercept that here without dup'ing fds. The smoke
	// asserts that the run is clean; deeper output assertions live
	// in the integration suite.
}

func TestReads_NotFoundEnvelopeMapsTo3(t *testing.T) {
	resetCLIState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"story not found"}`))
	}))
	t.Cleanup(srv.Close)
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", srv.URL, "--token", "fake", "story", "get", "sty_missing"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	var typed *cliexit.Error
	if !errors.As(err, &typed) || typed.Code != cliexit.NotFound {
		t.Fatalf("expected cliexit.NotFound, got %v", err)
	}
	if !strings.Contains(typed.Error(), "not found") {
		t.Fatalf("error message missing 'not found': %q", typed.Error())
	}
}

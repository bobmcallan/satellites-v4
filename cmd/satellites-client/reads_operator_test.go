package main

// reads_operator_test.go — happy-path + arg-validation integration
// coverage for the operator-tier read verbs landed in sty_ef248ab2.
// Asserts each CLI verb hits the expected /api/v1/<noun>/<verb>
// route on the stub server and rejects missing required flags.

import (
	"bytes"
	"testing"
)

// readsOperatorHappy lists the (cli args, tool name) tuples to drive
// against the httptest stub. Each tuple's tool name must match the
// path the cliremote.ToolNameToPath mapper produces; the stub returns
// a generic OK payload.
var readsOperatorHappy = []struct {
	name  string
	args  []string
	tool  string
	reply map[string]any
}{
	{"story list", []string{"story", "list", "--project-id", "proj_x"}, "story_list", map[string]any{"items": []any{}}},
	{"story template-get", []string{"story", "template-get", "--category", "feature"}, "story_template_get", map[string]any{"category": "feature"}},
	{"story template-list", []string{"story", "template-list"}, "story_template_list", map[string]any{"items": []any{}}},
	{"story export-walk", []string{"story", "export-walk", "sty_x"}, "story_export_walk", map[string]any{"filename": "sty_x-walk.md"}},
	{"task list", []string{"task", "list"}, "task_list", map[string]any{"items": []any{}}},
	{"project get", []string{"project", "get", "proj_x"}, "project_get", map[string]any{"project": map[string]any{"id": "proj_x"}}},
	{"project list", []string{"project", "list"}, "project_list", map[string]any{"items": []any{}}},
	{"workspace get", []string{"workspace", "get", "wksp_x"}, "workspace_get", map[string]any{"id": "wksp_x"}},
	{"workspace list", []string{"workspace", "list"}, "workspace_list", map[string]any{"items": []any{}}},
	{"workspace member-list", []string{"workspace", "member-list", "--workspace-id", "wksp_x"}, "workspace_member_list", map[string]any{"members": []any{}}},
	{"changelog get", []string{"changelog", "get", "chg_x"}, "changelog_get", map[string]any{"id": "chg_x"}},
	{"changelog list", []string{"changelog", "list", "--project-id", "proj_x"}, "changelog_list", map[string]any{"items": []any{}}},
	{"document search", []string{"document", "search", "--query", "auth"}, "document_search", map[string]any{"items": []any{}}},
	{"agent list", []string{"agent", "list"}, "agent_list", map[string]any{"items": []any{}}},
	{"agent search", []string{"agent", "search", "--query", "claude"}, "agent_search", map[string]any{"items": []any{}}},
	{"agent ephemeral-summary", []string{"agent", "ephemeral-summary"}, "agent_ephemeral_summary", map[string]any{"counts": map[string]any{}}},
	{"contract list", []string{"contract", "list"}, "contract_list", map[string]any{"items": []any{}}},
	{"contract search", []string{"contract", "search", "--query", "develop"}, "contract_search", map[string]any{"items": []any{}}},
	{"principle get name", []string{"principle", "get", "--name", "evidence_audit"}, "principle_get", map[string]any{"id": "doc_x"}},
	{"principle search", []string{"principle", "search", "--query", "evidence"}, "principle_search", map[string]any{"items": []any{}}},
	{"kv get", []string{"kv", "get", "--scope", "project", "--key", "k", "--project-id", "proj_x"}, "kv_get", map[string]any{"scope": "project", "key": "k", "value": "v"}},
	{"kv list", []string{"kv", "list", "--scope", "project", "--project-id", "proj_x"}, "kv_list", map[string]any{"scope": "project", "items": []any{}}},
	{"kv get-resolved", []string{"kv", "get-resolved", "--key", "k"}, "kv_get_resolved", map[string]any{"key": "k", "value": "v"}},
	{"repo get", []string{"repo", "get", "repo_x"}, "repo_get", map[string]any{"id": "repo_x"}},
	{"repo list", []string{"repo", "list", "--project-id", "proj_x"}, "repo_list", map[string]any{"repos": []any{}}},
	{"repo search", []string{"repo", "search", "--repo-id", "repo_x", "--query", "f"}, "repo_search", map[string]any{"items": []any{}}},
	{"repo search-text", []string{"repo", "search-text", "--repo-id", "repo_x", "--query", "f"}, "repo_search_text", map[string]any{"items": []any{}}},
	{"repo get-symbol-source", []string{"repo", "get-symbol-source", "--repo-id", "repo_x", "--symbol-id", "s"}, "repo_get_symbol_source", map[string]any{"source": ""}},
	{"repo get-file", []string{"repo", "get-file", "--repo-id", "repo_x", "--path", "main.go"}, "repo_get_file", map[string]any{"content": ""}},
	{"repo get-outline", []string{"repo", "get-outline", "--repo-id", "repo_x", "--path", "main.go"}, "repo_get_outline", map[string]any{"outline": ""}},
}

func TestReads_Operator_HappyPath(t *testing.T) {
	for _, tc := range readsOperatorHappy {
		t.Run(tc.name, func(t *testing.T) {
			resetCLIState(t)
			srv := stubHTTPAPIServer(t, map[string]any{tc.tool: tc.reply})
			root := newRootCmd()
			out := &bytes.Buffer{}
			root.SetOut(out)
			root.SetErr(out)
			full := append([]string{"--server", srv.URL, "--token", "fake", "--json"}, tc.args...)
			root.SetArgs(full)
			if err := root.Execute(); err != nil {
				t.Fatalf("%s: Execute: %v", tc.name, err)
			}
		})
	}
}

// readsOperatorMissingFlag exercises arg-validation: each CLI verb's
// required-flag check must fire before the remote call.
var readsOperatorMissingFlag = []struct {
	name string
	args []string
}{
	{"story list no project", []string{"story", "list"}},
	{"story template-get no category", []string{"story", "template-get"}},
	{"workspace member-list no workspace", []string{"workspace", "member-list"}},
	{"kv get no scope", []string{"kv", "get", "--key", "k"}},
	{"kv get no key", []string{"kv", "get", "--scope", "project"}},
	{"kv list no scope", []string{"kv", "list"}},
	{"kv get-resolved no key", []string{"kv", "get-resolved"}},
	{"repo search no repo-id", []string{"repo", "search", "--query", "f"}},
	{"repo search-text no query", []string{"repo", "search-text", "--repo-id", "repo_x"}},
	{"repo get-symbol-source no symbol", []string{"repo", "get-symbol-source", "--repo-id", "repo_x"}},
	{"repo get-file no path", []string{"repo", "get-file", "--repo-id", "repo_x"}},
	{"repo get-outline no path", []string{"repo", "get-outline", "--repo-id", "repo_x"}},
	{"principle get neither id nor name", []string{"principle", "get"}},
}

func TestReads_Operator_MissingRequiredFlag(t *testing.T) {
	for _, tc := range readsOperatorMissingFlag {
		t.Run(tc.name, func(t *testing.T) {
			resetCLIState(t)
			root := newRootCmd()
			out := &bytes.Buffer{}
			root.SetOut(out)
			root.SetErr(out)
			root.SetArgs(tc.args)
			if err := root.Execute(); err == nil {
				t.Fatalf("%s: expected error, got nil", tc.name)
			}
		})
	}
}

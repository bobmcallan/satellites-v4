package main

// writes_operator_test.go — happy-path + arg-validation coverage for
// the Tier A mutating verbs landed in sty_f38bd573. Each tuple drives
// the CLI verb through the httptest stub and asserts the expected
// /api/v1/<noun>/<verb> route is hit.

import (
	"bytes"
	"testing"
)

var writesOperatorHappy = []struct {
	name  string
	args  []string
	tool  string
	reply map[string]any
}{
	{"story create", []string{"story", "create", "--project-id", "proj_x", "--title", "T"}, "story_create", map[string]any{"id": "sty_x"}},
	{"story update", []string{"story", "update", "--id", "sty_x", "--title", "T2"}, "story_update", map[string]any{"id": "sty_x"}},
	{"story delete", []string{"story", "delete", "sty_x"}, "story_delete", map[string]any{"id": "sty_x", "deleted": true}},
	{"project create", []string{"project", "create", "--name", "proj"}, "project_create", map[string]any{"id": "proj_x"}},
	{"project update", []string{"project", "update", "--id", "proj_x", "--name", "new"}, "project_update", map[string]any{"id": "proj_x"}},
	{"project delete", []string{"project", "delete", "proj_x"}, "project_delete", map[string]any{"id": "proj_x"}},
	{"workspace create", []string{"workspace", "create", "--name", "w"}, "workspace_create", map[string]any{"id": "wksp_x"}},
	{"workspace member-add", []string{"workspace", "member-add", "--workspace-id", "wksp_x", "--user-id", "u_a", "--role", "admin"}, "workspace_member_add", map[string]any{"role": "admin"}},
	{"workspace member-update-role", []string{"workspace", "member-update-role", "--workspace-id", "wksp_x", "--user-id", "u_a", "--role", "member"}, "workspace_member_update_role", map[string]any{"role": "member"}},
	{"workspace member-remove", []string{"workspace", "member-remove", "--workspace-id", "wksp_x", "--user-id", "u_a"}, "workspace_member_remove", map[string]any{"removed": true}},
	{"kv set", []string{"kv", "set", "--scope", "project", "--key", "k", "--value", "v", "--project-id", "proj_x"}, "kv_set", map[string]any{"entry_id": "ldg_x"}},
	{"kv delete", []string{"kv", "delete", "--scope", "project", "--key", "k", "--project-id", "proj_x"}, "kv_delete", map[string]any{"deleted": true}},
	{"changelog add", []string{"changelog", "add", "--service", "client", "--content", "release"}, "changelog_add", map[string]any{"id": "chg_x"}},
	{"changelog update", []string{"changelog", "update", "--id", "chg_x", "--content", "new"}, "changelog_update", map[string]any{"id": "chg_x"}},
	{"changelog delete", []string{"changelog", "delete", "chg_x"}, "changelog_delete", map[string]any{"id": "chg_x", "deleted": true}},
	{"repo add", []string{"repo", "add", "--git-remote", "git@github.com:o/r.git"}, "repo_add", map[string]any{"repo_id": "repo_x"}},
	{"ledger dereference", []string{"ledger", "dereference", "--id", "ldg_x", "--reason", "wrong project"}, "ledger_dereference", map[string]any{"id": "ldg_x"}},
	{"session register", []string{"session", "register"}, "session_register", map[string]any{"session_id": "sess_x"}},
	{"document add", []string{"document", "add", "--scope", "system", "--name", "x", "--type", "agent"}, "document_add", map[string]any{"id": "doc_x"}},
	{"document update", []string{"document", "update", "--id", "doc_x", "--body", "b"}, "document_update", map[string]any{"id": "doc_x"}},
	{"document delete", []string{"document", "delete", "--id", "doc_x"}, "document_delete", map[string]any{"id": "doc_x", "deleted": true}},
	{"agent add", []string{"agent", "add", "--scope", "system", "--name", "a"}, "agent_add", map[string]any{"id": "doc_a"}},
	{"agent update", []string{"agent", "update", "--id", "doc_a", "--body", "b"}, "agent_update", map[string]any{"id": "doc_a"}},
	{"agent delete", []string{"agent", "delete", "--id", "doc_a"}, "agent_delete", map[string]any{"id": "doc_a", "deleted": true}},
	{"contract add", []string{"contract", "add", "--scope", "system", "--name", "c"}, "contract_add", map[string]any{"id": "doc_c"}},
	{"contract update", []string{"contract", "update", "--id", "doc_c", "--body", "b"}, "contract_update", map[string]any{"id": "doc_c"}},
	{"contract delete", []string{"contract", "delete", "--id", "doc_c"}, "contract_delete", map[string]any{"id": "doc_c", "deleted": true}},
	{"principle add", []string{"principle", "add", "--scope", "system", "--name", "p"}, "principle_add", map[string]any{"id": "doc_p"}},
	{"principle update", []string{"principle", "update", "--id", "doc_p", "--body", "b"}, "principle_update", map[string]any{"id": "doc_p"}},
	{"principle delete", []string{"principle", "delete", "--id", "doc_p"}, "principle_delete", map[string]any{"id": "doc_p", "deleted": true}},
	{"reviewer add", []string{"reviewer", "add", "--scope", "system", "--name", "r"}, "reviewer_add", map[string]any{"id": "doc_r"}},
	{"reviewer update", []string{"reviewer", "update", "--id", "doc_r", "--body", "b"}, "reviewer_update", map[string]any{"id": "doc_r"}},
	{"reviewer delete", []string{"reviewer", "delete", "--id", "doc_r"}, "reviewer_delete", map[string]any{"id": "doc_r", "deleted": true}},
	{"role add", []string{"role", "add", "--scope", "system", "--name", "ro"}, "role_add", map[string]any{"id": "doc_ro"}},
	{"role update", []string{"role", "update", "--id", "doc_ro", "--body", "b"}, "role_update", map[string]any{"id": "doc_ro"}},
	{"role delete", []string{"role", "delete", "--id", "doc_ro"}, "role_delete", map[string]any{"id": "doc_ro", "deleted": true}},
	{"skill add", []string{"skill", "add", "--scope", "system", "--name", "s"}, "skill_add", map[string]any{"id": "doc_s"}},
	{"skill update", []string{"skill", "update", "--id", "doc_s", "--body", "b"}, "skill_update", map[string]any{"id": "doc_s"}},
	{"skill delete", []string{"skill", "delete", "--id", "doc_s"}, "skill_delete", map[string]any{"id": "doc_s", "deleted": true}},
}

func TestWrites_Operator_HappyPath(t *testing.T) {
	for _, tc := range writesOperatorHappy {
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

var writesOperatorMissingFlag = []struct {
	name string
	args []string
}{
	{"story create no project", []string{"story", "create", "--title", "T"}},
	{"story create no title", []string{"story", "create", "--project-id", "p"}},
	{"story update no id", []string{"story", "update", "--title", "T"}},
	{"project create no name", []string{"project", "create"}},
	{"workspace create no name", []string{"workspace", "create"}},
	{"workspace member-add missing", []string{"workspace", "member-add", "--workspace-id", "w"}},
	{"kv set no scope", []string{"kv", "set", "--key", "k", "--value", "v"}},
	{"kv set no key", []string{"kv", "set", "--scope", "user"}},
	{"kv delete no scope", []string{"kv", "delete", "--key", "k"}},
	{"changelog add no service", []string{"changelog", "add", "--content", "c"}},
	{"changelog update no id", []string{"changelog", "update", "--content", "c"}},
	{"repo add no remote", []string{"repo", "add"}},
	{"ledger dereference no id", []string{"ledger", "dereference", "--reason", "r"}},
	{"ledger dereference no reason", []string{"ledger", "dereference", "--id", "ldg_x"}},
	{"document create no scope", []string{"document", "create", "--name", "n", "--type", "agent"}},
	{"document create no name", []string{"document", "create", "--scope", "system", "--type", "agent"}},
	{"agent create no scope", []string{"agent", "create", "--name", "a"}},
	{"agent update no id", []string{"agent", "update", "--body", "b"}},
}

func TestWrites_Operator_MissingRequiredFlag(t *testing.T) {
	for _, tc := range writesOperatorMissingFlag {
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

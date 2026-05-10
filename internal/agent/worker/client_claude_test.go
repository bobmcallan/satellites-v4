package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
)

// Tests live in the worker package (no _test suffix on the package
// declaration) because they exercise unexported helpers
// (composePrompt, ensureWorktree, cleanseHome, etc.) that the AC
// requires unit coverage on.

func TestComposePrompt_ThinPointerShape(t *testing.T) {
	cases := []struct {
		name string
		in   promptInputs
		want []string
	}{
		{
			name: "develop_action",
			in: promptInputs{
				Task: taskInfo{
					ID: "task_001", StoryID: "sty_aaa", ProjectID: "proj_x",
					WorkspaceID: "wksp_y", Action: "contract:develop",
				},
				Agent:    agentInfo{Name: "developer_agent"},
				Contract: contractInfo{Name: "develop"},
				Story:    storyInfo{ID: "sty_aaa"},
				HomePath: "/tmp/home_xxx",
				WorkDir:  "/repo/.satellites-agents/task_001",
			},
			want: []string{
				`satellites-client task get task_001`,
				`satellites-client agent get --name developer_agent`,
				`satellites-client contract get --name develop --project-id proj_x`,
				`satellites-client story get sty_aaa`,
				`satellites-client task walk --story-id sty_aaa`,
				`satellites-client principle list --active-only --project-id proj_x`,
				`satellites-client task update --id task_001 --status closed --outcome success`,
				"/tmp/home_xxx",
				"/repo/.satellites-agents/task_001",
				"Action:    contract:develop",
				"Role:      developer_agent",
			},
		},
		{
			name: "plan_action",
			in: promptInputs{
				Task: taskInfo{
					ID: "task_p", StoryID: "sty_p", ProjectID: "proj_p",
					WorkspaceID: "wksp_p", Action: "contract:plan",
				},
				Agent:    agentInfo{Name: "planner_agent"},
				Contract: contractInfo{Name: "plan"},
				Story:    storyInfo{ID: "sty_p"},
				HomePath: "/tmp/home_p",
				WorkDir:  "/repo/.satellites-agents/task_p",
			},
			want: []string{
				`satellites-client task get task_p`,
				`satellites-client agent get --name planner_agent`,
				`satellites-client contract get --name plan --project-id proj_p`,
				`satellites-client story get sty_p`,
				"Action:    contract:plan",
			},
		},
		{
			name: "free_form_action",
			in: promptInputs{
				Task: taskInfo{
					ID: "task_f", StoryID: "sty_f", ProjectID: "proj_f",
					WorkspaceID: "wksp_f", Action: "ad-hoc-task",
				},
				Agent:    agentInfo{Name: "adhoc_agent"},
				Contract: contractInfo{Name: "ad-hoc-task"},
				Story:    storyInfo{ID: "sty_f"},
				HomePath: "/tmp/home_f",
				WorkDir:  "/repo/.satellites-agents/task_f",
			},
			want: []string{
				"Action:    ad-hoc-task",
				`satellites-client contract get --name ad-hoc-task --project-id proj_f`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composePrompt(tc.in)
			for _, want := range tc.want {
				assert.Contains(t, got, want, "missing %q in prompt:\n%s", want, got)
			}
			assert.True(t, strings.HasPrefix(got, "You are dispatched on satellites task "), "prompt should lead with task pointer; got:\n%s", got)
			assert.Contains(t, got, "Begin.", "prompt should end with Begin.")
		})
	}
}

func TestRenderBranchName(t *testing.T) {
	cases := []struct {
		template, taskID, baseSHA, want string
	}{
		{"agent-{task_id}-from-{base_sha}", "task_x", "abcd123", "agent-task_x-from-abcd123"},
		{"sat/{task_id}", "task_y", "f0", "sat/task_y"},
		{"static-branch", "task_z", "deadbeef", "static-branch"},
		{"{base_sha}", "task_q", "1234", "1234"},
	}
	for _, tc := range cases {
		got := renderBranchName(tc.template, tc.taskID, tc.baseSHA)
		assert.Equal(t, tc.want, got)
	}
}

func TestResolveWorktreePath(t *testing.T) {
	got := resolveWorktreePath("/repo", "/abs/wt/", "task_a")
	assert.Equal(t, "/abs/wt/task_a", got)

	got = resolveWorktreePath("/repo", ".satellites-agents/", "task_b")
	assert.Equal(t, "/repo/.satellites-agents/task_b", got)
}

// TestEnsureWorktree_CreatePath drives the "no worktree exists" branch
// and asserts the recorded git commands match the seven-step shape.
func TestEnsureWorktree_CreatePath(t *testing.T) {
	tmp := t.TempDir()
	worktreeRoot := filepath.Join(tmp, "wt")
	repo := filepath.Join(tmp, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	var cmds [][]string
	c := &claudeClient{
		placeholderClient: &placeholderClient{
			cfg: config.AgentConfig{
				RepoPath:       repo,
				WorktreeRoot:   worktreeRoot,
				BranchTemplate: "agent-{task_id}-from-{base_sha}",
			},
		},
		gitRunner: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			cmds = append(cmds, append([]string{"DIR=" + dir}, args...))
			if len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--short" {
				return []byte("a225291\n"), nil
			}
			return nil, nil
		},
	}

	path, branch, err := c.ensureWorktree(context.Background(), TaskEnvelope{ID: "task_xyz"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(worktreeRoot, "task_xyz"), path)
	assert.Equal(t, "agent-task_xyz-from-a225291", branch)

	require.Len(t, cmds, 2)
	assert.Equal(t, []string{"DIR=" + repo, "rev-parse", "--short", "HEAD"}, cmds[0])
	assert.Equal(t, []string{"DIR=" + repo, "worktree", "add", "-b", "agent-task_xyz-from-a225291", filepath.Join(worktreeRoot, "task_xyz"), "a225291"}, cmds[1])
}

// TestEnsureWorktree_ReusePath drives the "worktree already exists"
// branch — the gitRunner must not be called.
func TestEnsureWorktree_ReusePath(t *testing.T) {
	tmp := t.TempDir()
	worktreeRoot := filepath.Join(tmp, "wt")
	repo := filepath.Join(tmp, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	// Pre-populate the worktree path with a .git marker file.
	wtPath := filepath.Join(worktreeRoot, "task_reuse")
	require.NoError(t, os.MkdirAll(wtPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, ".git"), []byte("gitdir: /elsewhere\n"), 0o600))

	gitCalled := false
	c := &claudeClient{
		placeholderClient: &placeholderClient{
			cfg: config.AgentConfig{
				RepoPath:       repo,
				WorktreeRoot:   worktreeRoot,
				BranchTemplate: "x-{task_id}-{base_sha}",
			},
		},
		gitRunner: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			gitCalled = true
			return nil, nil
		},
	}

	path, _, err := c.ensureWorktree(context.Background(), TaskEnvelope{ID: "task_reuse"})
	require.NoError(t, err)
	assert.Equal(t, wtPath, path)
	assert.False(t, gitCalled, "ensureWorktree must not invoke git when the worktree already exists")
}

func TestEnsureWorktree_RepoPathEmpty_Errors(t *testing.T) {
	c := &claudeClient{
		placeholderClient: &placeholderClient{cfg: config.AgentConfig{}},
		gitRunner:         func(ctx context.Context, dir string, args ...string) ([]byte, error) { return nil, nil },
	}
	_, _, err := c.ensureWorktree(context.Background(), TaskEnvelope{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo_path empty")
}

// TestCleanseHome_NoProjectsDir is the AC's HOME-cleansing assertion:
// the cleansed HOME copies .credentials.json + settings.json but never
// the projects/ directory.
func TestCleanseHome_NoProjectsDir(t *testing.T) {
	srcHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".claude", "projects", "fake-project"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", "projects", "fake-project", "MEMORY.md"), []byte("operator memory should not leak"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", ".credentials.json"), []byte(`{"oauth": "abc"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", "settings.json"), []byte(`{"theme":"dark"}`), 0o600))

	c := &claudeClient{
		placeholderClient: &placeholderClient{cfg: config.AgentConfig{}},
		findHome:          func() (string, error) { return srcHome, nil },
	}

	tmpHome, err := c.cleanseHome()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpHome) })

	// Auth artefacts must be present.
	for _, name := range []string{".credentials.json", "settings.json"} {
		st, err := os.Stat(filepath.Join(tmpHome, ".claude", name))
		require.NoError(t, err, "missing %s", name)
		assert.False(t, st.IsDir())
	}
	// projects/ must NOT be present — it would carry operator memory.
	_, err = os.Stat(filepath.Join(tmpHome, ".claude", "projects"))
	assert.True(t, os.IsNotExist(err), "cleansed HOME contains projects/ directory: %v", err)

	// MEMORY.md content sanity — recursive grep must produce no match
	// for the operator-memory marker.
	var found bool
	_ = filepath.Walk(tmpHome, func(p string, info os.FileInfo, e error) error {
		if e != nil || info == nil || info.IsDir() {
			return nil
		}
		body, _ := os.ReadFile(p)
		if strings.Contains(string(body), "operator memory should not leak") {
			found = true
		}
		return nil
	})
	assert.False(t, found, "cleansed HOME leaked operator memory text")
}

// TestCleanseHome_MissingSettingsTolerated covers the case where the
// operator has no settings.json — the cleanse must still succeed and
// the credentials file must still copy.
func TestCleanseHome_MissingSettingsTolerated(t *testing.T) {
	srcHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcHome, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcHome, ".claude", ".credentials.json"), []byte(`{}`), 0o600))

	c := &claudeClient{
		placeholderClient: &placeholderClient{cfg: config.AgentConfig{}},
		findHome:          func() (string, error) { return srcHome, nil },
	}

	tmpHome, err := c.cleanseHome()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpHome) })

	_, err = os.Stat(filepath.Join(tmpHome, ".claude", ".credentials.json"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmpHome, ".claude", "settings.json"))
	assert.True(t, os.IsNotExist(err), "settings.json should remain absent")
}

// TestBuildMCPConfigJSON checks the shape passed to claude
// --mcp-config: a single satellites server, http transport, with the
// agent's auth token threaded as a Bearer header.
func TestBuildMCPConfigJSON(t *testing.T) {
	cfg := config.AgentConfig{
		MCPURL:    "https://satellites-pprod.fly.dev/mcp?project_id=proj_x",
		AuthToken: "sat_test123",
	}
	raw, err := buildMCPConfigJSON(cfg)
	require.NoError(t, err)

	var got struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &got))

	require.Contains(t, got.MCPServers, "satellites")
	srv := got.MCPServers["satellites"]
	assert.Equal(t, "http", srv.Type)
	assert.Equal(t, cfg.MCPURL, srv.URL)
	assert.Equal(t, "Bearer sat_test123", srv.Headers["Authorization"])
	// strict-mcp-config must see ONLY the satellites server.
	assert.Len(t, got.MCPServers, 1)
}

func TestBuildMCPConfigJSON_NoToken_OmitsAuthHeader(t *testing.T) {
	cfg := config.AgentConfig{MCPURL: "http://localhost:8080/mcp"}
	raw, err := buildMCPConfigJSON(cfg)
	require.NoError(t, err)
	assert.NotContains(t, raw, "Authorization")
	assert.NotContains(t, raw, "Bearer")
}

// TestEnforceHomeEnv guarantees HOME=tmpHome wins even when the
// inherited environment already declared HOME — the AC's HOME-
// cleansing requirement must be observable to claude regardless of
// the parent's env shape.
func TestEnforceHomeEnv(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HOME=/operator", "USER=op"}
	out := enforceHomeEnv(in, "/tmp/cleansed")
	// Final entry must be the cleansed HOME and there must be exactly
	// one HOME= entry.
	homeCount := 0
	for _, e := range out {
		if strings.HasPrefix(e, "HOME=") {
			homeCount++
		}
	}
	assert.Equal(t, 1, homeCount, "must be exactly one HOME= entry; got %v", out)
	assert.Equal(t, "HOME=/tmp/cleansed", out[len(out)-1])
}

// TestFindSessionJSONL_Empty handles the case where no session JSONL
// file has been written yet — the helper returns "" rather than
// erroring.
func TestFindSessionJSONL_Empty(t *testing.T) {
	tmp := t.TempDir()
	got := findSessionJSONL(tmp)
	assert.Equal(t, "", got)
}

func TestFindSessionJSONL_PicksMostRecent(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, ".claude", "projects", "fake")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	older := filepath.Join(dir, "old.jsonl")
	newer := filepath.Join(dir, "new.jsonl")
	require.NoError(t, os.WriteFile(older, []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(newer, []byte("{}"), 0o600))
	// Force ordering by modtime.
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	new_ := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(older, old, old))
	require.NoError(t, os.Chtimes(newer, new_, new_))

	got := findSessionJSONL(tmp)
	assert.Equal(t, newer, got)
}

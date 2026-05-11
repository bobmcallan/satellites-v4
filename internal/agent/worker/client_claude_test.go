package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
)

// Tests live in the worker package (no _test suffix on the package
// declaration) because they exercise unexported helpers
// (composePrompt, ensureWorktree, etc.) that the AC requires unit
// coverage on.

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
				WorkDir: "/repo/.satellites-agents/task_001",
			},
			want: []string{
				`satellites-client task get task_001`,
				`satellites-client agent get --name developer_agent`,
				`satellites-client contract get --name develop --project-id proj_x`,
				`satellites-client story get sty_aaa`,
				`satellites-client task walk --story-id sty_aaa`,
				`satellites-client principle list --active-only --project-id proj_x`,
				`satellites-client task update --id task_001 --status closed --outcome success`,
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
				WorkDir: "/repo/.satellites-agents/task_p",
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
				WorkDir: "/repo/.satellites-agents/task_f",
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

// TestContractInfo_DispatchClass (sty_3b3e4e66 Layer A) — the
// contractInfo.dispatchClass() helper decodes the dispatch_class
// frontmatter field from the contract document's Structured payload.
// The worker dispatcher reads this to choose between the heavy claude
// subprocess and the in-process hot-path runner (Layer B).
//
// Returns "" when Structured is empty (the configseed loader prunes
// missing dispatch_class), absent, or malformed — the worker MUST
// treat "" as the heavy default so unmarked contracts continue to
// dispatch the existing claude subprocess.
func TestContractInfo_DispatchClass(t *testing.T) {
	cases := []struct {
		name       string
		structured []byte
		want       string
	}{
		{
			name:       "hot",
			structured: []byte(`{"category":"push","dispatch_class":"hot"}`),
			want:       "hot",
		},
		{
			name:       "heavy_explicit",
			structured: []byte(`{"category":"develop","dispatch_class":"heavy"}`),
			want:       "heavy",
		},
		{
			name:       "absent_falls_back_to_empty",
			structured: []byte(`{"category":"develop","validation_mode":"llm"}`),
			want:       "",
		},
		{
			name:       "nil_structured",
			structured: nil,
			want:       "",
		},
		{
			name:       "malformed_payload",
			structured: []byte(`{not valid json`),
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci := contractInfo{Name: "test", Structured: tc.structured}
			assert.Equal(t, tc.want, ci.dispatchClass())
		})
	}
}

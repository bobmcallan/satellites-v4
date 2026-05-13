package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/cliremote"
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
		cfg: config.AgentConfig{
			RepoPath:       repo,
			WorktreeRoot:   worktreeRoot,
			BranchTemplate: "agent-{task_id}-from-{base_sha}",
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
		cfg: config.AgentConfig{
			RepoPath:       repo,
			WorktreeRoot:   worktreeRoot,
			BranchTemplate: "x-{task_id}-{base_sha}",
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
		cfg:       config.AgentConfig{},
		gitRunner: func(ctx context.Context, dir string, args ...string) ([]byte, error) { return nil, nil },
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
		SpawnMCPURL: "https://satellites-pprod.fly.dev/mcp?project_id=proj_x",
		AuthToken:   "sat_test123",
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
	assert.Equal(t, cfg.SpawnMCPURL, srv.URL)
	assert.Equal(t, "Bearer sat_test123", srv.Headers["Authorization"])
	// strict-mcp-config must see ONLY the satellites server.
	assert.Len(t, got.MCPServers, 1)
}

func TestBuildMCPConfigJSON_NoToken_OmitsAuthHeader(t *testing.T) {
	cfg := config.AgentConfig{SpawnMCPURL: "http://localhost:8080/mcp"}
	raw, err := buildMCPConfigJSON(cfg)
	require.NoError(t, err)
	assert.NotContains(t, raw, "Authorization")
	assert.NotContains(t, raw, "Bearer")
}

// TestFetchHelpers_RouteThroughAPIv1 drives each fetch helper plus
// appendExecuteEvidence against a per-test httptest server, asserting
// the request path + decoded request body shape. The cliremote client
// posts to /api/v1/<noun>/<verb>; this test pins each helper's
// noun/verb mapping + payload contract.
func TestFetchHelpers_RouteThroughAPIv1(t *testing.T) {
	type capture struct {
		path string
		body map[string]any
	}

	cases := []struct {
		name     string
		wantPath string
		wantBody map[string]any
		resp     string
		run      func(t *testing.T, cc *claudeClient) any
		assert   func(t *testing.T, got any)
	}{
		{
			name:     "fetchTaskInfo",
			wantPath: "/api/v1/task/get",
			wantBody: map[string]any{"id": "task_x"},
			resp:     `{"id":"task_x","story_id":"sty_a","project_id":"proj_p","workspace_id":"wksp_w","agent_id":"doc_dev","action":"contract:develop"}`,
			run: func(t *testing.T, cc *claudeClient) any {
				ti, err := cc.fetchTaskInfo(context.Background(), "task_x")
				require.NoError(t, err)
				return ti
			},
			assert: func(t *testing.T, got any) {
				ti := got.(taskInfo)
				assert.Equal(t, "task_x", ti.ID)
				assert.Equal(t, "sty_a", ti.StoryID)
				assert.Equal(t, "contract:develop", ti.Action)
			},
		},
		{
			name:     "fetchAgentInfo_uses_document_get",
			wantPath: "/api/v1/document/get",
			wantBody: map[string]any{"id": "doc_dev", "type": "agent"},
			resp:     `{"id":"doc_dev","name":"developer_agent"}`,
			run: func(t *testing.T, cc *claudeClient) any {
				ai, err := cc.fetchAgentInfo(context.Background(), "doc_dev")
				require.NoError(t, err)
				return ai
			},
			assert: func(t *testing.T, got any) {
				ai := got.(agentInfo)
				assert.Equal(t, "doc_dev", ai.ID)
				assert.Equal(t, "developer_agent", ai.Name)
			},
		},
		{
			name:     "fetchContractInfo_uses_document_get",
			wantPath: "/api/v1/document/get",
			wantBody: map[string]any{"name": "develop", "type": "contract", "project_id": "proj_p"},
			resp:     `{"name":"develop","structured":"eyJjYXRlZ29yeSI6ImRldmVsb3AifQ=="}`,
			run: func(t *testing.T, cc *claudeClient) any {
				ci, err := cc.fetchContractInfo(context.Background(), "contract:develop", "proj_p")
				require.NoError(t, err)
				return ci
			},
			assert: func(t *testing.T, got any) {
				ci := got.(contractInfo)
				assert.Equal(t, "develop", ci.Name)
				assert.NotEmpty(t, ci.Structured)
			},
		},
		{
			name:     "fetchStoryInfo",
			wantPath: "/api/v1/story/get",
			wantBody: map[string]any{"id": "sty_a"},
			resp:     `{"story":{"id":"sty_a","title":"Example"}}`,
			run: func(t *testing.T, cc *claudeClient) any {
				si, err := cc.fetchStoryInfo(context.Background(), "sty_a")
				require.NoError(t, err)
				return si
			},
			assert: func(t *testing.T, got any) {
				si := got.(storyInfo)
				assert.Equal(t, "sty_a", si.ID)
				assert.Equal(t, "Example", si.Title)
			},
		},
		{
			name:     "appendExecuteEvidence",
			wantPath: "/api/v1/ledger/append",
			// Compare just the keys with stable scalar values — content +
			// tags are asserted via the captured raw body in the test
			// body after the case loop.
			wantBody: nil,
			resp:     `{"id":"ldg_test"}`,
			run: func(t *testing.T, cc *claudeClient) any {
				return cc.appendExecuteEvidence(
					context.Background(),
					TaskEnvelope{ID: "task_x", ProjectID: "proj_p"},
					taskInfo{StoryID: "sty_a", AgentID: "doc_dev", Action: "contract:develop"},
					"prompt-body", 0, "/worktree/.log",
				)
			},
			assert: func(t *testing.T, got any) {
				if got == nil {
					return
				}
				assert.NoError(t, got.(error))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got capture
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got.path = r.URL.Path
				_ = json.Unmarshal(body, &got.body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.resp))
			}))
			defer srv.Close()

			cc := &claudeClient{
				cfg: config.AgentConfig{},
				api: cliremote.New(srv.URL, "tok", nil),
			}
			result := tc.run(t, cc)
			tc.assert(t, result)
			assert.Equal(t, tc.wantPath, got.path)
			if tc.wantBody != nil {
				for k, v := range tc.wantBody {
					assert.Equal(t, v, got.body[k], "request body field %q", k)
				}
			}
			if tc.name == "appendExecuteEvidence" {
				// Verify the evidence row carries the required tag set
				// + content marker the reviewer asserts on.
				assert.Equal(t, "evidence", got.body["type"])
				assert.Equal(t, "proj_p", got.body["project_id"])
				assert.Equal(t, "sty_a", got.body["story_id"])
				tags, _ := got.body["tags"].([]any)
				var sawKind, sawTaskID bool
				for _, tag := range tags {
					s, _ := tag.(string)
					if s == "kind:agent-execute-evidence" {
						sawKind = true
					}
					if s == "task_id:task_x" {
						sawTaskID = true
					}
				}
				assert.True(t, sawKind, "missing kind:agent-execute-evidence tag")
				assert.True(t, sawTaskID, "missing task_id:task_x tag")
				content, _ := got.body["content"].(string)
				assert.Contains(t, content, "agent-execute-evidence")
				assert.Contains(t, content, "exit_code=0")
			}
		})
	}
}

// TestFetchTaskInfo_EmptyResponseSurfacesAsError covers the "task_get
// returned an empty row" guard the prompt composer depends on.
func TestFetchTaskInfo_EmptyResponseSurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cc := &claudeClient{api: cliremote.New(srv.URL, "tok", nil)}
	_, err := cc.fetchTaskInfo(context.Background(), "task_missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty response")
}

// TestFetchContractInfo_ErrorSurfacesNameOnly mirrors the
// error-tolerant shape the prior callTool path had: a contract row
// that fails to resolve degrades to the stripped name so the heavy-
// path dispatcher still composes a valid prompt.
func TestFetchContractInfo_ErrorSurfacesNameOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	cc := &claudeClient{api: cliremote.New(srv.URL, "tok", nil)}
	ci, err := cc.fetchContractInfo(context.Background(), "contract:freeform", "proj_p")
	require.NoError(t, err)
	assert.Equal(t, "freeform", ci.Name)
	assert.Empty(t, ci.Structured)
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

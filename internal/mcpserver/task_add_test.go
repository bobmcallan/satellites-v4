package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// agentDocID resolves the document id for a system-scope agent by name —
// orchestratorFixture seeds agents anonymously, so tests look them up
// here when they need the id. Memberships nil because system-scope rows
// are globally readable.
func agentDocID(t *testing.T, srv *Server, name string) string {
	t.Helper()
	doc, err := srv.deps.Documents.GetByName(context.Background(), "", name, nil)
	require.NoError(t, err, "lookup agent %q", name)
	return doc.ID
}

func callAddHandler(t *testing.T, f *orchestratorFixture, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := f.server.handleTaskAdd(f.callerCtx(), req)
	require.NoError(t, err)
	return res
}

func decodeResult(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	require.NotEmpty(t, res.Content, "result content empty")
	text := res.Content[0].(mcpgo.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out
}

func TestTaskAdd_HappyPath_StoryAttached(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	res := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "Implement the new substrate verb.",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, res.IsError, "happy path errored: %v", res.Content)

	out := decodeResult(t, res)
	require.Equal(t, f.storyID, out["story_id"])
	require.Equal(t, false, out["story_minted"])
	require.Equal(t, task.StatusPublished, out["status"])
	_, hasReviewID := out["review_task_id"]
	require.False(t, hasReviewID, "task_add response must not advertise review_task_id")
	require.NotEmpty(t, out["task_id"])

	// Confirm the row landed.
	row, err := f.taskStore.GetByID(context.Background(), out["task_id"].(string), nil)
	require.NoError(t, err)
	require.Equal(t, devID, row.AgentID)
	require.Equal(t, "Implement the new substrate verb.", row.Description)
	require.Equal(t, task.KindWork, row.Kind)
}

func TestTaskAdd_AutoMintsStoryWhenOmitted(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	res := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "First line of an ad-hoc prompt.\nSecond line ignored for title.",
	})
	require.False(t, res.IsError)
	out := decodeResult(t, res)
	require.Equal(t, true, out["story_minted"])
	storyID := out["story_id"].(string)
	require.NotEmpty(t, storyID)

	st, err := f.server.deps.Stories.GetByID(context.Background(), storyID, []string{f.wsID})
	require.NoError(t, err)
	require.Equal(t, "First line of an ad-hoc prompt.", st.Title)
	require.Equal(t, "see task body", st.AcceptanceCriteria)
	require.Contains(t, st.Tags, "adhoc")
}

func TestTaskAdd_RejectsCapabilityMismatch(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	// developer_agent delivers plan + develop; push is releaser_agent's.
	devID := agentDocID(t, f.server, "developer_agent")

	res := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "push the branch",
		"story_id": f.storyID,
		"action":   task.ContractAction("push"),
	})
	require.True(t, res.IsError, "expected capability rejection")
	require.Contains(t, errorText(res), "agent_cannot_deliver")
}

func TestTaskAdd_RejectsUnknownAgent(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)

	res := callAddHandler(t, f, map[string]any{
		"agent_id": "doc_does_not_exist",
		"prompt":   "x",
		"story_id": f.storyID,
	})
	require.True(t, res.IsError)
	require.Contains(t, errorText(res), "agent_not_found")
}

func TestTaskAdd_RequiresPrompt(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	res := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "   ",
		"story_id": f.storyID,
	})
	require.True(t, res.IsError)
	require.Contains(t, errorText(res), "prompt must not be empty")
}

// TestTaskAdd_MintsExactlyOneTask locks AC #2 of sty_9f3562b8: task_add
// mints exactly one task per call regardless of agent doc shape, story
// origin, or structured payload. The substrate no longer auto-pairs a
// review sibling — pairing, when a contract requires it, is authored
// by the reviewer's contract prose.
func TestTaskAdd_MintsExactlyOneTask(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	// Variant A — plain agent + explicit story.
	resA := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "variant A: explicit story",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, resA.IsError, errorText(resA))
	outA := decodeResult(t, resA)

	// Variant B — story_id omitted (auto-mint path).
	resB := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "variant B: ad-hoc story",
		"action":   task.ContractAction("develop"),
	})
	require.False(t, resB.IsError, errorText(resB))
	outB := decodeResult(t, resB)
	storyB := outB["story_id"].(string)

	// Variant C — fresh agent with arbitrary structured payload.
	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers: []string{task.ContractAction("develop")},
	})
	doc, err := f.server.deps.Documents.Create(context.Background(), document.Document{
		Type:       document.TypeAgent,
		Scope:      document.ScopeSystem,
		Name:       "extra_agent",
		Body:       "agent body",
		Status:     document.StatusActive,
		Structured: settings,
	}, f.now)
	require.NoError(t, err)
	resC := callAddHandler(t, f, map[string]any{
		"agent_id": doc.ID,
		"prompt":   "variant C: arbitrary settings",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, resC.IsError, errorText(resC))

	// No variant should advertise a review_task_id, and no review row
	// should appear on any of the involved stories.
	for _, out := range []map[string]any{outA, outB, decodeResult(t, resC)} {
		_, hasReviewID := out["review_task_id"]
		require.False(t, hasReviewID, "task_add response advertised review_task_id: %v", out)
	}

	for _, sid := range []string{f.storyID, storyB} {
		reviews, err := f.taskStore.List(context.Background(), task.ListOptions{
			StoryID: sid,
			Kind:    task.KindReview,
		}, nil)
		require.NoError(t, err)
		require.Empty(t, reviews, "story %s has unexpected review rows: %+v", sid, reviews)
	}
}

// TestTaskAdd_SeedAgentResolvesForOtherWorkspaceCaller covers
// sty_6ee30308's task_add agent-resolver AC: a caller whose only
// workspace does not contain the seed-tier system rows must still be
// able to resolve a scope=system agent doc by id and publish a task
// against it. Mirrors the live multi-user case where each operator
// has their own personal workspace.
func TestTaskAdd_SeedAgentResolvesForOtherWorkspaceCaller(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	// Mint a second user in their own workspace + project so they have
	// a session but no membership in the seed workspace.
	otherWS, err := f.server.deps.Workspaces.Create(context.Background(), "user_other", "other-tier", f.now)
	require.NoError(t, err)
	require.NoError(t, f.server.deps.Workspaces.AddMember(context.Background(), otherWS.ID, "user_other", "admin", "system", f.now))
	otherProj, err := f.server.deps.Projects.Create(context.Background(), "user_other", otherWS.ID, "other-proj", f.now)
	require.NoError(t, err)

	// Create a story in the other user's project so task_add doesn't
	// try to auto-mint against the default workspace.
	otherStory, err := f.server.deps.Stories.Create(context.Background(), story.Story{
		WorkspaceID: otherWS.ID,
		ProjectID:   otherProj.ID,
		Title:       "other-user story",
	}, f.now)
	require.NoError(t, err)

	otherCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "user_other", Source: "session"})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"agent_id": devID,
		"prompt":   "dogfood — sty_6ee30308 resolves seed agent across workspaces",
		"story_id": otherStory.ID,
	}
	res, err := f.server.handleTaskAdd(otherCtx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "task_add rejected seed agent across workspace boundary: %s", errorText(res))

	out := decodeResult(t, res)
	require.Equal(t, otherStory.ID, out["story_id"])
	require.Equal(t, task.StatusPublished, out["status"])
}

// TestTaskAdd_WorkspaceTier_SameWorkspace covers sty_92271886's
// task_add tenancy AC: a scope=workspace agent in caller's workspace
// resolves to the caller's session-bound project (or the default
// project when both lie in the agent's workspace).
func TestTaskAdd_WorkspaceTier_SameWorkspace(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)

	// Mint a scope=workspace agent stamped with the fixture's workspace.
	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers: []string{task.ContractAction("develop")},
	})
	wsAgent, err := f.server.deps.Documents.Create(context.Background(), document.Document{
		Type:        document.TypeAgent,
		Scope:       document.ScopeWorkspace,
		Name:        "ws_developer",
		WorkspaceID: f.wsID,
		Body:        "agent body",
		Status:      document.StatusActive,
		Structured:  settings,
	}, f.now)
	require.NoError(t, err)

	res := callAddHandler(t, f, map[string]any{
		"agent_id": wsAgent.ID,
		"prompt":   "ship the develop work",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, res.IsError, "ws-tier same-workspace failed: %s", errorText(res))

	out := decodeResult(t, res)
	row, err := f.taskStore.GetByID(context.Background(), out["task_id"].(string), nil)
	require.NoError(t, err)
	require.Equal(t, f.wsID, row.WorkspaceID, "task workspace matches agent workspace")
	require.Equal(t, f.projectID, row.ProjectID, "task project resolved from caller's chain")
}

// TestTaskAdd_WorkspaceTier_CrossWorkspaceRejected covers the
// negative branch: caller in a different workspace must not be able
// to dispatch a workspace-tier agent — agent_unavailable.
func TestTaskAdd_WorkspaceTier_CrossWorkspaceRejected(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)

	// Workspace-tier agent in workspace alpha (the fixture's workspace).
	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers: []string{task.ContractAction("develop")},
	})
	wsAgent, err := f.server.deps.Documents.Create(context.Background(), document.Document{
		Type:        document.TypeAgent,
		Scope:       document.ScopeWorkspace,
		Name:        "ws_developer_alpha",
		WorkspaceID: f.wsID,
		Body:        "agent body",
		Status:      document.StatusActive,
		Structured:  settings,
	}, f.now)
	require.NoError(t, err)

	// A second user with a fresh workspace + project — no membership in
	// the agent's workspace.
	otherWS, err := f.server.deps.Workspaces.Create(context.Background(), "user_other", "beta", f.now)
	require.NoError(t, err)
	require.NoError(t, f.server.deps.Workspaces.AddMember(context.Background(), otherWS.ID, "user_other", "admin", "system", f.now))
	otherProj, err := f.server.deps.Projects.Create(context.Background(), "user_other", otherWS.ID, "other-proj", f.now)
	require.NoError(t, err)
	otherStory, err := f.server.deps.Stories.Create(context.Background(), story.Story{
		WorkspaceID: otherWS.ID,
		ProjectID:   otherProj.ID,
		Title:       "other story",
	}, f.now)
	require.NoError(t, err)

	otherCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "user_other", Source: "session"})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"agent_id": wsAgent.ID,
		"prompt":   "should be rejected",
		"story_id": otherStory.ID,
		"action":   task.ContractAction("develop"),
	}
	res, err := f.server.handleTaskAdd(otherCtx, req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected agent_unavailable, got success: %s", errorText(res))
	require.Contains(t, errorText(res), "agent_unavailable")
}

// TestTaskAdd_WorkspaceTier_CrossProjectInSameWorkspace is the
// sty_21d4d830 AC2 regression: a scope=workspace agent must be
// callable against any project in its workspace via story_id, even
// when neither callerActiveProjectID nor defaultProjectID resolves to
// a project in the agent's workspace. Pre-fix, task_add returned
// `agent_unavailable: ... no caller project resolvable in agent
// workspace`; post-fix, the supplied story_id's project is preferred.
func TestTaskAdd_WorkspaceTier_CrossProjectInSameWorkspace(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	ctx := context.Background()

	// Workspace-tier agent in the fixture's workspace.
	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers: []string{task.ContractAction("develop")},
	})
	wsAgent, err := f.server.deps.Documents.Create(ctx, document.Document{
		Type:        document.TypeAgent,
		Scope:       document.ScopeWorkspace,
		Name:        "ws_developer_cross",
		WorkspaceID: f.wsID,
		Body:        "agent body",
		Status:      document.StatusActive,
		Structured:  settings,
	}, f.now)
	require.NoError(t, err)

	// Mint a SECOND project in the same workspace + a story scoped to it.
	otherProj, err := f.server.deps.Projects.Create(ctx, "user_alice", f.wsID, "p2", f.now)
	require.NoError(t, err)
	otherStory, err := f.server.deps.Stories.Create(ctx, story.Story{
		WorkspaceID: f.wsID,
		ProjectID:   otherProj.ID,
		Title:       "cross-project story",
	}, f.now)
	require.NoError(t, err)

	// Force the resolution paths that previously decided cross-project
	// dispatch to fail: clear defaultProjectID and the session-bound
	// active project so the only viable resolution route is the story
	// supplied on the call.
	f.server.deps.DefaultProjectID = ""

	res := callAddHandler(t, f, map[string]any{
		"agent_id": wsAgent.ID,
		"prompt":   "ship the cross-project develop work",
		"story_id": otherStory.ID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, res.IsError, "ws-tier cross-project failed: %s", errorText(res))

	out := decodeResult(t, res)
	row, err := f.taskStore.GetByID(ctx, out["task_id"].(string), nil)
	require.NoError(t, err)
	require.Equal(t, f.wsID, row.WorkspaceID, "task workspace matches agent workspace")
	require.Equal(t, otherProj.ID, row.ProjectID, "task project resolved from supplied story_id")
}

// TestTaskAdd_TriggerRoundtripsViaMCP locks sty_a39e9e41 Slice 1 AC4:
// the MCP `trigger` argument lands verbatim on Task.Trigger bytes after
// a full wire-layer call. Asserts the byte payload (not a parsed map) so
// any future canonicalisation drift would fail the test.
func TestTaskAdd_TriggerRoundtripsViaMCP(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")
	triggerJSON := `{"branch":"feature-x","sha":"deadbeef"}`

	res := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "carry trigger bytes through MCP",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
		"trigger":  triggerJSON,
	})
	require.False(t, res.IsError, "trigger round-trip errored: %v", res.Content)
	out := decodeResult(t, res)
	taskID := out["task_id"].(string)
	require.NotEmpty(t, taskID)

	row, err := f.taskStore.GetByID(context.Background(), taskID, nil)
	require.NoError(t, err)
	require.Equal(t, triggerJSON, string(row.Trigger), "trigger bytes must round-trip verbatim via MCP")
}

// TestTaskAdd_TriggerOmittedViaMCPLeavesNil locks AC6: omitting the
// trigger argument on the MCP call must leave Task.Trigger empty — pre-
// existing task_add callers (every test fixture above) must continue to
// land trigger-free rows.
func TestTaskAdd_TriggerOmittedViaMCPLeavesNil(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	res := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "no trigger today",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, res.IsError)
	taskID := decodeResult(t, res)["task_id"].(string)

	row, err := f.taskStore.GetByID(context.Background(), taskID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, len(row.Trigger), "omitted trigger must leave Task.Trigger empty")
}

// errorText returns the inner text of a tool result regardless of whether
// it came back as IsError or as success — both shapes carry the message
// in Content[0].
func errorText(res *mcpgo.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcpgo.TextContent); ok {
		return strings.TrimSpace(tc.Text)
	}
	return ""
}

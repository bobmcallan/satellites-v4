package mcpserver

import (
	"context"
	"encoding/json"
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
	doc, err := srv.docs.GetByName(context.Background(), "", name, nil)
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
	require.Equal(t, "", out["review_task_id"])
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

	st, err := f.server.stories.GetByID(context.Background(), storyID, []string{f.wsID})
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

func TestTaskAdd_AgentDocDrivesReviewPairing(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	// Mint a fresh agent that declares requires_review=true.
	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers:       []string{task.ContractAction("develop")},
		RequiresReview: true,
	})
	doc, err := f.server.docs.Create(context.Background(), document.Document{
		Type:       document.TypeAgent,
		Scope:      document.ScopeSystem,
		Name:       "review_required_agent",
		Body:       "agent body",
		Status:     document.StatusActive,
		Structured: settings,
	}, f.now)
	require.NoError(t, err)

	res := callAddHandler(t, f, map[string]any{
		"agent_id": doc.ID,
		"prompt":   "do the work",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, res.IsError, errorText(res))
	out := decodeResult(t, res)
	reviewID := out["review_task_id"].(string)
	require.NotEmpty(t, reviewID, "expected paired review task")

	review, err := f.taskStore.GetByID(context.Background(), reviewID, nil)
	require.NoError(t, err)
	require.Equal(t, task.KindReview, review.Kind)
	require.Equal(t, task.StatusPlanned, review.Status)
	require.Equal(t, out["task_id"], review.ParentTaskID)
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
	otherWS, err := f.server.workspaces.Create(context.Background(), "user_other", "other-tier", f.now)
	require.NoError(t, err)
	require.NoError(t, f.server.workspaces.AddMember(context.Background(), otherWS.ID, "user_other", "admin", "system", f.now))
	otherProj, err := f.server.projects.Create(context.Background(), "user_other", otherWS.ID, "other-proj", f.now)
	require.NoError(t, err)

	// Create a story in the other user's project so task_add doesn't
	// try to auto-mint against the default workspace.
	otherStory, err := f.server.stories.Create(context.Background(), story.Story{
		WorkspaceID: otherWS.ID,
		ProjectID:   otherProj.ID,
		Title:       "other-user story",
	}, f.now)
	require.NoError(t, err)

	otherCtx := withCaller(context.Background(), CallerIdentity{UserID: "user_other", Source: "session"})
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
	wsAgent, err := f.server.docs.Create(context.Background(), document.Document{
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
	wsAgent, err := f.server.docs.Create(context.Background(), document.Document{
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
	otherWS, err := f.server.workspaces.Create(context.Background(), "user_other", "beta", f.now)
	require.NoError(t, err)
	require.NoError(t, f.server.workspaces.AddMember(context.Background(), otherWS.ID, "user_other", "admin", "system", f.now))
	otherProj, err := f.server.projects.Create(context.Background(), "user_other", otherWS.ID, "other-proj", f.now)
	require.NoError(t, err)
	otherStory, err := f.server.stories.Create(context.Background(), story.Story{
		WorkspaceID: otherWS.ID,
		ProjectID:   otherProj.ID,
		Title:       "other story",
	}, f.now)
	require.NoError(t, err)

	otherCtx := withCaller(context.Background(), CallerIdentity{UserID: "user_other", Source: "session"})
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

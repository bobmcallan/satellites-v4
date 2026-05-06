package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
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

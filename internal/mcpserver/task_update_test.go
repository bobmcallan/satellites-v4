package mcpserver

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/task"
)

func callUpdateHandler(t *testing.T, f *orchestratorFixture, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := f.server.handleTaskUpdate(f.callerCtx(), req)
	require.NoError(t, err)
	return res
}

func TestTaskUpdate_ClosesWorkTask(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	addRes := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "x",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, addRes.IsError)
	taskID := decodeResult(t, addRes)["task_id"].(string)

	res := callUpdateHandler(t, f, map[string]any{
		"id":      taskID,
		"status":  task.StatusClosed,
		"outcome": task.OutcomeSuccess,
	})
	require.False(t, res.IsError, errorText(res))
	out := decodeResult(t, res)
	require.Equal(t, task.StatusClosed, out["status"])
	require.Equal(t, task.OutcomeSuccess, out["outcome"])
}

func TestTaskUpdate_PublishesPairedReviewSibling(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	settings, _ := document.MarshalAgentSettings(document.AgentSettings{
		Delivers:       []string{task.ContractAction("develop")},
		RequiresReview: true,
	})
	doc, err := f.server.docs.Create(context.Background(), document.Document{
		Type:       document.TypeAgent,
		Scope:      document.ScopeSystem,
		Name:       "needs_review_agent",
		Body:       "x",
		Status:     document.StatusActive,
		Structured: settings,
	}, f.now)
	require.NoError(t, err)

	addRes := callAddHandler(t, f, map[string]any{
		"agent_id": doc.ID,
		"prompt":   "do work",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, addRes.IsError)
	addOut := decodeResult(t, addRes)
	workID := addOut["task_id"].(string)
	plannedReviewID := addOut["review_task_id"].(string)
	require.NotEmpty(t, plannedReviewID, "review must have been minted at task_add time")

	res := callUpdateHandler(t, f, map[string]any{
		"id":     workID,
		"status": task.StatusClosed,
	})
	require.False(t, res.IsError, errorText(res))
	out := decodeResult(t, res)
	require.Equal(t, plannedReviewID, out["published_review_id"])

	// Confirm the review is now published.
	review, err := f.taskStore.GetByID(context.Background(), plannedReviewID, nil)
	require.NoError(t, err)
	require.Equal(t, task.StatusPublished, review.Status)
}

func TestTaskUpdate_RejectsTerminalTask(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")
	addRes := callAddHandler(t, f, map[string]any{
		"agent_id": devID, "prompt": "x", "story_id": f.storyID, "action": task.ContractAction("develop"),
	})
	taskID := decodeResult(t, addRes)["task_id"].(string)

	first := callUpdateHandler(t, f, map[string]any{"id": taskID, "status": task.StatusClosed})
	require.False(t, first.IsError)

	second := callUpdateHandler(t, f, map[string]any{"id": taskID, "status": task.StatusClosed})
	require.True(t, second.IsError)
	require.Contains(t, errorText(second), "task_already_terminal")
}

func TestTaskUpdate_RejectsBadOutcome(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")
	addRes := callAddHandler(t, f, map[string]any{
		"agent_id": devID, "prompt": "x", "story_id": f.storyID, "action": task.ContractAction("develop"),
	})
	taskID := decodeResult(t, addRes)["task_id"].(string)

	res := callUpdateHandler(t, f, map[string]any{
		"id":      taskID,
		"status":  task.StatusClosed,
		"outcome": "wibble",
	})
	require.True(t, res.IsError)
	require.Contains(t, errorText(res), "invalid_outcome")
}

func TestTaskUpdate_RejectsUnsupportedStatus(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")
	addRes := callAddHandler(t, f, map[string]any{
		"agent_id": devID, "prompt": "x", "story_id": f.storyID, "action": task.ContractAction("develop"),
	})
	taskID := decodeResult(t, addRes)["task_id"].(string)

	res := callUpdateHandler(t, f, map[string]any{
		"id":     taskID,
		"status": "in_flight",
	})
	require.True(t, res.IsError)
	require.Contains(t, errorText(res), "unsupported status target")
}

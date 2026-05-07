package mcpserver

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

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

// TestTaskUpdate_CloseHasNoSideEffects locks AC #3 of sty_9f3562b8:
// task_update(closed) mutates exactly the target row. An unrelated
// planned task in the same story stays untouched — closure no longer
// has a publish-on-close side effect.
func TestTaskUpdate_CloseHasNoSideEffects(t *testing.T) {
	t.Parallel()
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	addRes := callAddHandler(t, f, map[string]any{
		"agent_id": devID,
		"prompt":   "develop work",
		"story_id": f.storyID,
		"action":   task.ContractAction("develop"),
	})
	require.False(t, addRes.IsError, errorText(addRes))
	workID := decodeResult(t, addRes)["task_id"].(string)

	// Pre-stage an unrelated planned task in the same story.
	unrelated, err := f.taskStore.Enqueue(context.Background(), task.Task{
		WorkspaceID: f.wsID,
		ProjectID:   f.projectID,
		StoryID:     f.storyID,
		Kind:        task.KindWork,
		Action:      task.ContractAction("develop"),
		AgentID:     devID,
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityMedium,
		Status:      task.StatusPlanned,
	}, f.now)
	require.NoError(t, err)

	res := callUpdateHandler(t, f, map[string]any{
		"id":     workID,
		"status": task.StatusClosed,
	})
	require.False(t, res.IsError, errorText(res))
	out := decodeResult(t, res)
	require.Equal(t, task.StatusClosed, out["status"])
	_, hasPublished := out["published_review_id"]
	require.False(t, hasPublished, "task_update response must not advertise published_review_id")

	// The unrelated planned row is unchanged.
	after, err := f.taskStore.GetByID(context.Background(), unrelated.ID, nil)
	require.NoError(t, err)
	require.Equal(t, task.StatusPlanned, after.Status, "unrelated planned task must not be published")
	require.Equal(t, "", after.ClaimedBy, "unrelated planned task must not be claimed")
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

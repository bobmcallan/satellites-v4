package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/task"
)

func taskTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg:    &config.Config{},
		tasks:  task.NewMemoryStore(),
		ledger: ledger.NewMemoryStore(),
	}
}

func callTaskHandler(t *testing.T, handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error), userID string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{UserID: userID, Source: "apikey"})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(ctx, req)
	require.NoError(t, err)
	return res
}

// seedTask is the test scaffold for the post-checkpoint-12 world: the
// task_enqueue MCP verb is gone, so tests that need a task fixture
// write directly through the store. Returns the task id.
func seedTask(t *testing.T, s *Server, seed task.Task) string {
	t.Helper()
	if seed.Status == "" {
		seed.Status = task.StatusEnqueued
	}
	if seed.Priority == "" {
		seed.Priority = task.PriorityMedium
	}
	if seed.Origin == "" {
		seed.Origin = task.OriginScheduled
	}
	row, err := s.tasks.Enqueue(context.Background(), seed, time.Now().UTC())
	require.NoError(t, err)
	return row.ID
}

func TestTaskClaim_ReturnsNullWhenEmpty(t *testing.T) {
	t.Parallel()
	s := taskTestServer(t)
	res := callTaskHandler(t, s.handleTaskClaim, "apikey", map[string]any{
		"worker_id":    "worker_a",
		"workspace_id": "wksp_a",
	})
	require.False(t, res.IsError)
	text := res.Content[0].(mcpgo.TextContent).Text
	assert.Equal(t, "null", text, "empty queue → null result")
}

func TestTaskClaim_PicksHighestPriorityFirst(t *testing.T) {
	t.Parallel()
	s := taskTestServer(t)
	// Seed medium first.
	seedTask(t, s, task.Task{
		WorkspaceID: "wksp_a",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityMedium,
	})
	// Then critical.
	critID := seedTask(t, s, task.Task{
		WorkspaceID: "wksp_a",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityCritical,
	})

	claimRes := callTaskHandler(t, s.handleTaskClaim, "apikey", map[string]any{
		"worker_id":    "worker_a",
		"workspace_id": "wksp_a",
	})
	require.False(t, claimRes.IsError)
	var claimed map[string]any
	require.NoError(t, json.Unmarshal([]byte(claimRes.Content[0].(mcpgo.TextContent).Text), &claimed))
	assert.Equal(t, critID, claimed["id"], "critical should claim first even though medium was enqueued earlier")
	assert.Equal(t, "claimed", claimed["status"])
}

func TestTaskGet_WorkspaceIsolation(t *testing.T) {
	t.Parallel()
	s := taskTestServer(t)
	taskID := seedTask(t, s, task.Task{
		WorkspaceID: "wksp_a",
		Origin:      task.OriginScheduled,
	})

	// Get with a different workspace_id must reject.
	row, err := s.tasks.GetByID(context.Background(), taskID, []string{"wksp_b"})
	assert.ErrorIs(t, err, task.ErrNotFound)
	assert.Empty(t, row.ID)
}

func TestTaskList_FiltersByStatus(t *testing.T) {
	t.Parallel()
	s := taskTestServer(t)
	// Seed two tasks, claim one.
	for range []int{0, 1} {
		seedTask(t, s, task.Task{WorkspaceID: "wksp_a", Origin: task.OriginScheduled})
	}
	_ = callTaskHandler(t, s.handleTaskClaim, "apikey", map[string]any{
		"worker_id":    "worker_a",
		"workspace_id": "wksp_a",
	})

	listRes := callTaskHandler(t, s.handleTaskList, "apikey", map[string]any{
		"status": task.StatusEnqueued,
	})
	require.False(t, listRes.IsError)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(listRes.Content[0].(mcpgo.TextContent).Text), &rows))
	// 1 enqueued + 1 claimed; filter asks for enqueued.
	assert.Len(t, rows, 1)
	for _, row := range rows {
		assert.Equal(t, "enqueued", row["status"])
	}
}

// TestTaskList_FiltersByStoryAndKind: task_list filters on
// story_id and kind. sty_c6d76a5b checkpoint 14 retired the
// contract_instance_id filter; tasks bind to stories directly.
func TestTaskList_FiltersByStoryAndKind(t *testing.T) {
	t.Parallel()
	s := taskTestServer(t)

	seedTask(t, s, task.Task{
		WorkspaceID: "wksp_a",
		StoryID:     "sty_x",
		Kind:        task.KindWork,
		Origin:      task.OriginStoryStage,
	})
	seedTask(t, s, task.Task{
		WorkspaceID: "wksp_a",
		StoryID:     "sty_x",
		Kind:        task.KindReview,
		Origin:      task.OriginStoryStage,
	})
	seedTask(t, s, task.Task{
		WorkspaceID: "wksp_a",
		StoryID:     "sty_y",
		Kind:        task.KindWork,
		Origin:      task.OriginStoryStage,
	})

	listRes := callTaskHandler(t, s.handleTaskList, "apikey", map[string]any{
		"story_id": "sty_x",
	})
	require.False(t, listRes.IsError)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(listRes.Content[0].(mcpgo.TextContent).Text), &rows))
	assert.Len(t, rows, 2)
	for _, r := range rows {
		assert.Equal(t, "sty_x", r["story_id"])
	}

	listRes2 := callTaskHandler(t, s.handleTaskList, "apikey", map[string]any{
		"kind": task.KindWork,
	})
	require.False(t, listRes2.IsError)
	var rows2 []map[string]any
	require.NoError(t, json.Unmarshal([]byte(listRes2.Content[0].(mcpgo.TextContent).Text), &rows2))
	assert.Len(t, rows2, 2)
	for _, r := range rows2 {
		assert.Equal(t, task.KindWork, r["kind"])
	}

	listRes3 := callTaskHandler(t, s.handleTaskList, "apikey", map[string]any{
		"story_id": "sty_x",
		"kind":     task.KindReview,
	})
	require.False(t, listRes3.IsError)
	var rows3 []map[string]any
	require.NoError(t, json.Unmarshal([]byte(listRes3.Content[0].(mcpgo.TextContent).Text), &rows3))
	assert.Len(t, rows3, 1)
	assert.Equal(t, "sty_x", rows3[0]["story_id"])
	assert.Equal(t, task.KindReview, rows3[0]["kind"])
}

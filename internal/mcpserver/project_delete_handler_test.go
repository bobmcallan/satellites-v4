// Tests for project_delete over the MCP transport (sty_690b06ee).
//
// Handler responsibilities under test: cascade summary surfaces on the
// response, project_has_open_work envelope fires when the project has
// open tasks, archived status is reflected on the response.
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

func TestHandleProjectDelete_HappyPath(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, err := s.handleProjectDelete(ctx, newCallToolReq("project_delete", map[string]any{"id": id}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("project_delete unexpected error: %s", firstText(res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(firstText(res)), &out)
	if out["status"] != project.StatusArchived {
		t.Errorf("status = %v, want %s", out["status"], project.StatusArchived)
	}
	cs, ok := out["cascade_summary"].(map[string]any)
	if !ok {
		t.Fatalf("cascade_summary missing or wrong shape: %v", out["cascade_summary"])
	}
	if cs["stories_cancelled"] == nil || cs["apikeys_archived"] == nil {
		t.Errorf("cascade_summary missing counts: %v", cs)
	}
}

func TestHandleProjectDelete_RejectsWhenOpenWork(t *testing.T) {
	t.Parallel()
	s, wsID := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")

	// Seed a story + open task on the project to trip the pre-flight gate.
	ctx := context.Background()
	st, err := s.deps.Stories.Create(ctx, story.Story{
		WorkspaceID: wsID, ProjectID: id,
		Title: "blocking", Status: story.StatusInProgress, CreatedBy: "u_alice",
	}, s.nowUTC())
	if err != nil {
		t.Fatalf("story seed: %v", err)
	}
	if _, err := s.deps.Tasks.Enqueue(ctx, task.Task{
		WorkspaceID: wsID, ProjectID: id, StoryID: st.ID,
		Action: "contract:develop", Description: "blocking", Status: task.StatusPublished,
		Origin: task.OriginStoryStage, Priority: task.PriorityMedium,
	}, s.nowUTC()); err != nil {
		t.Fatalf("task seed: %v", err)
	}

	callerCtx := withCaller(ctx, auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectDelete(callerCtx, newCallToolReq("project_delete", map[string]any{"id": id}))
	if !res.IsError {
		t.Fatalf("project_delete should reject with open work, got: %s", firstText(res))
	}
	text := firstText(res)
	if !strings.Contains(text, "project_has_open_work") {
		t.Errorf("error envelope missing project_has_open_work tag: %s", text)
	}
}

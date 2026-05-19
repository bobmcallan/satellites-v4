// Tests for sty_d357b28d: workspace_delete + project_delete hard=true
// + project_move_workspace over the MCP transport.
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// ---- workspace_delete -------------------------------------------------------

func TestHandleWorkspaceDelete_RefusesWhenProjectsExist(t *testing.T) {
	t.Parallel()
	s, wsID := newProjectWriteTestServer(t)
	_ = mintProjectForUpdate(t, s, "satellites")

	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleWorkspaceDelete(ctx, newCallToolReq("workspace_delete", map[string]any{"id": wsID}))
	if !res.IsError {
		t.Fatalf("workspace_delete should reject when workspace contains projects; got: %s", firstText(res))
	}
	body := firstText(res)
	if !strings.Contains(body, "workspace_has_projects") {
		t.Errorf("error envelope missing workspace_has_projects tag: %s", body)
	}
	if !strings.Contains(body, "project_ids") {
		t.Errorf("error envelope must surface the blocking project_ids: %s", body)
	}
}

func TestHandleWorkspaceDelete_HappyPathOnEmpty(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	now := s.nowUTC()
	ws, err := s.deps.Workspaces.Create(context.Background(), "u_alice", "throwaway", now)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleWorkspaceDelete(ctx, newCallToolReq("workspace_delete", map[string]any{"id": ws.ID}))
	if res.IsError {
		t.Fatalf("workspace_delete should succeed on empty workspace; got: %s", firstText(res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(firstText(res)), &out)
	if out["deleted"] != true {
		t.Errorf("response missing deleted=true: %v", out)
	}
	if _, err := s.deps.Workspaces.GetByID(context.Background(), ws.ID); err == nil {
		t.Errorf("workspace row still present post-delete")
	}
}

func TestHandleWorkspaceDelete_RejectsNonAdmin(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	now := s.nowUTC()
	ws, err := s.deps.Workspaces.Create(context.Background(), "u_alice", "alice-only", now)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := s.deps.Workspaces.AddMember(context.Background(), ws.ID, "u_bob", workspace.RoleMember, "u_alice", now); err != nil {
		t.Fatalf("add bob: %v", err)
	}
	// Bob is a member but not an admin — should be refused.
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_bob"})
	res, _ := s.handleWorkspaceDelete(ctx, newCallToolReq("workspace_delete", map[string]any{"id": ws.ID}))
	if !res.IsError {
		t.Fatalf("workspace_delete should reject non-admin; got: %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "not_admin") {
		t.Errorf("error envelope missing not_admin tag: %s", firstText(res))
	}
}

// ---- project_delete hard=true ----------------------------------------------

func TestHandleProjectDelete_HardRefusesIfNotArchived(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectDelete(ctx, newCallToolReq("project_delete", map[string]any{"id": id, "hard": true}))
	if !res.IsError {
		t.Fatalf("project_delete hard=true should reject on active project; got: %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "project_not_archived") {
		t.Errorf("error envelope missing project_not_archived tag: %s", firstText(res))
	}
}

func TestHandleProjectDelete_HardCascadeAfterSoftArchive(t *testing.T) {
	t.Parallel()
	s, wsID := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	// Seed a story + a closed task so the cascade has something to purge.
	st, err := s.deps.Stories.Create(context.Background(), story.Story{
		WorkspaceID: wsID, ProjectID: id,
		Title: "to-be-purged", Status: story.StatusBacklog, CreatedBy: "u_alice",
	}, s.nowUTC())
	if err != nil {
		t.Fatalf("story seed: %v", err)
	}

	// Step 1: soft-delete.
	res, _ := s.handleProjectDelete(ctx, newCallToolReq("project_delete", map[string]any{"id": id}))
	if res.IsError {
		t.Fatalf("soft-delete unexpected error: %s", firstText(res))
	}

	// Step 2: hard-delete the now-archived project.
	res, _ = s.handleProjectDelete(ctx, newCallToolReq("project_delete", map[string]any{"id": id, "hard": true}))
	if res.IsError {
		t.Fatalf("hard-delete unexpected error: %s", firstText(res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(firstText(res)), &out)
	if out["hard"] != true {
		t.Errorf("response missing hard=true: %v", out)
	}
	cs, ok := out["cascade_summary"].(map[string]any)
	if !ok {
		t.Fatalf("cascade_summary missing: %v", out)
	}
	if cs["stories_purged"] == nil {
		t.Errorf("cascade_summary missing stories_purged: %v", cs)
	}
	// The project row must be physically gone.
	if _, err := s.deps.Projects.GetByID(context.Background(), id, nil); err == nil {
		t.Errorf("project row still present after hard-delete")
	}
	// The seeded story should also be gone.
	if _, err := s.deps.Stories.GetByID(context.Background(), st.ID, nil); err == nil {
		t.Errorf("story row still present after hard-delete cascade")
	}
}

// ---- project_move_workspace ------------------------------------------------

func TestHandleProjectMoveWorkspace_IdempotentSameSourceTarget(t *testing.T) {
	t.Parallel()
	s, wsID := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectMoveWorkspace(ctx, newCallToolReq("project_move_workspace", map[string]any{
		"id": id, "target_workspace_id": wsID,
	}))
	if res.IsError {
		t.Fatalf("project_move_workspace same-target should succeed idempotently; got: %s", firstText(res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(firstText(res)), &out)
	if out["stories_moved"] != float64(0) {
		t.Errorf("expected zero rows moved on same-target; got stories_moved=%v", out["stories_moved"])
	}
	if out["target_workspace_id"] != wsID {
		t.Errorf("target_workspace_id = %v, want %s", out["target_workspace_id"], wsID)
	}
}

func TestHandleProjectMoveWorkspace_CascadesWorkspaceID(t *testing.T) {
	t.Parallel()
	s, sourceWS := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	now := s.nowUTC()
	targetWS, err := s.deps.Workspaces.Create(context.Background(), "u_alice", "target", now)
	if err != nil {
		t.Fatalf("create target ws: %v", err)
	}

	// Seed a story on the project so the cascade has something to move.
	st, err := s.deps.Stories.Create(context.Background(), story.Story{
		WorkspaceID: sourceWS, ProjectID: id,
		Title: "to-be-moved", Status: story.StatusBacklog, CreatedBy: "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("story seed: %v", err)
	}

	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectMoveWorkspace(ctx, newCallToolReq("project_move_workspace", map[string]any{
		"id": id, "target_workspace_id": targetWS.ID,
	}))
	if res.IsError {
		t.Fatalf("project_move_workspace unexpected error: %s", firstText(res))
	}

	// Project row's workspace_id should now match the target.
	p, err := s.deps.Projects.GetByID(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("project get: %v", err)
	}
	if p.WorkspaceID != targetWS.ID {
		t.Errorf("project workspace_id = %s, want %s", p.WorkspaceID, targetWS.ID)
	}
	// Story row's workspace_id should also have moved.
	updated, err := s.deps.Stories.GetByID(context.Background(), st.ID, nil)
	if err != nil {
		t.Fatalf("story get: %v", err)
	}
	if updated.WorkspaceID != targetWS.ID {
		t.Errorf("story workspace_id = %s, want %s after move", updated.WorkspaceID, targetWS.ID)
	}
}

func TestHandleProjectMoveWorkspace_RejectsNonAdminTarget(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	now := s.nowUTC()
	// Target workspace exists but caller is not a member.
	targetWS, err := s.deps.Workspaces.Create(context.Background(), "u_bob", "bob-ws", now)
	if err != nil {
		t.Fatalf("create target ws: %v", err)
	}
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectMoveWorkspace(ctx, newCallToolReq("project_move_workspace", map[string]any{
		"id": id, "target_workspace_id": targetWS.ID,
	}))
	if !res.IsError {
		t.Fatalf("project_move_workspace should reject non-admin target; got: %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "not_admin_target") {
		t.Errorf("error envelope missing not_admin_target: %s", firstText(res))
	}
}

// ---- helper: unused-import suppression --------------------------------------

var _ = project.StatusArchived
var _ = time.Time{}

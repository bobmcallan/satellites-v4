// Tests for project_update over the MCP transport (sty_690b06ee).
//
// Handler responsibilities under test: required id, pointer-semantic
// description (nil = unchanged, *"" = clear, *<v> = set), pointer-
// semantic status validated against {active, archived}, and the
// existing name / mcp_url paths stay intact.
package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bobmcallan/satellites/internal/auth"
)

func mintProjectForUpdate(t *testing.T, s *Server, name string) string {
	t.Helper()
	got := callProjectAdd(t, s, map[string]any{"name": name})
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("seed project_add returned no id: %v", got)
	}
	return id
}

func callProjectUpdate(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, err := s.handleProjectUpdate(ctx, newCallToolReq("project_update", args))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("project_update unexpected error: %s", firstText(res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(firstText(res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestHandleProjectUpdate_RequiresID(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectUpdate(ctx, newCallToolReq("project_update", map[string]any{}))
	if !res.IsError {
		t.Fatalf("expected error when id missing, got: %s", firstText(res))
	}
}

func TestHandleProjectUpdate_DescriptionSet(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	got := callProjectUpdate(t, s, map[string]any{
		"id":          id,
		"description": "patched",
	})
	if got["description"] != "patched" {
		t.Errorf("description = %v, want patched", got["description"])
	}
}

func TestHandleProjectUpdate_DescriptionClear(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	_ = callProjectUpdate(t, s, map[string]any{"id": id, "description": "initial"})
	got := callProjectUpdate(t, s, map[string]any{"id": id, "description": ""})
	if got["description"] != nil && got["description"] != "" {
		t.Errorf("description should clear, got %v", got["description"])
	}
}

func TestHandleProjectUpdate_StatusFlip(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	got := callProjectUpdate(t, s, map[string]any{"id": id, "status": "archived"})
	if got["status"] != "archived" {
		t.Errorf("status = %v, want archived", got["status"])
	}
	got = callProjectUpdate(t, s, map[string]any{"id": id, "status": "active"})
	if got["status"] != "active" {
		t.Errorf("status = %v, want active", got["status"])
	}
}

func TestHandleProjectUpdate_StatusInvalidRejected(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	id := mintProjectForUpdate(t, s, "satellites")
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectUpdate(ctx, newCallToolReq("project_update", map[string]any{
		"id":     id,
		"status": "deleted",
	}))
	if !res.IsError {
		t.Fatalf("invalid status should reject, got: %s", firstText(res))
	}
}

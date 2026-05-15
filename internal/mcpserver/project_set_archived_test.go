// Tests project_set's archived-skip behaviour (sty_690b06ee R3.7).
//
// After project_delete archives a project, project_set against the
// archived project's repo URL must return no_project_for_remote rather
// than resolve to the dead row. Without this, the orchestrator's
// dogfood chain step 5 would silently re-resolve to the archived
// project and the cascade's invariants would not be observable.
package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bobmcallan/satellites/internal/auth"
)

func TestHandleProjectSet_SkipsArchived(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	// Mint a project bound to a repo URL.
	added := callProjectAdd(t, s, map[string]any{
		"name":     "satellites",
		"repo_url": "git@github.com:bobmcallan/satellites.git",
	})
	id, _ := added["id"].(string)
	if id == "" {
		t.Fatalf("project_add returned no id: %v", added)
	}

	// Sanity: project_set resolves while active.
	res, _ := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{
		"repo_url": "git@github.com:bobmcallan/satellites.git",
	}))
	if res.IsError {
		t.Fatalf("project_set unexpected error while active: %s", firstText(res))
	}
	var resolved map[string]any
	_ = json.Unmarshal([]byte(firstText(res)), &resolved)
	if resolved["status"] != "resolved" {
		t.Errorf("status = %v, want resolved while active", resolved["status"])
	}

	// Archive via project_delete.
	delRes, _ := s.handleProjectDelete(ctx, newCallToolReq("project_delete", map[string]any{"id": id}))
	if delRes.IsError {
		t.Fatalf("project_delete unexpected error: %s", firstText(delRes))
	}

	// project_set against the same repo_url must now skip.
	postRes, _ := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{
		"repo_url": "git@github.com:bobmcallan/satellites.git",
	}))
	if postRes.IsError {
		t.Fatalf("project_set after archive unexpected error: %s", firstText(postRes))
	}
	var skipped map[string]any
	_ = json.Unmarshal([]byte(firstText(postRes)), &skipped)
	if skipped["status"] != "no_project_for_remote" {
		t.Errorf("status = %v, want no_project_for_remote after archive", skipped["status"])
	}
}

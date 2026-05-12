// MCP roundtrip tests for the changelog_* verbs (sty_12af0bdc).
package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

func newChangelogTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	led := ledger.NewMemoryStore()
	docs := document.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	projects := project.NewMemoryStore()
	wss := workspace.NewMemoryStore()
	sessions := session.NewMemoryStore()
	cl := changelog.NewMemoryStore()
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:       docs,
			Projects:   projects,
			Ledger:    led,
			Stories:     stories,
			Workspaces: wss,
			Sessions:   sessions,
			Changelog: cl,
		},
	})
}

func seedChangelogProject(t *testing.T, s *Server) (projectID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)
	proj, _ := s.deps.Projects.Create(ctx, "u_alice", ws.ID, "alpha-1", now)
	return proj.ID
}

func TestChangelog_AddListGetUpdateDeleteRoundTrip(t *testing.T) {
	t.Parallel()
	s := newChangelogTestServer(t)
	projectID := seedChangelogProject(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	// add
	addRes, _ := s.handleChangelogAdd(ctx, newCallToolReq("changelog_add", map[string]any{
		"project_id":   projectID,
		"service":      "satellites",
		"version_from": "0.0.165",
		"version_to":   "0.0.166",
		"content":      "tag chips visible\n\nV3-parity layout fix.",
	}))
	if addRes.IsError {
		t.Fatalf("add error: %s", firstText(addRes))
	}
	var added changelog.Changelog
	_ = json.Unmarshal([]byte(firstText(addRes)), &added)
	if added.ID == "" || added.VersionTo != "0.0.166" {
		t.Fatalf("add unexpected payload: %+v", added)
	}

	// list
	listRes, _ := s.handleChangelogList(ctx, newCallToolReq("changelog_list", map[string]any{
		"project_id": projectID,
	}))
	if listRes.IsError {
		t.Fatalf("list error: %s", firstText(listRes))
	}
	var rows []changelog.Changelog
	_ = json.Unmarshal([]byte(firstText(listRes)), &rows)
	if len(rows) != 1 || rows[0].ID != added.ID {
		t.Errorf("list = %+v, want one row matching %s", rows, added.ID)
	}

	// get
	getRes, _ := s.handleChangelogGet(ctx, newCallToolReq("changelog_get", map[string]any{"id": added.ID}))
	if getRes.IsError {
		t.Fatalf("get error: %s", firstText(getRes))
	}

	// update
	updRes, _ := s.handleChangelogUpdate(ctx, newCallToolReq("changelog_update", map[string]any{
		"id":      added.ID,
		"content": "rewritten body",
	}))
	if updRes.IsError {
		t.Fatalf("update error: %s", firstText(updRes))
	}
	var updated changelog.Changelog
	_ = json.Unmarshal([]byte(firstText(updRes)), &updated)
	if updated.Content != "rewritten body" {
		t.Errorf("Content = %q, want rewritten body", updated.Content)
	}
	if updated.VersionTo != "0.0.166" {
		t.Errorf("omitted field changed; VersionTo = %q", updated.VersionTo)
	}

	// delete
	delRes, _ := s.handleChangelogDelete(ctx, newCallToolReq("changelog_delete", map[string]any{"id": added.ID}))
	if delRes.IsError {
		t.Fatalf("delete error: %s", firstText(delRes))
	}

	// list now empty
	listRes2, _ := s.handleChangelogList(ctx, newCallToolReq("changelog_list", map[string]any{"project_id": projectID}))
	var rows2 []changelog.Changelog
	_ = json.Unmarshal([]byte(firstText(listRes2)), &rows2)
	if len(rows2) != 0 {
		t.Errorf("list after delete = %d rows, want 0", len(rows2))
	}
}

func TestChangelog_CrossOwnerNotFound(t *testing.T) {
	t.Parallel()
	s := newChangelogTestServer(t)
	projectID := seedChangelogProject(t, s)

	aliceCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	addRes, _ := s.handleChangelogAdd(aliceCtx, newCallToolReq("changelog_add", map[string]any{
		"project_id": projectID, "service": "satellites", "content": "x",
	}))
	if addRes.IsError {
		t.Fatalf("add: %s", firstText(addRes))
	}
	var row changelog.Changelog
	_ = json.Unmarshal([]byte(firstText(addRes)), &row)

	// Carol tries to read Alice's row — must be rejected.
	carolCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_carol"})
	getRes, _ := s.handleChangelogGet(carolCtx, newCallToolReq("changelog_get", map[string]any{"id": row.ID}))
	if !getRes.IsError {
		t.Errorf("cross-owner get should be rejected, got success: %s", firstText(getRes))
	}
}

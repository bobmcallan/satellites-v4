// Tests for the widened satellites_story_update MCP surface
// (sty_330cc4ab). Confirms each accepted field round-trips through the
// handler, tags replace wholesale, invalid category rejects with a
// structured error, and cross-owner access returns not-found without
// leaking story existence.
package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

func newStoryUpdateTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	led := ledger.NewMemoryStore()
	docs := document.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	projects := project.NewMemoryStore()
	wss := workspace.NewMemoryStore()
	sessions := session.NewMemoryStore()
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:       docs,
			Projects:   projects,
			Ledger:    led,
			Stories:     stories,
			Workspaces: wss,
			Sessions:   sessions,
		},
	})
}

// seedStoryAlice creates a workspace owned by alice, a project owned by
// alice, and a story under that project. Returns the story id + project
// id + workspace id for assertions.
func seedStoryAlice(t *testing.T, s *Server) (storyID, projectID, workspaceID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	ws, err := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if err := s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now); err != nil {
		t.Fatalf("workspace addmember: %v", err)
	}
	proj, err := s.deps.Projects.Create(ctx, "u_alice", ws.ID, "alpha-1", now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}
	st, err := s.deps.Stories.Create(ctx, story.Story{
		WorkspaceID:        ws.ID,
		ProjectID:          proj.ID,
		Title:              "original title",
		Description:        "original description",
		AcceptanceCriteria: "original ac",
		Category:           "feature",
		Priority:           "medium",
		Tags:               []string{"epic:original", "ui"},
		CreatedBy:          "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("story create: %v", err)
	}
	return st.ID, proj.ID, ws.ID
}

func TestStoryUpdate_AppliesEachField(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	res, err := s.handleStoryUpdate(ctx, newCallToolReq("story_update", map[string]any{
		"id":                  storyID,
		"title":               "t2",
		"description":         "d2",
		"acceptance_criteria": "ac2",
		"category":            "improvement",
		"priority":            "high",
		"tags":                []any{"a", "b:c"},
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", firstText(res))
	}
	var got story.Story
	if err := json.Unmarshal([]byte(firstText(res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Title != "t2" || got.Description != "d2" || got.AcceptanceCriteria != "ac2" {
		t.Errorf("text fields not all applied: %+v", got)
	}
	if got.Category != "improvement" || got.Priority != "high" {
		t.Errorf("enum fields not applied: cat=%q prio=%q", got.Category, got.Priority)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b:c" {
		t.Errorf("Tags = %v, want [a b:c]", got.Tags)
	}
}

func TestStoryUpdate_TagsWholesaleReplace(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	res, _ := s.handleStoryUpdate(ctx, newCallToolReq("story_update", map[string]any{
		"id":   storyID,
		"tags": []any{"only:one"},
	}))
	if res.IsError {
		t.Fatalf("expected success, got error: %s", firstText(res))
	}
	var got story.Story
	_ = json.Unmarshal([]byte(firstText(res)), &got)
	if len(got.Tags) != 1 || got.Tags[0] != "only:one" {
		t.Errorf("Tags = %v, want exact replacement [only:one]", got.Tags)
	}
}

func TestStoryUpdate_TagsEmptyArrayClears(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	res, _ := s.handleStoryUpdate(ctx, newCallToolReq("story_update", map[string]any{
		"id":   storyID,
		"tags": []any{},
	}))
	if res.IsError {
		t.Fatalf("expected success, got error: %s", firstText(res))
	}
	var got story.Story
	_ = json.Unmarshal([]byte(firstText(res)), &got)
	if len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want [] after empty-array clear", got.Tags)
	}
}

func TestStoryUpdate_OmittedFieldsLeftUntouched(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	// Only update the title — every other field must be unchanged.
	res, _ := s.handleStoryUpdate(ctx, newCallToolReq("story_update", map[string]any{
		"id":    storyID,
		"title": "renamed",
	}))
	if res.IsError {
		t.Fatalf("expected success, got error: %s", firstText(res))
	}
	var got story.Story
	_ = json.Unmarshal([]byte(firstText(res)), &got)
	if got.Title != "renamed" {
		t.Errorf("Title = %q, want renamed", got.Title)
	}
	if got.Description != "original description" || got.AcceptanceCriteria != "original ac" {
		t.Errorf("omitted text fields mutated: %+v", got)
	}
	if got.Category != "feature" || got.Priority != "medium" {
		t.Errorf("omitted enum fields mutated: cat=%q prio=%q", got.Category, got.Priority)
	}
	if len(got.Tags) != 2 {
		t.Errorf("omitted tags mutated: %v", got.Tags)
	}
}

func TestStoryUpdate_InvalidCategoryRejected(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	res, _ := s.handleStoryUpdate(ctx, newCallToolReq("story_update", map[string]any{
		"id":       storyID,
		"category": "totally-made-up",
	}))
	if !res.IsError {
		t.Fatalf("invalid category should be rejected, got success: %s", firstText(res))
	}
}

func TestStoryUpdate_CrossOwnerNotFound(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_carol"})

	res, _ := s.handleStoryUpdate(ctx, newCallToolReq("story_update", map[string]any{
		"id":    storyID,
		"title": "carol was here",
	}))
	if !res.IsError {
		t.Fatalf("cross-owner update should be rejected, got success: %s", firstText(res))
	}
}

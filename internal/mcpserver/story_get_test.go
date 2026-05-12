package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// TestStoryGet_HappyPath verifies that story_get returns the
// orientation bundle in a single roundtrip: story body, owning project,
// recent ledger evidence, agent_process body, and the category template.
func TestStoryGet_HappyPath(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, projectID, wsID := seedStoryAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	now := time.Now().UTC()

	// Seed an agent_process system-default artifact so AgentProcess
	// resolves to non-empty (the resolver walks project override → system
	// default → "").
	if _, err := s.deps.Documents.Create(ctx, document.Document{
		Type:   document.TypeArtifact,
		Scope:  document.ScopeSystem,
		Name:   agentprocess.SystemDefaultName,
		Status: document.StatusActive,
		Tags:   []string{agentprocess.KindTag},
		Body:   "# how to act\nfollow the contract.",
	}, now); err != nil {
		t.Fatalf("seed agent_process: %v", err)
	}

	// Seed a couple of ledger rows so RecentEvidence is populated.
	storyRef := storyID
	for i := 0; i < 3; i++ {
		if _, err := s.deps.Ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: wsID,
			ProjectID:   projectID,
			StoryID:     &storyRef,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:test"},
			Content:     "evidence row",
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			Status:      ledger.StatusActive,
			CreatedBy:   "u_alice",
		}, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed ledger %d: %v", i, err)
		}
	}

	res, err := s.handleStoryGet(ctx, newCallToolReq("story_get", map[string]any{
		"id": storyID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", firstText(res))
	}

	var view client.StoryGetOutput
	if err := json.Unmarshal([]byte(firstText(res)), &view); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, firstText(res))
	}

	if view.Story.ID != storyID {
		t.Errorf("Story.ID = %q, want %q", view.Story.ID, storyID)
	}
	if view.Story.Title != "original title" {
		t.Errorf("Story.Title = %q, want %q", view.Story.Title, "original title")
	}
	if view.Project == nil || view.Project.ID != projectID {
		t.Errorf("Project missing or wrong id; got=%+v want=%s", view.Project, projectID)
	}
	if len(view.RecentEvidence) != 3 {
		t.Errorf("RecentEvidence len = %d, want 3", len(view.RecentEvidence))
	}
	if !strings.Contains(view.AgentProcess, "follow the contract") {
		t.Errorf("AgentProcess missing seed body; got %q", view.AgentProcess)
	}
}

// TestStoryGet_StoryNotFound returns the same structured error as
// story_get when the id is unknown — never leaks existence to a
// non-owning caller.
func TestStoryGet_StoryNotFound(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, err := s.handleStoryGet(ctx, newCallToolReq("story_get", map[string]any{
		"id": "sty_missing0",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError; got %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "story not found") {
		t.Errorf("error text = %q, want story not found", firstText(res))
	}
}

// TestStoryGet_CrossOwnerRejected ensures the project-scoped owner
// check matches story_get: a caller who isn't the project owner gets
// "story not found" rather than the row body.
func TestStoryGet_CrossOwnerRejected(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	storyID, _, _ := seedStoryAlice(t, s)
	bobCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_bob"})
	res, err := s.handleStoryGet(bobCtx, newCallToolReq("story_get", map[string]any{
		"id": storyID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for cross-owner; got %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "story not found") {
		t.Errorf("error text = %q, want story not found", firstText(res))
	}
}

// TestStoryGet_RequiresID rejects calls missing the id arg with the
// canonical mcpgo required-arg error.
func TestStoryGet_RequiresID(t *testing.T) {
	t.Parallel()
	s := newStoryUpdateTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, err := s.handleStoryGet(ctx, newCallToolReq("story_get", map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for missing id; got %s", firstText(res))
	}
}

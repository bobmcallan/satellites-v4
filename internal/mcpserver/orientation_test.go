// Tests for the project orientation bundle (sty_31d51494 layer 2,
// sty_48e38e83 rename). Cover the bundle helper, project_set's
// enriched return, project_get's orientation bundle, and the auto-bind
// on Mcp-Session-Id.
package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

const sampleProjectIntentBody = "# Project intent test body — what this project is about."
const sampleSystemPrincipleMD = `---
name: pr_test_universal
---
# Universal principle

Body.`

const sampleProjectPrincipleMD = `---
name: pr_test_project
---
# Project-scope principle

Body.`

func newOrientationFixture(t *testing.T) (*Server, project.Project, string) {
	t.Helper()
	cfg := &config.Config{Env: "dev", PublicURL: "https://sat.test"}
	docs := document.NewMemoryStore()
	led := ledger.NewMemoryStore()
	s := New(cfg, satarbor.New("info"), time.Now(), Deps{
		DocStore:       docs,
		ProjectStore:   project.NewMemoryStore(),
		RepoStore:      repo.NewMemoryStore(),
		SessionStore:   session.NewMemoryStore(),
		WorkspaceStore: workspace.NewMemoryStore(),
		LedgerStore:    led,
		StoryStore:     story.NewMemoryStore(led),
	})
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	now := time.Now().UTC()

	ws, err := s.workspaces.Create(ctx, "u_alice", "alpha", now)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := s.workspaces.AddMember(ctx, ws.ID, "u_alice", "admin", "u_alice", now); err != nil {
		t.Fatalf("member: %v", err)
	}
	p, err := s.projects.Create(ctx, "u_alice", ws.ID, "satellites", now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// sty_14dfd05b: the project↔remote binding lives on the repo row.
	// project_set walks repos.GetByRemote(ws, canonical) → projectID, so
	// the orientation fixture must seed a repo row for the project.
	if _, err := s.repos.Create(ctx, repo.Repo{
		WorkspaceID: ws.ID,
		ProjectID:   p.ID,
		GitRemote:   "https://github.com/owner/repo",
		Status:      repo.StatusActive,
	}, now); err != nil {
		t.Fatalf("repo: %v", err)
	}

	pid := p.ID
	if _, err := docs.Create(ctx, document.Document{
		WorkspaceID: ws.ID,
		ProjectID:   &pid,
		Type:        document.TypeArtifact,
		Scope:       document.ScopeProject,
		Name:        client.ProjectIntentArtifactName,
		Body:        sampleProjectIntentBody,
		Tags:        []string{"kind:project-intent"},
		Status:      document.StatusActive,
	}, now); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	if _, err := docs.Create(ctx, document.Document{
		Type:   document.TypePrinciple,
		Scope:  document.ScopeSystem,
		Name:   "pr_test_universal",
		Body:   sampleSystemPrincipleMD,
		Status: document.StatusActive,
	}, now); err != nil {
		t.Fatalf("seed sys principle: %v", err)
	}
	if _, err := docs.Create(ctx, document.Document{
		WorkspaceID: ws.ID,
		ProjectID:   &pid,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeProject,
		Name:        "pr_test_project",
		Body:        sampleProjectPrincipleMD,
		Status:      document.StatusActive,
	}, now); err != nil {
		t.Fatalf("seed proj principle: %v", err)
	}
	return s, p, ws.ID
}

func TestBuildOrientation_ReturnsIntentAndPrinciples(t *testing.T) {
	t.Parallel()
	s, p, _ := newOrientationFixture(t)
	bundle := s.cli().BuildOrientation(context.Background(), p)
	if bundle.IntentBody != sampleProjectIntentBody {
		t.Errorf("IntentBody = %q, want %q", bundle.IntentBody, sampleProjectIntentBody)
	}
	scopes := map[string]int{}
	for _, pr := range bundle.Principles {
		scopes[pr.Scope]++
	}
	if scopes["system"] != 1 {
		t.Errorf("system principles = %d, want 1", scopes["system"])
	}
	if scopes["project"] != 1 {
		t.Errorf("project principles = %d, want 1", scopes["project"])
	}
	for _, pr := range bundle.Principles {
		if pr.Body == "" {
			t.Errorf("principle %q has empty body", pr.Name)
		}
	}
}

// TestBuildOrientation_IncludesWorkspaceTier covers sty_7f5585e9: a
// scope=workspace principle stamped to the bound project's workspace
// is appended to bundle.Principles with Scope:"workspace". A second
// workspace-tier principle in a *different* workspace is NOT included
// — cross-workspace isolation.
func TestBuildOrientation_IncludesWorkspaceTier(t *testing.T) {
	t.Parallel()
	s, p, wsID := newOrientationFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: wsID,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeWorkspace,
		Name:        "pr_test_workspace",
		Body:        "# Workspace principle\n\nBody.",
		Status:      document.StatusActive,
	}, now); err != nil {
		t.Fatalf("seed wksp principle: %v", err)
	}
	otherWS, err := s.workspaces.Create(ctx, "u_carol", "carol-tier", now)
	if err != nil {
		t.Fatalf("carol ws: %v", err)
	}
	if _, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: otherWS.ID,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeWorkspace,
		Name:        "pr_other_workspace",
		Body:        "# Different workspace.\n",
		Status:      document.StatusActive,
	}, now); err != nil {
		t.Fatalf("seed other ws principle: %v", err)
	}

	bundle := s.cli().BuildOrientation(ctx, p)
	scopes := map[string]int{}
	names := map[string]bool{}
	for _, pr := range bundle.Principles {
		scopes[pr.Scope]++
		names[pr.Name] = true
	}
	if scopes["workspace"] != 1 {
		t.Errorf("workspace principles = %d, want 1 (got names=%v)", scopes["workspace"], names)
	}
	if !names["pr_test_workspace"] {
		t.Errorf("expected pr_test_workspace; got %v", names)
	}
	if names["pr_other_workspace"] {
		t.Errorf("cross-workspace principle leaked into bundle: %v", names)
	}
}

func TestProjectSet_ReturnsOrientationBundle(t *testing.T) {
	t.Parallel()
	s, p, _ := newOrientationFixture(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})

	res, err := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{
		"repo_url":   "git@github.com:owner/repo.git",
		"session_id": "sess_xyz",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", firstText(res))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["project_id"] != p.ID {
		t.Errorf("project_id = %v, want %s", body["project_id"], p.ID)
	}
	intent, _ := body["intent_body"].(string)
	if !strings.Contains(intent, "Project intent test body") {
		t.Errorf("intent_body missing seeded content: %q", intent)
	}
	principles, _ := body["principles"].([]any)
	if len(principles) != 2 {
		t.Errorf("principles len = %d, want 2 (1 system + 1 project)", len(principles))
	}
}

func TestProjectSet_AutoRegistersSessionWhenNotPresent(t *testing.T) {
	t.Parallel()
	s, p, _ := newOrientationFixture(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})

	// No session pre-registered. project_set must create the row and
	// stamp active_project_id on it (auto-bind).
	if _, err := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{
		"repo_url":   "https://github.com/owner/repo",
		"session_id": "sess_fresh",
	})); err != nil {
		t.Fatalf("handler: %v", err)
	}
	got, err := s.sessions.Get(ctx, "u_alice", "sess_fresh")
	if err != nil {
		t.Fatalf("session lookup: %v — auto-register should have created the row", err)
	}
	if got.ActiveProjectID != p.ID {
		t.Errorf("active_project_id = %q, want %q", got.ActiveProjectID, p.ID)
	}
}

// TestProjectGet_ReturnsOrientationBundle: post-sty_48e38e83 the
// project_get verb returns the orientation bundle (project view +
// intent_body + principles[]) for an explicit project id, replacing
// the prior session-keyed project_context refresh surface.
func TestProjectGet_ReturnsOrientationBundle(t *testing.T) {
	t.Parallel()
	s, p, _ := newOrientationFixture(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})

	res, err := s.handleProjectGet(ctx, newCallToolReq("project_get", map[string]any{
		"id": p.ID,
	}))
	if err != nil {
		t.Fatalf("project_get: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", firstText(res))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	proj, _ := body["project"].(map[string]any)
	if proj["id"] != p.ID {
		t.Errorf("project.id = %v, want %s", proj["id"], p.ID)
	}
	intent, _ := body["intent_body"].(string)
	if !strings.Contains(intent, "Project intent test body") {
		t.Errorf("intent_body missing seeded content: %q", intent)
	}
	principles, _ := body["principles"].([]any)
	if len(principles) != 2 {
		t.Errorf("principles len = %d, want 2", len(principles))
	}
}

func TestProjectGet_UnknownIDReturnsError(t *testing.T) {
	t.Parallel()
	s, _, _ := newOrientationFixture(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	res, err := s.handleProjectGet(ctx, newCallToolReq("project_get", map[string]any{
		"id": "proj_missing0",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for unknown id; body=%s", firstText(res))
	}
	if body := firstText(res); !strings.Contains(body, "project not found") {
		t.Errorf("expected project not found; got %q", body)
	}
}

func TestStoryGet_IncludesOrientationBundle(t *testing.T) {
	t.Parallel()
	s, p, wsID := newOrientationFixture(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	now := time.Now().UTC()

	st, err := s.stories.Create(ctx, story.Story{
		WorkspaceID:        wsID,
		ProjectID:          p.ID,
		Title:              "test-story",
		Description:        "desc",
		AcceptanceCriteria: "AC",
		Category:           "feature",
		Priority:           "high",
		CreatedBy:          "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("story create: %v", err)
	}
	res, err := s.handleStoryGet(ctx, newCallToolReq("story_get", map[string]any{
		"id": st.ID,
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", firstText(res))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	intent, _ := body["intent_body"].(string)
	if !strings.Contains(intent, "Project intent test body") {
		t.Errorf("story_get.intent_body missing seeded content")
	}
	principles, _ := body["principles"].([]any)
	if len(principles) < 1 {
		t.Errorf("story_get.principles empty; want at least 1")
	}
}

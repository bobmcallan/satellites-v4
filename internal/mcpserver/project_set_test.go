// sty_4db7c3a3 — `project_set` MCP verb tests. The verb is the agent's
// first call when working in a local repo: it normalises the supplied
// git remote URL and resolves an existing project keyed on
// (workspace_id, git_remote). It never creates a project.
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
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/workspace"
)

func newProjectSetTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev", PublicURL: "https://sat.test"}
	return New(cfg, satarbor.New("info"), time.Now(), Deps{
		Client: client.Deps{
			Projects:   project.NewMemoryStore(),
			Repos:      repo.NewMemoryStore(),
			Sessions:   session.NewMemoryStore(),
			Workspaces: workspace.NewMemoryStore(),
		},
	})
}

// seedProjectWithRemote creates a project plus its repo row (canonical
// remote) — the post-sty_14dfd05b shape. Replaces the legacy
// CreateWithRemote fixture path.
func seedProjectWithRemote(t *testing.T, s *Server, ctx context.Context, ownerUserID, wsID, name, canonicalRemote string, now time.Time) project.Project {
	t.Helper()
	p, err := s.deps.Projects.Create(ctx, ownerUserID, wsID, name, now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}
	if _, err := s.deps.Repos.Create(ctx, repo.Repo{
		WorkspaceID: wsID,
		ProjectID:   p.ID,
		GitRemote:   canonicalRemote,
		Status:      repo.StatusActive,
	}, now); err != nil {
		t.Fatalf("repo create: %v", err)
	}
	return p
}

func decodeProjectSet(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, raw)
	}
	return out
}

func TestProjectSet_HappyPath_StampsActiveProject(t *testing.T) {
	t.Parallel()
	s := newProjectSetTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	now := time.Now().UTC()

	ws, err := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if err := s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", "admin", "u_alice", now); err != nil {
		t.Fatalf("workspace member: %v", err)
	}
	if _, err := s.deps.Sessions.Register(ctx, "u_alice", "sess_abc", session.SourceSessionStart, now); err != nil {
		t.Fatalf("session register: %v", err)
	}
	p := seedProjectWithRemote(t, s, ctx, "u_alice", ws.ID, "satellites", "https://github.com/owner/repo", now)

	res, err := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{
		"repo_url":   "git@github.com:owner/repo.git",
		"session_id": "sess_abc",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError; body=%s", firstText(res))
	}
	body := decodeProjectSet(t, firstText(res))
	if body["status"] != "resolved" {
		t.Errorf("status = %v, want resolved", body["status"])
	}
	if body["project_id"] != p.ID {
		t.Errorf("project_id = %v, want %s", body["project_id"], p.ID)
	}
	if body["repo_url_canonical"] != "https://github.com/owner/repo" {
		t.Errorf("canonical = %v, want https://github.com/owner/repo", body["repo_url_canonical"])
	}
	wantMCP := "https://sat.test/mcp?project_id=" + p.ID
	if body["mcp_url"] != wantMCP {
		t.Errorf("mcp_url = %v, want %s", body["mcp_url"], wantMCP)
	}

	// active_project_id stamped on the session row.
	got, err := s.deps.Sessions.Get(ctx, "u_alice", "sess_abc")
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	if got.ActiveProjectID != p.ID {
		t.Errorf("ActiveProjectID = %q, want %q", got.ActiveProjectID, p.ID)
	}
}

func TestProjectSet_NoProjectForRemote(t *testing.T) {
	t.Parallel()
	s := newProjectSetTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	now := time.Now().UTC()
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", "admin", "u_alice", now)

	res, err := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{
		"repo_url": "git@github.com:owner/unknown.git",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError; body=%s", firstText(res))
	}
	body := decodeProjectSet(t, firstText(res))
	if body["status"] != "no_project_for_remote" {
		t.Errorf("status = %v, want no_project_for_remote", body["status"])
	}
	if body["repo_url_canonical"] != "https://github.com/owner/unknown" {
		t.Errorf("canonical = %v", body["repo_url_canonical"])
	}
	if _, ok := body["project_id"]; ok {
		t.Errorf("project_id leaked on no-match path: %v", body["project_id"])
	}
}

func TestProjectSet_NormalisationParity(t *testing.T) {
	t.Parallel()
	// Pin the same canonical form across every accepted input shape so
	// project_set + project_add cannot drift on what counts as the
	// same repo. AC3.
	want := "https://github.com/owner/repo"
	cases := []string{
		"git@github.com:owner/repo.git",
		"git@github.com:owner/repo",
		"ssh://git@github.com/owner/repo.git",
		"https://github.com/owner/repo.git",
		"https://github.com/owner/repo/",
		"HTTPS://GitHub.com/owner/repo",
	}
	for _, in := range cases {
		s := newProjectSetTestServer(t)
		ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
		now := time.Now().UTC()
		ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
		_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", "admin", "u_alice", now)
		seedProjectWithRemote(t, s, ctx, "u_alice", ws.ID, "p", want, now)
		res, _ := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{"repo_url": in}))
		body := decodeProjectSet(t, firstText(res))
		if body["status"] != "resolved" {
			t.Errorf("input %q: status = %v, want resolved", in, body["status"])
		}
		if body["repo_url_canonical"] != want {
			t.Errorf("input %q: canonical = %v, want %s", in, body["repo_url_canonical"], want)
		}
	}
}

func TestProjectSet_CrossWorkspaceIsolation(t *testing.T) {
	t.Parallel()
	s := newProjectSetTestServer(t)
	now := time.Now().UTC()
	ctx := context.Background()

	wsA, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, wsA.ID, "u_alice", "admin", "u_alice", now)
	wsB, _ := s.deps.Workspaces.Create(ctx, "u_bob", "beta", now)
	_ = s.deps.Workspaces.AddMember(ctx, wsB.ID, "u_bob", "admin", "u_bob", now)

	// Same canonical remote registered in workspace A only.
	seedProjectWithRemote(t, s, ctx, "u_alice", wsA.ID, "shared", "https://github.com/owner/shared", now)

	// Bob (in workspace B) can't see Alice's project.
	bobCtx := withCaller(ctx, auth.CallerIdentity{UserID: "u_bob", Source: "session"})
	res, _ := s.handleProjectSet(bobCtx, newCallToolReq("project_set", map[string]any{
		"repo_url": "git@github.com:owner/shared.git",
	}))
	body := decodeProjectSet(t, firstText(res))
	if body["status"] != "no_project_for_remote" {
		t.Errorf("cross-workspace status = %v, want no_project_for_remote", body["status"])
	}

	// Alice resolves it.
	aliceCtx := withCaller(ctx, auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	res, _ = s.handleProjectSet(aliceCtx, newCallToolReq("project_set", map[string]any{
		"repo_url": "git@github.com:owner/shared.git",
	}))
	body = decodeProjectSet(t, firstText(res))
	if body["status"] != "resolved" {
		t.Errorf("same-workspace status = %v, want resolved", body["status"])
	}
}

func TestProjectSet_MalformedInput(t *testing.T) {
	t.Parallel()
	s := newProjectSetTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})

	// Missing param.
	res, _ := s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{}))
	if !res.IsError {
		t.Errorf("missing repo_url not rejected; body=%s", firstText(res))
	}
	if !strings.Contains(firstText(res), "repo_url") {
		t.Errorf("rejection text = %q, want to mention repo_url", firstText(res))
	}

	// Empty.
	res, _ = s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{"repo_url": ""}))
	if !res.IsError {
		t.Errorf("empty repo_url not rejected; body=%s", firstText(res))
	}

	// Malformed.
	res, _ = s.handleProjectSet(ctx, newCallToolReq("project_set", map[string]any{"repo_url": "not a url"}))
	if !res.IsError {
		t.Errorf("malformed repo_url not rejected; body=%s", firstText(res))
	}
	if !strings.Contains(firstText(res), "invalid") {
		t.Errorf("rejection text = %q, want to mention invalid", firstText(res))
	}
}

func TestProjectSet_NoCallerIdentity(t *testing.T) {
	t.Parallel()
	s := newProjectSetTestServer(t)
	res, _ := s.handleProjectSet(context.Background(), newCallToolReq("project_set", map[string]any{
		"repo_url": "git@github.com:owner/repo.git",
	}))
	if !res.IsError {
		t.Errorf("missing caller not rejected; body=%s", firstText(res))
	}
}

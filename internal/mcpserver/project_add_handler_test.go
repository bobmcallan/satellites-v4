// Tests for project_add over the MCP transport (sty_690b06ee).
//
// Handler responsibilities under test: required name, optional
// repo_url binds the canonical remote in one shot, optional
// description persists, optional workspace_id overrides the caller's
// default. Layering remains intact (no internal/project import in
// these tests' production source path — see layering_test.go).
package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// newProjectWriteTestServer builds a Server with MemoryStore-backed
// dependencies sufficient to exercise project_add, project_update,
// project_delete (cascade), and project_set (archived resolver) over
// the MCP transport.
func newProjectWriteTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	led := ledger.NewMemoryStore()
	docs := document.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	projects := project.NewMemoryStore()
	wss := workspace.NewMemoryStore()
	sessions := session.NewMemoryStore()
	repos := repo.NewMemoryStore()
	tasks := task.NewMemoryStore()
	apikeys := auth.NewMemoryAgentAPIKeyStore()

	ws, err := wss.Create(context.Background(), "u_alice", "alpha", now)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if err := wss.AddMember(context.Background(), ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now); err != nil {
		t.Fatalf("workspace addmember: %v", err)
	}

	srv := New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:  docs,
			Projects:   projects,
			Ledger:     led,
			Stories:    stories,
			Workspaces: wss,
			Sessions:   sessions,
			Repos:      repos,
			Tasks:      tasks,
			APIKeys:    apikeys,
		},
	})
	return srv, ws.ID
}

// callProjectAdd is a sugar over newCallToolReq + handleProjectAdd for
// the project_add wire-shape tests below.
func callProjectAdd(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Email: "alice@local"})
	res, err := s.handleProjectAdd(ctx, newCallToolReq("project_add", args))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("project_add unexpected error: %s", firstText(res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(firstText(res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestHandleProjectAdd_RequiresName(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleProjectAdd(ctx, newCallToolReq("project_add", map[string]any{}))
	if !res.IsError {
		t.Fatalf("expected error when name missing, got: %s", firstText(res))
	}
}

func TestHandleProjectAdd_HappyPath(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	got := callProjectAdd(t, s, map[string]any{"name": "satellites"})
	if got["id"] == nil {
		t.Fatalf("response missing id: %v", got)
	}
	if got["status"] != "active" {
		t.Errorf("status = %v, want active", got["status"])
	}
}

func TestHandleProjectAdd_WithRepoURLBindsCanonical(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	got := callProjectAdd(t, s, map[string]any{
		"name":     "satellites",
		"repo_url": "git@github.com:bobmcallan/resumere.git",
	})
	rc, _ := got["repo_url_canonical"].(string)
	if rc == "" {
		t.Errorf("response missing repo_url_canonical: %v", got)
	}
}

func TestHandleProjectAdd_WithDescription(t *testing.T) {
	t.Parallel()
	s, _ := newProjectWriteTestServer(t)
	got := callProjectAdd(t, s, map[string]any{
		"name":        "satellites",
		"description": "a substrate test bench",
	})
	if got["description"] != "a substrate test bench" {
		t.Errorf("description = %v, want a substrate test bench", got["description"])
	}
}

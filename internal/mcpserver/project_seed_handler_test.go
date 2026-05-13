package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

const sampleProjectArtifactForSeed = `---
name: project_intent
tags: [kind:project-intent]
---
# Project intent placeholder

Body for the project-tier seed test.
`

// newProjectSeedFixture builds a Server with one project row and a
// fake seed dir carrying one project-scoped artifact under
// <dir>/<workspaceID>/<projectID>/artifacts/ (sty_87e203c1 layout).
func newProjectSeedFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Env: "dev"}
	docs := document.NewMemoryStore()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	projects := project.NewMemoryStore()
	ws := workspace.NewMemoryStore()
	sessions := session.NewMemoryStore()

	proj, err := projects.Create(context.Background(), "u_bob", "wksp_a", "satellites", now)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	server := New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:  docs,
			Projects:   projects,
			Ledger:     led,
			Stories:    stories,
			Workspaces: ws,
			Sessions:   sessions,
		},
	})

	dir := t.TempDir()
	subdir := filepath.Join(dir, proj.WorkspaceID, proj.ID, "artifacts")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "intent.md"), []byte(sampleProjectArtifactForSeed), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	t.Setenv(configseed.SeedDirEnv, dir)

	return server, dir, proj.ID
}

func TestProjectSeedRun_ForbiddenForNonAdmin(t *testing.T) {
	server, _, pid := newProjectSeedFixture(t)
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_alice", Email: "alice@x.io", Source: "session", GlobalAdmin: false,
	})
	res, err := server.handleProjectSeedRun(ctx, newCallToolReq("project_seed_run", map[string]any{
		"project_id": pid,
	}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError; got body=%s", firstText(res))
	}
	if body := firstText(res); !strings.Contains(body, "forbidden") {
		t.Errorf("expected forbidden error; got %q", body)
	}
}

func TestProjectSeedRun_RejectsEmptyProjectID(t *testing.T) {
	server, _, _ := newProjectSeedFixture(t)
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_bob", Email: "bob@x.io", Source: "session", GlobalAdmin: true,
	})
	res, err := server.handleProjectSeedRun(ctx, newCallToolReq("project_seed_run", map[string]any{}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError on empty project_id; body=%s", firstText(res))
	}
	if body := firstText(res); !strings.Contains(body, "project_id required") {
		t.Errorf("expected 'project_id required' error; got %q", body)
	}
}

func TestProjectSeedRun_AdminSucceedsAndWritesLedger(t *testing.T) {
	server, _, pid := newProjectSeedFixture(t)
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_bob", Email: "bob@x.io", Source: "session", GlobalAdmin: true,
	})
	res, err := server.handleProjectSeedRun(ctx, newCallToolReq("project_seed_run", map[string]any{
		"project_id": pid,
	}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %s", firstText(res))
	}
	var summary ProjectSeedRunResult
	if err := json.Unmarshal([]byte(firstText(res)), &summary); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if summary.ProjectID != pid {
		t.Errorf("ProjectID = %q, want %q", summary.ProjectID, pid)
	}
	if summary.Loaded != 1 || summary.Created != 1 {
		t.Errorf("loaded=%d created=%d, want 1/1", summary.Loaded, summary.Created)
	}
	rows, err := server.deps.Ledger.List(context.Background(), pid, ledger.ListOptions{
		Type: ledger.TypeDecision,
		Tags: []string{"kind:project-seed-run"},
	}, nil)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	if rows[0].ProjectID != pid {
		t.Errorf("ledger row project_id = %q, want %q", rows[0].ProjectID, pid)
	}
	if rows[0].CreatedBy != "u_bob" {
		t.Errorf("CreatedBy = %q, want %q", rows[0].CreatedBy, "u_bob")
	}

	// Sanity: the seeded artifact is project-scoped, attached to the
	// resolved project_id.
	doc, err := server.deps.Documents.GetByName(context.Background(), pid, "project_intent", nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if doc.Scope != document.ScopeProject {
		t.Errorf("scope = %s, want project", doc.Scope)
	}
	if doc.ProjectID == nil || *doc.ProjectID != pid {
		t.Errorf("project_id = %v, want %s", doc.ProjectID, pid)
	}
}

func TestProjectSeedRun_UnknownProject(t *testing.T) {
	server, _, _ := newProjectSeedFixture(t)
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_bob", Email: "bob@x.io", Source: "session", GlobalAdmin: true,
	})
	res, err := server.handleProjectSeedRun(ctx, newCallToolReq("project_seed_run", map[string]any{
		"project_id": "proj_doesnotexist",
	}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for unknown project; body=%s", firstText(res))
	}
	if body := firstText(res); !strings.Contains(body, "project not found") {
		t.Errorf("expected 'project not found' error; got %q", body)
	}
}

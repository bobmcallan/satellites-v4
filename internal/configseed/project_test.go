package configseed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

const sampleProjectArtifactMD = `---
name: project_intent
tags: [kind:project-intent]
---
# Satellites — project intent

Placeholder body for the project-tier seed test.
`

func writeProjectSeed(t *testing.T, root, workspaceID, projectID, kind, filename, content string) {
	t.Helper()
	dir := filepath.Join(root, workspaceID, projectID, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

// TestRunProject_ProducesProjectScopedRows is the AC1 anchor: files
// under <seedDir>/<workspace_id>/<project_id>/<kind>/*.md upsert as
// scope=project, project_id=<resolved>, type=<kind> documents — same
// parsers and frontmatter validation as the system loader.
func TestRunProject_ProducesProjectScopedRows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const ws = "wksp_a"
	pid := "proj_test01"
	writeProjectSeed(t, dir, ws, pid, "artifacts", "project_intent.md", sampleProjectArtifactMD)

	docs := document.NewMemoryStore()
	now := time.Now().UTC()
	summary, err := RunProject(context.Background(), docs, dir, pid, ws, "system", now)
	if err != nil {
		t.Fatalf("RunProject: %v", err)
	}
	if summary.Loaded != 1 || summary.Created != 1 {
		t.Errorf("summary = %+v, want loaded=1 created=1", summary)
	}
	doc, err := docs.GetByName(context.Background(), pid, "project_intent", nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if doc.Scope != document.ScopeProject {
		t.Errorf("scope = %s, want project", doc.Scope)
	}
	if doc.ProjectID == nil || *doc.ProjectID != pid {
		t.Errorf("project_id = %v, want %s", doc.ProjectID, pid)
	}
	if doc.Type != document.TypeArtifact {
		t.Errorf("type = %s, want artifact", doc.Type)
	}
}

// TestRunProject_IdempotentOnSameBody asserts a second pass with
// identical seed content produces zero new writes (Skipped++, no
// Updated bump).
func TestRunProject_IdempotentOnSameBody(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const ws = "wksp_a"
	pid := "proj_test02"
	writeProjectSeed(t, dir, ws, pid, "artifacts", "x.md", sampleProjectArtifactMD)

	docs := document.NewMemoryStore()
	now := time.Now().UTC()
	if _, err := RunProject(context.Background(), docs, dir, pid, ws, "system", now); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunProject(context.Background(), docs, dir, pid, ws, "system", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Created != 0 {
		t.Errorf("second pass created %d rows; want 0 (idempotent)", second.Created)
	}
	if second.Skipped == 0 {
		t.Errorf("second pass skipped = 0; expected the body-hash short-circuit to skip the file")
	}
}

// TestRunProject_MissingDirIsNotAnError covers the cold-boot case:
// a project whose seed directory simply doesn't exist returns an
// empty summary, not a structural error. Boot must stay non-fatal.
func TestRunProject_MissingDirIsNotAnError(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	dir := t.TempDir()
	summary, err := RunProject(context.Background(), docs, dir, "proj_doesnotexist", "wksp_a", "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("RunProject on missing dir: %v", err)
	}
	if summary.Loaded != 0 {
		t.Errorf("Loaded = %d, want 0", summary.Loaded)
	}
	if len(summary.Errors) != 0 {
		t.Errorf("errors = %v, want none", summary.Errors)
	}
}

// TestRunProject_RejectsEmptyProjectID guards the boot path against
// being called with no project anchor.
func TestRunProject_RejectsEmptyProjectID(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	if _, err := RunProject(context.Background(), docs, t.TempDir(), "", "wksp_a", "system", time.Now().UTC()); err == nil {
		t.Errorf("expected error when project_id is empty")
	}
}

// TestDiscoverProjectDirs_FindsWkspProjPairs asserts the discovery
// helper walks <seedDir>/wksp_*/proj_*/ and returns the pairs sorted
// by workspace then project, ignoring system kind dirs and non-prefix
// entries at either level.
func TestDiscoverProjectDirs_FindsWkspProjPairs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Top-level decoys + two real workspaces.
	for _, sub := range []string{"system", "agents", "contracts", "principles", "tools"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	// wksp_b first on disk, but discovery sorts.
	for _, p := range []string{
		"wksp_b/proj_bbb22222",
		"wksp_a/proj_aaa11111",
		"wksp_a/proj_aaa22222",
		"wksp_a/not_a_project", // non-prefix child, ignored
	} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	out, err := DiscoverProjectDirs(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d pairs, want 3: %v", len(out), out)
	}
	want := []DiscoveredProject{
		{WorkspaceID: "wksp_a", ProjectID: "proj_aaa11111"},
		{WorkspaceID: "wksp_a", ProjectID: "proj_aaa22222"},
		{WorkspaceID: "wksp_b", ProjectID: "proj_bbb22222"},
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("pair[%d] = %+v, want %+v", i, out[i], w)
		}
	}
}

// TestDiscoverProjectDirs_EmptyWorkspaceDir asserts that a wksp_* dir
// with no proj_* children contributes zero pairs and is not an error.
func TestDiscoverProjectDirs_EmptyWorkspaceDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wksp_empty"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out, err := DiscoverProjectDirs(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d pairs from empty workspace dir, want 0: %v", len(out), out)
	}
}

func TestDiscoverProjectDirs_MissingSeedDir(t *testing.T) {
	t.Parallel()
	out, err := DiscoverProjectDirs(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("err on missing seed dir: %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}

// TestSystemRunNeverTouchesProjectScope is the AC6 anchor (system
// side). configseed.Run produces only scope=system documents; even
// if a project subdirectory exists alongside the system kind dirs,
// the system loader does not walk it. Sty_8868eaf4.
func TestSystemRunNeverTouchesProjectScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// One system contract under <dir>/system/contracts/.
	if err := os.MkdirAll(filepath.Join(dir, SystemSubdir, "contracts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SystemSubdir, "contracts", "sample.md"), []byte(sampleContractMD), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A workspace/project subtree with its own contract — the system
	// loader must skip this entirely.
	pid := "proj_isolated"
	writeProjectSeed(t, dir, "wksp_a", pid, "contracts", "project_only.md", sampleContractMD)

	docs := document.NewMemoryStore()
	if _, err := Run(context.Background(), docs, dir, "wksp_a", "system", time.Now().UTC()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, err := docs.List(context.Background(), document.ListOptions{
		Type:  document.TypeContract,
		Scope: document.ScopeProject,
	}, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("system Run produced %d project-scoped contract rows; want 0", len(rows))
	}
}

// TestProjectRunNeverTouchesSystemScope is the AC6 anchor (project
// side). Even with a system kind dir present, RunProject only walks
// <seedDir>/<workspaceID>/<projectID>/ and produces only scope=project
// rows.
func TestProjectRunNeverTouchesSystemScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Sibling system contract — should not be touched.
	if err := os.MkdirAll(filepath.Join(dir, SystemSubdir, "contracts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SystemSubdir, "contracts", "system_only.md"), []byte(sampleContractMD), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	const ws = "wksp_a"
	pid := "proj_isolated2"
	writeProjectSeed(t, dir, ws, pid, "artifacts", "x.md", sampleProjectArtifactMD)

	docs := document.NewMemoryStore()
	if _, err := RunProject(context.Background(), docs, dir, pid, ws, "system", time.Now().UTC()); err != nil {
		t.Fatalf("RunProject: %v", err)
	}
	systemRows, err := docs.List(context.Background(), document.ListOptions{Scope: document.ScopeSystem}, nil)
	if err != nil {
		t.Fatalf("List system: %v", err)
	}
	if len(systemRows) != 0 {
		t.Errorf("project run produced %d system-scoped rows; want 0", len(systemRows))
	}
	projectRows, err := docs.List(context.Background(), document.ListOptions{Scope: document.ScopeProject}, nil)
	if err != nil {
		t.Fatalf("List project: %v", err)
	}
	if len(projectRows) != 1 {
		t.Errorf("project run produced %d project-scoped rows; want 1", len(projectRows))
	}
}

// TestRunProject_RealSeedDirSatellitesPlaceholder is the end-to-end
// AC10 anchor: RunProject against the real config/seed directory
// loads the placeholder artifact for proj_7a62aedb (the satellites
// project) under its canonical workspace and the row comes back with
// scope=project, project_id set.
func TestRunProject_RealSeedDirSatellitesPlaceholder(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const (
		ws  = "wksp_5b3257d1"
		pid = "proj_7a62aedb"
	)
	if _, statErr := os.Stat(filepath.Join(seedDir, ws, pid)); statErr != nil {
		t.Fatalf("expected real seed dir for %s/%s: %v", ws, pid, statErr)
	}
	docs := document.NewMemoryStore()
	summary, err := RunProject(context.Background(), docs, seedDir, pid, ws, "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("RunProject: %v", err)
	}
	if summary.Loaded == 0 {
		t.Fatalf("Loaded = 0; expected at least the project_intent placeholder")
	}
	doc, err := docs.GetByName(context.Background(), pid, "project_intent", nil)
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

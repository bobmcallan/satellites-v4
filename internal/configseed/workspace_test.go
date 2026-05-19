package configseed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestRunWorkspace_StampsScopeAndWorkspaceID is the AC2 anchor:
// RunWorkspace walks <seedDir>/<workspaceID>/<kind>/*.md and produces
// scope=workspace UpsertInputs stamped with the supplied workspace_id
// and a nil project_id.
func TestRunWorkspace_StampsScopeAndWorkspaceID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const wsID = "wksp_runws"

	if err := os.MkdirAll(filepath.Join(dir, wsID, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, wsID, "contracts"), 0o755); err != nil {
		t.Fatalf("mkdir contracts: %v", err)
	}

	const agentMD = `---
name: shared_dev_agent
tags: []
---
agent body for workspace tier
`
	const contractMD = `---
name: shared_develop
category: develop
review_required: true
tags: []
---
contract body for workspace tier
`
	if err := os.WriteFile(filepath.Join(dir, wsID, "agents", "developer.md"), []byte(agentMD), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsID, "contracts", "develop.md"), []byte(contractMD), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	summary, err := RunWorkspace(ctx, docs, dir, wsID, "system", now)
	if err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}
	if summary.Loaded != 2 {
		t.Errorf("Loaded = %d, want 2", summary.Loaded)
	}
	if summary.Created != 2 {
		t.Errorf("Created = %d, want 2", summary.Created)
	}
	if len(summary.Errors) != 0 {
		t.Errorf("Errors = %v, want none", summary.Errors)
	}

	rows, err := docs.List(ctx, document.ListOptions{Scope: document.ScopeWorkspace}, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("scope=workspace rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.WorkspaceID != wsID {
			t.Errorf("%s WorkspaceID = %q, want %q", r.Name, r.WorkspaceID, wsID)
		}
		if r.ProjectID != nil {
			t.Errorf("%s ProjectID = %v, want nil", r.Name, r.ProjectID)
		}
		if r.Scope != document.ScopeWorkspace {
			t.Errorf("%s Scope = %q, want workspace", r.Name, r.Scope)
		}
	}
}

// TestRunWorkspace_LoadsLifecycleContractAtWorkspaceScope is the
// sty_9ee6fc46 anchor: a workspace seed dir containing
// contracts/develop.md must produce a name=develop document at
// scope=workspace, stamped with the supplied workspace_id and a
// nil project_id. Locks the relocation of develop / push /
// merge_to_main from the system tier to the workspace tier.
func TestRunWorkspace_LoadsLifecycleContractAtWorkspaceScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const wsID = "wksp_lifecycle"

	if err := os.MkdirAll(filepath.Join(dir, wsID, "contracts"), 0o755); err != nil {
		t.Fatalf("mkdir contracts: %v", err)
	}
	const developMD = `---
name: develop
category: develop
validation_mode: llm
required_role: role_orchestrator
review_required: true
tags: [v4, lifecycle, workspace]
---
develop body
`
	if err := os.WriteFile(filepath.Join(dir, wsID, "contracts", "develop.md"), []byte(developMD), 0o644); err != nil {
		t.Fatalf("write develop contract: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	if _, err := RunWorkspace(ctx, docs, dir, wsID, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}

	rows, err := docs.List(ctx, document.ListOptions{Scope: document.ScopeWorkspace, Type: document.TypeContract}, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("workspace contract rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Name != "develop" {
		t.Errorf("Name = %q, want %q", got.Name, "develop")
	}
	if got.Scope != document.ScopeWorkspace {
		t.Errorf("Scope = %q, want workspace", got.Scope)
	}
	if got.WorkspaceID != wsID {
		t.Errorf("WorkspaceID = %q, want %q", got.WorkspaceID, wsID)
	}
	if got.ProjectID != nil {
		t.Errorf("ProjectID = %v, want nil", got.ProjectID)
	}
}

// TestRunWorkspace_SkipsProjectSubdir asserts a proj_*/ entry directly
// under the workspace dir is NOT walked by RunWorkspace — those are
// the project-tier loader's responsibility.
func TestRunWorkspace_SkipsProjectSubdir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const wsID = "wksp_skipproj"

	// Workspace-tier file (should be loaded).
	if err := os.MkdirAll(filepath.Join(dir, wsID, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir ws/agents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsID, "agents", "ws.md"), []byte(`---
name: workspace_agent
tags: []
---
ws body`), 0o644); err != nil {
		t.Fatalf("write ws agent: %v", err)
	}

	// Project-tier file under proj_x/ (must be ignored by RunWorkspace).
	if err := os.MkdirAll(filepath.Join(dir, wsID, "proj_x", "agents"), 0o755); err != nil {
		t.Fatalf("mkdir proj/agents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsID, "proj_x", "agents", "proj.md"), []byte(`---
name: project_agent
tags: []
---
proj body`), 0o644); err != nil {
		t.Fatalf("write proj agent: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	summary, err := RunWorkspace(ctx, docs, dir, wsID, "system", now)
	if err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}
	if summary.Loaded != 1 {
		t.Errorf("Loaded = %d, want 1 (project_agent must be skipped)", summary.Loaded)
	}

	rows, _ := docs.List(ctx, document.ListOptions{Scope: document.ScopeWorkspace}, nil)
	if len(rows) != 1 || rows[0].Name != "workspace_agent" {
		t.Errorf("rows = %+v, want only workspace_agent", rows)
	}
}

// TestRunWorkspace_Idempotent — second pass with the same body hash
// records every row as Skipped.
func TestRunWorkspace_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const wsID = "wksp_idem"

	if err := os.MkdirAll(filepath.Join(dir, wsID, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsID, "agents", "a.md"), []byte(`---
name: stable_agent
tags: []
---
body`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	if _, err := RunWorkspace(ctx, docs, dir, wsID, "system", now); err != nil {
		t.Fatalf("first RunWorkspace: %v", err)
	}
	second, err := RunWorkspace(ctx, docs, dir, wsID, "system", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second RunWorkspace: %v", err)
	}
	if second.Created != 0 || second.Updated != 0 || second.Skipped != 1 {
		t.Errorf("second pass counts = %+v, want Created=0 Updated=0 Skipped=1", second)
	}
}

// TestRunWorkspace_MissingDirNoError covers the cold-boot path: an
// empty seed root returns Summary{} with no error.
func TestRunWorkspace_MissingDirNoError(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	summary, err := RunWorkspace(context.Background(), docs, t.TempDir(), "wksp_absent", "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("RunWorkspace missing dir: %v", err)
	}
	if summary.Loaded != 0 {
		t.Errorf("Loaded = %d, want 0", summary.Loaded)
	}
}

// TestRunWorkspace_RejectsEmptyWorkspaceID guards against the boot
// path passing a zero-value workspace id.
func TestRunWorkspace_RejectsEmptyWorkspaceID(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	if _, err := RunWorkspace(context.Background(), docs, t.TempDir(), "", "system", time.Now().UTC()); err == nil {
		t.Errorf("empty workspace_id accepted; want rejection")
	}
}

// TestDiscoverWorkspaceDirs returns wksp_*/ entries directly under
// seedDir.
func TestDiscoverWorkspaceDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range []string{"wksp_a", "wksp_b", "system", "not_a_workspace"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	got, err := DiscoverWorkspaceDirs(dir)
	if err != nil {
		t.Fatalf("DiscoverWorkspaceDirs: %v", err)
	}
	want := []string{"wksp_a", "wksp_b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

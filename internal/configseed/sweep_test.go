package configseed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/document"
)

const sampleSweepPrincipleMD = `---
id: pr_x
name: Sample principle
scope: system
tags: []
---
Body for the sweep test.
`

// TestSweepOrphanedSystemPrinciples_ArchivesUnmatched seeds two
// scope=system principle rows directly into the store, leaves only
// one matching seed file on disk, and asserts the sweep archives the
// other.
func TestSweepOrphanedSystemPrinciples_ArchivesUnmatched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, SystemSubdir, "principles"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SystemSubdir, "principles", "kept.md"), []byte(sampleSweepPrincipleMD), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	if _, err := docs.Upsert(ctx, document.UpsertInput{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Name:  "Sample principle",
		Body:  []byte("kept body"),
		Actor: "system",
	}, now); err != nil {
		t.Fatalf("seed kept: %v", err)
	}

	orphan, err := docs.Upsert(ctx, document.UpsertInput{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Name:  "Orphan principle",
		Body:  []byte("orphan body"),
		Actor: "system",
	}, now)
	if err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	logger := arbor.New("warn")
	archived, err := SweepOrphanedSystemPrinciples(ctx, docs, dir, logger, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if archived != 1 {
		t.Errorf("archived = %d, want 1", archived)
	}

	got, err := docs.GetByID(ctx, orphan.Document.ID, nil)
	if err != nil {
		t.Fatalf("GetByID orphan: %v", err)
	}
	if got.Status != document.StatusArchived {
		t.Errorf("orphan status = %q, want archived", got.Status)
	}

	rows, err := docs.List(ctx, document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
	}, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	active := 0
	for _, r := range rows {
		if r.Status == document.StatusActive {
			active++
		}
	}
	if active != 1 {
		t.Errorf("active system principles = %d, want 1", active)
	}
}

// TestSweepOrphanedProjectDocs_ArchivesUnmatched (sty_94c54229) seeds
// two scope=project principle rows under one (workspace, project) pair,
// leaves only one matching seed file on disk, and asserts the project-
// tier sweep archives the other. Mirrors the system-tier coverage at
// the project tier — the rename in this story relies on the project
// sweep firing on every project-seed pass.
func TestSweepOrphanedProjectDocs_ArchivesUnmatched(t *testing.T) {
	t.Parallel()
	const ws = "wksp_test"
	const proj = "proj_test"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ws, proj, "principles"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const keptPrinciple = `---
id: kept
name: kept
scope: project
tags: []
---
Body.
`
	if err := os.WriteFile(filepath.Join(dir, ws, proj, "principles", "kept.md"), []byte(keptPrinciple), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)

	pid := proj
	if _, err := docs.Upsert(ctx, document.UpsertInput{
		WorkspaceID: ws,
		ProjectID:   &pid,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeProject,
		Name:        "kept",
		Body:        []byte("kept body"),
		Actor:       "system",
	}, now); err != nil {
		t.Fatalf("seed kept: %v", err)
	}

	orphan, err := docs.Upsert(ctx, document.UpsertInput{
		WorkspaceID: ws,
		ProjectID:   &pid,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeProject,
		Name:        "pr_dropped",
		Body:        []byte("orphan body"),
		Actor:       "system",
	}, now)
	if err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	logger := arbor.New("warn")
	archived, err := SweepOrphanedProjectDocs(ctx, docs, dir, ws, proj, logger, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if archived != 1 {
		t.Errorf("archived = %d, want 1", archived)
	}

	got, err := docs.GetByID(ctx, orphan.Document.ID, nil)
	if err != nil {
		t.Fatalf("GetByID orphan: %v", err)
	}
	if got.Status != document.StatusArchived {
		t.Errorf("orphan status = %q, want archived", got.Status)
	}

	// Second pass is a no-op.
	again, err := SweepOrphanedProjectDocs(ctx, docs, dir, ws, proj, logger, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("sweep idempotent: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep archived = %d, want 0", again)
	}
}

// TestSweepOrphanedSystemPrinciples_Idempotent asserts a second pass
// against the post-sweep state archives zero additional rows.
func TestSweepOrphanedSystemPrinciples_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	if _, err := docs.Upsert(ctx, document.UpsertInput{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Name:  "Stale principle",
		Body:  []byte("body"),
		Actor: "system",
	}, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	logger := arbor.New("warn")
	if archived, err := SweepOrphanedSystemPrinciples(ctx, docs, dir, logger, now); err != nil || archived != 1 {
		t.Fatalf("first sweep: archived=%d err=%v", archived, err)
	}
	archived, err := SweepOrphanedSystemPrinciples(ctx, docs, dir, logger, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if archived != 0 {
		t.Errorf("second sweep archived = %d, want 0 (idempotent)", archived)
	}
}

// TestSweepOrphanedSystemDocs_GenericKindCoverage exercises the
// generalised sweep across multiple kinds (sty_92271886). One orphan
// per kind, plus one matching seed file per kind, plus one row of an
// unrelated tier (scope=workspace) that must NOT be archived.
func TestSweepOrphanedSystemDocs_GenericKindCoverage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, SystemSubdir, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, SystemSubdir, "contracts"), 0o755); err != nil {
		t.Fatalf("mkdir contracts: %v", err)
	}

	const agentMD = `---
name: kept_agent
tags: []
---
agent body
`
	const contractMD = `---
name: kept_contract
tags: []
---
contract body
`
	if err := os.WriteFile(filepath.Join(dir, SystemSubdir, "agents", "kept.md"), []byte(agentMD), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SystemSubdir, "contracts", "kept.md"), []byte(contractMD), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	// kept rows (match a seed file)
	if _, err := docs.Upsert(ctx, document.UpsertInput{Type: document.TypeAgent, Scope: document.ScopeSystem, Name: "kept_agent", Body: []byte("a"), Actor: "system"}, now); err != nil {
		t.Fatalf("seed kept_agent: %v", err)
	}
	if _, err := docs.Upsert(ctx, document.UpsertInput{Type: document.TypeContract, Scope: document.ScopeSystem, Name: "kept_contract", Body: []byte("c"), Actor: "system"}, now); err != nil {
		t.Fatalf("seed kept_contract: %v", err)
	}

	// orphan rows (no matching seed file)
	orphanAgent, err := docs.Upsert(ctx, document.UpsertInput{Type: document.TypeAgent, Scope: document.ScopeSystem, Name: "orphan_agent", Body: []byte("a"), Actor: "system"}, now)
	if err != nil {
		t.Fatalf("seed orphan_agent: %v", err)
	}
	orphanContract, err := docs.Upsert(ctx, document.UpsertInput{Type: document.TypeContract, Scope: document.ScopeSystem, Name: "orphan_contract", Body: []byte("c"), Actor: "system"}, now)
	if err != nil {
		t.Fatalf("seed orphan_contract: %v", err)
	}

	// scope=workspace row of the same type — must NOT be archived; the
	// sweep targets scope=system only.
	wsAgent, err := docs.Upsert(ctx, document.UpsertInput{Type: document.TypeAgent, Scope: document.ScopeWorkspace, Name: "ws_agent", WorkspaceID: "wksp_unrelated", Body: []byte("a"), Actor: "system"}, now)
	if err != nil {
		t.Fatalf("seed ws_agent: %v", err)
	}

	logger := arbor.New("warn")
	archived, err := SweepOrphanedSystemDocs(ctx, docs, dir, logger, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if archived != 2 {
		t.Errorf("archived = %d, want 2 (one orphan per kind)", archived)
	}

	for _, p := range []struct {
		name string
		id   string
		want string
	}{
		{"orphan_agent", orphanAgent.Document.ID, document.StatusArchived},
		{"orphan_contract", orphanContract.Document.ID, document.StatusArchived},
		{"ws_agent_untouched", wsAgent.Document.ID, document.StatusActive},
	} {
		got, err := docs.GetByID(ctx, p.id, nil)
		if err != nil {
			t.Fatalf("get %s: %v", p.name, err)
		}
		if got.Status != p.want {
			t.Errorf("%s status = %q, want %q", p.name, got.Status, p.want)
		}
	}

	// idempotent: second pass archives 0.
	again, err := SweepOrphanedSystemDocs(ctx, docs, dir, logger, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep archived = %d, want 0 (idempotent)", again)
	}
}

// TestSweepOrphanedSystemPrinciples_MissingDirNoError covers the
// post-migration shape: no system/principles/ dir at all. Sweep
// archives every active scope=system principle row.
func TestSweepOrphanedSystemPrinciples_MissingDirNoError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	for i, name := range []string{"a", "b", "c"} {
		_ = i
		if _, err := docs.Upsert(ctx, document.UpsertInput{
			Type:  document.TypePrinciple,
			Scope: document.ScopeSystem,
			Name:  name,
			Body:  []byte("body"),
			Actor: "system",
		}, now); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	logger := arbor.New("warn")
	archived, err := SweepOrphanedSystemPrinciples(ctx, docs, dir, logger, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if archived != 3 {
		t.Errorf("archived = %d, want 3", archived)
	}
}

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
		WorkspaceID: "wksp_a",
		Type:        document.TypePrinciple,
		Scope:       document.ScopeSystem,
		Name:        "Sample principle",
		Body:        []byte("kept body"),
		Actor:       "system",
	}, now); err != nil {
		t.Fatalf("seed kept: %v", err)
	}

	orphan, err := docs.Upsert(ctx, document.UpsertInput{
		WorkspaceID: "wksp_b",
		Type:        document.TypePrinciple,
		Scope:       document.ScopeSystem,
		Name:        "Orphan principle",
		Body:        []byte("orphan body"),
		Actor:       "system",
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

// TestSweepOrphanedSystemPrinciples_Idempotent asserts a second pass
// against the post-sweep state archives zero additional rows.
func TestSweepOrphanedSystemPrinciples_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	if _, err := docs.Upsert(ctx, document.UpsertInput{
		WorkspaceID: "wksp_a",
		Type:        document.TypePrinciple,
		Scope:       document.ScopeSystem,
		Name:        "Stale principle",
		Body:        []byte("body"),
		Actor:       "system",
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
			WorkspaceID: "wksp_a",
			Type:        document.TypePrinciple,
			Scope:       document.ScopeSystem,
			Name:        name,
			Body:        []byte("body"),
			Actor:       "system",
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

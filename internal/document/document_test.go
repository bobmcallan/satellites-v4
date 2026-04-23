package document

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
)

func TestHashBody_Stable(t *testing.T) {
	t.Parallel()
	a := HashBody([]byte("hello"))
	b := HashBody([]byte("hello"))
	if a != b {
		t.Errorf("hash not stable: %q vs %q", a, b)
	}
	c := HashBody([]byte("world"))
	if a == c {
		t.Errorf("distinct bodies must hash differently")
	}
}

func TestMemoryStore_UpsertIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	first, err := store.Upsert(ctx, "x.md", "architecture", []byte("body"), now)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !first.Changed {
		t.Errorf("first upsert must be Created+Changed: %+v", first)
	}
	if first.Document.Version != 1 {
		t.Errorf("version = %d, want 1", first.Document.Version)
	}

	// Same body → no-op.
	second, _ := store.Upsert(ctx, "x.md", "architecture", []byte("body"), now.Add(time.Hour))
	if second.Created || second.Changed {
		t.Errorf("unchanged upsert must be !Created+!Changed: %+v", second)
	}
	if second.Document.Version != 1 {
		t.Errorf("version = %d, want 1 (unchanged)", second.Document.Version)
	}
	if second.Document.ID != first.Document.ID {
		t.Errorf("unchanged upsert minted a new id: %q → %q", first.Document.ID, second.Document.ID)
	}

	// Changed body → version++.
	third, _ := store.Upsert(ctx, "x.md", "architecture", []byte("body2"), now.Add(2*time.Hour))
	if third.Created || !third.Changed {
		t.Errorf("changed upsert must be !Created+Changed: %+v", third)
	}
	if third.Document.Version != 2 {
		t.Errorf("version = %d, want 2", third.Document.Version)
	}
	if third.Document.ID != first.Document.ID {
		t.Errorf("changed upsert minted a new id: %q → %q", first.Document.ID, third.Document.ID)
	}
}

func TestIngestFile_PathTraversalBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	store := NewMemoryStore()
	logger := satarbor.New("info")

	for _, bad := range []string{
		"../etc/passwd",
		"../../secret",
		"/etc/passwd",
		"./../outside.md",
	} {
		if _, err := IngestFile(ctx, store, logger, dir, bad, time.Now()); err == nil {
			t.Errorf("expected traversal error for %q", bad)
		}
	}
}

func TestIngestFile_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	logger := satarbor.New("info")

	res, err := IngestFile(ctx, store, logger, dir, "architecture.md", time.Now())
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	if !res.Created {
		t.Errorf("first ingest must be Created")
	}
	got, err := store.GetByFilename(ctx, "architecture.md")
	if err != nil {
		t.Fatalf("GetByFilename: %v", err)
	}
	if got.Body != "# arch\n" {
		t.Errorf("body = %q, want \"# arch\\n\"", got.Body)
	}
	if got.Type != "architecture" {
		t.Errorf("type = %q, want architecture", got.Type)
	}
}

func TestSeed_SkipsWhenPopulated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	logger := satarbor.New("info")

	// Pre-populate with a different doc so Count > 0.
	_, _ = store.Upsert(ctx, "already.md", "architecture", []byte("x"), time.Now())

	n, err := Seed(ctx, store, logger, dir, []string{"architecture.md"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("seed ingested %d; expected 0 when store pre-populated", n)
	}
}

func TestSeed_IngestsWhenEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ui-design.md"), []byte("# design\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewMemoryStore()
	logger := satarbor.New("info")
	n, err := Seed(ctx, store, logger, dir, []string{"architecture.md", "ui-design.md"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("seed ingested %d; expected 2", n)
	}
}

func TestInferType(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"architecture.md": "architecture",
		"ui-design.md":    "design",
		"development.md":  "development",
		"random.md":       "document",
	}
	for f, want := range tests {
		if got := InferType(f); got != want {
			t.Errorf("InferType(%q) = %q, want %q", f, got, want)
		}
	}
}

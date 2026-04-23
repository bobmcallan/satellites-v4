package document

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
)

const testProjectID = "proj_test"

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

	first, err := store.Upsert(ctx, "", testProjectID, "x.md", "architecture", []byte("body"), now)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !first.Changed {
		t.Errorf("first upsert must be Created+Changed: %+v", first)
	}
	if first.Document.Version != 1 {
		t.Errorf("version = %d, want 1", first.Document.Version)
	}
	if first.Document.ProjectID != testProjectID {
		t.Errorf("project_id = %q, want %q", first.Document.ProjectID, testProjectID)
	}

	// Same body → no-op.
	second, _ := store.Upsert(ctx, "", testProjectID, "x.md", "architecture", []byte("body"), now.Add(time.Hour))
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
	third, _ := store.Upsert(ctx, "", testProjectID, "x.md", "architecture", []byte("body2"), now.Add(2*time.Hour))
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

func TestMemoryStore_ProjectIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	if _, err := store.Upsert(ctx, "", "proj_a", "x.md", "t", []byte("A"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(ctx, "", "proj_b", "x.md", "t", []byte("B"), now); err != nil {
		t.Fatal(err)
	}

	a, err := store.GetByFilename(ctx, "proj_a", "x.md")
	if err != nil {
		t.Fatalf("GetByFilename proj_a: %v", err)
	}
	if a.Body != "A" {
		t.Errorf("proj_a body = %q, want A", a.Body)
	}

	b, err := store.GetByFilename(ctx, "proj_b", "x.md")
	if err != nil {
		t.Fatalf("GetByFilename proj_b: %v", err)
	}
	if b.Body != "B" {
		t.Errorf("proj_b body = %q, want B", b.Body)
	}

	if a.ID == b.ID {
		t.Errorf("distinct projects should mint distinct document ids")
	}

	if nA, _ := store.Count(ctx, "proj_a"); nA != 1 {
		t.Errorf("Count(proj_a) = %d, want 1", nA)
	}
	if nB, _ := store.Count(ctx, "proj_b"); nB != 1 {
		t.Errorf("Count(proj_b) = %d, want 1", nB)
	}
	if nMissing, _ := store.Count(ctx, "proj_unknown"); nMissing != 0 {
		t.Errorf("Count(proj_unknown) = %d, want 0", nMissing)
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
		if _, err := IngestFile(ctx, store, logger, "", testProjectID, dir, bad, time.Now()); err == nil {
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

	res, err := IngestFile(ctx, store, logger, "", testProjectID, dir, "architecture.md", time.Now())
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	if !res.Created {
		t.Errorf("first ingest must be Created")
	}
	if res.Document.ProjectID != testProjectID {
		t.Errorf("ingested doc project_id = %q, want %q", res.Document.ProjectID, testProjectID)
	}
	got, err := store.GetByFilename(ctx, testProjectID, "architecture.md")
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

func TestSeed_SkipsWhenProjectPopulated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	logger := satarbor.New("info")

	// Pre-populate a different doc in the target project so Count > 0.
	_, _ = store.Upsert(ctx, "", testProjectID, "already.md", "architecture", []byte("x"), time.Now())

	n, err := Seed(ctx, store, logger, "", testProjectID, dir, []string{"architecture.md"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("seed ingested %d; expected 0 when project pre-populated", n)
	}
}

func TestSeed_IngestsWhenProjectEmpty(t *testing.T) {
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
	n, err := Seed(ctx, store, logger, "", testProjectID, dir, []string{"architecture.md", "ui-design.md"})
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

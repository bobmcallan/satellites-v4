package document

import (
	"context"
	"errors"
	"testing"
	"time"
)

func versionsFixture(t *testing.T) (*MemoryStore, Document, time.Time) {
	t.Helper()
	store := NewMemoryStore()
	now := time.Now().UTC()
	res, err := store.Upsert(context.Background(), UpsertInput{
		ProjectID: StringPtr(testProjectID),
		Type:      TypeArtifact,
		Name:      "doc-v",
		Body:      []byte("v1"),
		Scope:     ScopeProject,
		Actor:     "alice",
	}, now)
	if err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	return store, res.Document, now
}

func TestMemoryStore_AppendVersionOnUpdate(t *testing.T) {
	t.Parallel()
	store, doc, now := versionsFixture(t)
	body := "v2"
	updated, err := store.Update(context.Background(), doc.ID, UpdateFields{Body: &body}, "bob", now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("live version = %d, want 2", updated.Version)
	}
	versions, err := store.ListVersions(context.Background(), doc.ID, nil)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1 prior version", len(versions))
	}
	if versions[0].Version != 1 || versions[0].Body != "v1" {
		t.Errorf("prior version = %+v, want {Version:1, Body:v1}", versions[0])
	}
	if versions[0].UpdatedBy != "alice" {
		t.Errorf("prior updated_by = %q, want alice (the v1 author)", versions[0].UpdatedBy)
	}
}

func TestMemoryStore_AppendVersionOnUpsert(t *testing.T) {
	t.Parallel()
	store, doc, now := versionsFixture(t)
	_, err := store.Upsert(context.Background(), UpsertInput{
		ProjectID: doc.ProjectID,
		Type:      TypeArtifact,
		Name:      doc.Name,
		Body:      []byte("v2-via-upsert"),
		Scope:     ScopeProject,
		Actor:     "bob",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	versions, err := store.ListVersions(context.Background(), doc.ID, nil)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 || versions[0].Body != "v1" {
		t.Errorf("versions = %+v, want one prior with Body=v1", versions)
	}
}

func TestMemoryStore_DedupOnIdenticalBody_Update(t *testing.T) {
	t.Parallel()
	store, doc, now := versionsFixture(t)
	sameBody := "v1"
	updated, err := store.Update(context.Background(), doc.ID, UpdateFields{Body: &sameBody}, "bob", now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Version != 1 {
		t.Errorf("version bumped to %d on identical body; want 1 (dedup)", updated.Version)
	}
	versions, _ := store.ListVersions(context.Background(), doc.ID, nil)
	if len(versions) != 0 {
		t.Errorf("versions = %d, want 0 (no prior captured on identical body)", len(versions))
	}
}

func TestMemoryStore_DedupOnIdenticalBody_Upsert(t *testing.T) {
	t.Parallel()
	store, doc, now := versionsFixture(t)
	_, err := store.Upsert(context.Background(), UpsertInput{
		ProjectID: doc.ProjectID,
		Type:      TypeArtifact,
		Name:      doc.Name,
		Body:      []byte("v1"),
		Scope:     ScopeProject,
		Actor:     "bob",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	versions, _ := store.ListVersions(context.Background(), doc.ID, nil)
	if len(versions) != 0 {
		t.Errorf("versions = %d, want 0 (no prior on identical body)", len(versions))
	}
}

func TestMemoryStore_ListVersionsOrderingDESC(t *testing.T) {
	t.Parallel()
	store, doc, now := versionsFixture(t)
	for i, body := range []string{"v2", "v3", "v4"} {
		b := body
		_, err := store.Update(context.Background(), doc.ID, UpdateFields{Body: &b}, "bob", now.Add(time.Duration(i+1)*time.Minute), nil)
		if err != nil {
			t.Fatalf("Update %s: %v", body, err)
		}
	}
	versions, err := store.ListVersions(context.Background(), doc.ID, nil)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3 prior versions", len(versions))
	}
	wantBodies := []string{"v3", "v2", "v1"} // DESC: latest prior first
	for i, want := range wantBodies {
		if versions[i].Body != want {
			t.Errorf("versions[%d].Body = %q, want %q (DESC order)", i, versions[i].Body, want)
		}
	}
}

func TestMemoryStore_ListVersions_MembershipScoping(t *testing.T) {
	t.Parallel()
	store, doc, now := versionsFixture(t)
	body := "v2"
	if _, err := store.Update(context.Background(), doc.ID, UpdateFields{Body: &body}, "bob", now.Add(time.Minute), nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, err := store.ListVersions(context.Background(), doc.ID, []string{"wrong-workspace"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ListVersions with wrong membership = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_ListVersions_UnknownDocument(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	_, err := store.ListVersions(context.Background(), "doc_missing", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ListVersions(missing) = %v, want ErrNotFound", err)
	}
}

package repo

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newCommitsFixture(t *testing.T) (*MemoryStore, Repo) {
	t.Helper()
	store := NewMemoryStore()
	r, err := store.Create(context.Background(), Repo{
		WorkspaceID:   "ws_1",
		ProjectID:     "proj_a",
		GitRemote:     "git@x:y.git",
		DefaultBranch: "main",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return store, r
}

func TestMemoryStore_UpsertCommit_Roundtrip(t *testing.T) {
	t.Parallel()
	store, r := newCommitsFixture(t)
	c := Commit{
		RepoID:      r.ID,
		SHA:         "deadbeef",
		Subject:     "feat: thing",
		Author:      "Alice",
		CommittedAt: time.Now().UTC(),
		StoryIDs:    []string{"story_abcd1234"},
	}
	if _, err := store.UpsertCommit(context.Background(), c); err != nil {
		t.Fatalf("UpsertCommit: %v", err)
	}
	got, err := store.GetCommit(context.Background(), r.ID, "deadbeef", nil)
	if err != nil {
		t.Fatalf("GetCommit: %v", err)
	}
	if got.Subject != "feat: thing" || got.Author != "Alice" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if len(got.StoryIDs) != 1 || got.StoryIDs[0] != "story_abcd1234" {
		t.Errorf("StoryIDs = %v", got.StoryIDs)
	}
}

func TestMemoryStore_UpsertCommit_Idempotent(t *testing.T) {
	t.Parallel()
	store, r := newCommitsFixture(t)
	c := Commit{RepoID: r.ID, SHA: "x", Subject: "v1", CommittedAt: time.Now().UTC()}
	if _, err := store.UpsertCommit(context.Background(), c); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	c.Subject = "v2"
	if _, err := store.UpsertCommit(context.Background(), c); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _ := store.GetCommit(context.Background(), r.ID, "x", nil)
	if got.Subject != "v2" {
		t.Errorf("subject = %q after re-upsert; want v2", got.Subject)
	}
	rows, _ := store.ListCommits(context.Background(), r.ID, "", 0, nil)
	if len(rows) != 1 {
		t.Errorf("ListCommits len = %d, want 1 (idempotent)", len(rows))
	}
}

func TestMemoryStore_UpsertCommit_RejectsEmptyKeys(t *testing.T) {
	t.Parallel()
	store, _ := newCommitsFixture(t)
	if _, err := store.UpsertCommit(context.Background(), Commit{SHA: "x"}); err == nil {
		t.Errorf("UpsertCommit without RepoID succeeded")
	}
	if _, err := store.UpsertCommit(context.Background(), Commit{RepoID: "x"}); err == nil {
		t.Errorf("UpsertCommit without SHA succeeded")
	}
}

func TestMemoryStore_ListCommits_OrderingDESC(t *testing.T) {
	t.Parallel()
	store, r := newCommitsFixture(t)
	now := time.Now().UTC()
	for i, sha := range []string{"a", "b", "c"} {
		_, _ = store.UpsertCommit(context.Background(), Commit{
			RepoID: r.ID, SHA: sha, Subject: sha,
			CommittedAt: now.Add(time.Duration(i) * time.Minute),
		})
	}
	rows, err := store.ListCommits(context.Background(), r.ID, "", 0, nil)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].SHA != "c" || rows[1].SHA != "b" || rows[2].SHA != "a" {
		t.Errorf("ordering = [%s %s %s], want [c b a]", rows[0].SHA, rows[1].SHA, rows[2].SHA)
	}
}

func TestMemoryStore_ListCommits_Limit(t *testing.T) {
	t.Parallel()
	store, r := newCommitsFixture(t)
	now := time.Now().UTC()
	for i, sha := range []string{"a", "b", "c"} {
		_, _ = store.UpsertCommit(context.Background(), Commit{
			RepoID: r.ID, SHA: sha,
			CommittedAt: now.Add(time.Duration(i) * time.Minute),
		})
	}
	rows, _ := store.ListCommits(context.Background(), r.ID, "", 2, nil)
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2 (limit honored)", len(rows))
	}
}

func TestMemoryStore_ListCommits_UnknownRepo(t *testing.T) {
	t.Parallel()
	store, _ := newCommitsFixture(t)
	_, err := store.ListCommits(context.Background(), "repo_missing", "", 0, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ListCommits(missing) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_Diff_ParentWalk(t *testing.T) {
	t.Parallel()
	store, r := newCommitsFixture(t)
	now := time.Now().UTC()
	chain := []Commit{
		{RepoID: r.ID, SHA: "c1", Subject: "first", CommittedAt: now},
		{RepoID: r.ID, SHA: "c2", Subject: "second", ParentSHA: "c1", CommittedAt: now.Add(time.Minute)},
		{RepoID: r.ID, SHA: "c3", Subject: "third", ParentSHA: "c2", CommittedAt: now.Add(2 * time.Minute)},
	}
	for _, c := range chain {
		_, _ = store.UpsertCommit(context.Background(), c)
	}
	d, err := store.Diff(context.Background(), r.ID, "c1", "c3", nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(d.Commits) != 2 {
		t.Fatalf("Diff.Commits = %d, want 2 (c3, c2)", len(d.Commits))
	}
	if d.Commits[0].SHA != "c3" || d.Commits[1].SHA != "c2" {
		t.Errorf("Diff.Commits SHAs = [%s %s], want [c3 c2]", d.Commits[0].SHA, d.Commits[1].SHA)
	}
}

func TestMemoryStore_Diff_DiffSourceUnavailable(t *testing.T) {
	t.Parallel()
	store, r := newCommitsFixture(t)
	d, err := store.Diff(context.Background(), r.ID, "from", "to", nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.DiffSource != DiffSourceUnavailable {
		t.Errorf("DiffSource = %q, want %q", d.DiffSource, DiffSourceUnavailable)
	}
	if d.DiffSourceReason == "" {
		t.Errorf("DiffSourceReason empty; want non-empty constraint marker")
	}
	if d.Unified != "" || len(d.SymbolChanges) != 0 {
		t.Errorf("Unified+SymbolChanges should be empty in v1; got Unified=%q SymbolChanges=%v", d.Unified, d.SymbolChanges)
	}
}

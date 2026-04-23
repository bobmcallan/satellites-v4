package project

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewID_Format(t *testing.T) {
	t.Parallel()
	id := NewID()
	if len(id) != len("proj_")+8 {
		t.Errorf("id %q has wrong length", id)
	}
	if id[:5] != "proj_" {
		t.Errorf("id %q missing proj_ prefix", id)
	}
	if NewID() == id {
		t.Errorf("NewID should mint unique ids")
	}
}

func TestMemoryStore_CreateAndGetByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	p, err := store.Create(ctx, "user_1", "", "first project", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID == "" {
		t.Error("Create should mint an id")
	}
	if p.Status != StatusActive {
		t.Errorf("status = %q, want active", p.Status)
	}
	if !p.CreatedAt.Equal(now) || !p.UpdatedAt.Equal(now) {
		t.Errorf("timestamps not stamped from now")
	}

	got, err := store.GetByID(ctx, p.ID, nil)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != p {
		t.Errorf("GetByID mismatch: got %+v want %+v", got, p)
	}
}

func TestMemoryStore_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	_, err := store.GetByID(ctx, "proj_missing", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_ListByOwner_NewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	older, _ := store.Create(ctx, "user_1", "", "older", t0)
	newer, _ := store.Create(ctx, "user_1", "", "newer", t0.Add(time.Hour))
	_, _ = store.Create(ctx, "user_2", "", "other-owner", t0.Add(2*time.Hour))

	got, err := store.ListByOwner(ctx, "user_1", nil)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 projects for user_1, got %d", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("expected newest-first: got [%s,%s]", got[0].ID, got[1].ID)
	}
}

func TestMemoryStore_ListByOwner_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	got, err := store.ListByOwner(ctx, "user_unknown", nil)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty slice, got %d", len(got))
	}
}

func TestMemoryStore_UpdateName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now().UTC()

	p, _ := store.Create(ctx, "user_1", "", "original", t0)

	t1 := t0.Add(time.Hour)
	updated, err := store.UpdateName(ctx, p.ID, "renamed", t1)
	if err != nil {
		t.Fatalf("UpdateName: %v", err)
	}
	if updated.Name != "renamed" {
		t.Errorf("name = %q, want renamed", updated.Name)
	}
	if !updated.UpdatedAt.Equal(t1) {
		t.Errorf("UpdatedAt not bumped: got %v, want %v", updated.UpdatedAt, t1)
	}
	if !updated.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt mutated: got %v, want %v", updated.CreatedAt, t0)
	}

	got, _ := store.GetByID(ctx, p.ID, nil)
	if got.Name != "renamed" {
		t.Errorf("GetByID after update: name = %q, want renamed", got.Name)
	}
}

func TestMemoryStore_UpdateName_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	_, err := store.UpdateName(ctx, "proj_missing", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_OwnerIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	_, _ = store.Create(ctx, "user_a", "", "a-1", now)
	_, _ = store.Create(ctx, "user_a", "", "a-2", now)
	_, _ = store.Create(ctx, "user_b", "", "b-1", now)

	a, _ := store.ListByOwner(ctx, "user_a", nil)
	b, _ := store.ListByOwner(ctx, "user_b", nil)
	if len(a) != 2 {
		t.Errorf("user_a list size = %d, want 2", len(a))
	}
	if len(b) != 1 {
		t.Errorf("user_b list size = %d, want 1", len(b))
	}
}

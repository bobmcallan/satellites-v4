package workspace

import (
	"context"
	"errors"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
)

func TestNewID_Format(t *testing.T) {
	t.Parallel()
	id := NewID()
	if len(id) != len("wksp_")+8 {
		t.Errorf("id %q has wrong length", id)
	}
	if id[:5] != "wksp_" {
		t.Errorf("id %q missing wksp_ prefix", id)
	}
	if NewID() == id {
		t.Error("NewID should mint unique ids")
	}
}

func TestIsValidRole(t *testing.T) {
	t.Parallel()
	for _, r := range []string{RoleAdmin, RoleMember, RoleReviewer, RoleViewer} {
		if !IsValidRole(r) {
			t.Errorf("role %q should be valid", r)
		}
	}
	for _, r := range []string{"", "owner", "guest", "ADMIN"} {
		if IsValidRole(r) {
			t.Errorf("role %q should be invalid", r)
		}
	}
}

func TestMemoryStore_CreateAndGetByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	w, err := store.Create(ctx, "user_1", "alpha", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.ID == "" {
		t.Error("Create should mint an id")
	}
	if w.Status != StatusActive {
		t.Errorf("status = %q, want active", w.Status)
	}
	if !w.CreatedAt.Equal(now) || !w.UpdatedAt.Equal(now) {
		t.Error("timestamps not stamped from now")
	}

	got, err := store.GetByID(ctx, w.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != w {
		t.Errorf("GetByID mismatch: got %+v want %+v", got, w)
	}
}

func TestMemoryStore_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.GetByID(ctx, "wksp_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_Create_AdminMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	w, err := store.Create(ctx, "user_1", "alpha", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	is, err := store.IsMember(ctx, w.ID, "user_1")
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if !is {
		t.Error("creator should be member of their workspace")
	}
	role, err := store.GetRole(ctx, w.ID, "user_1")
	if err != nil {
		t.Fatalf("GetRole: %v", err)
	}
	if role != RoleAdmin {
		t.Errorf("role = %q, want admin", role)
	}
}

func TestMemoryStore_IsMember_NonMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	w, _ := store.Create(ctx, "user_1", "alpha", now)
	is, err := store.IsMember(ctx, w.ID, "user_other")
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if is {
		t.Error("non-member should not be reported as member")
	}
}

func TestMemoryStore_ListByMember_FiltersAndSortsNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	older, _ := store.Create(ctx, "user_1", "older", t0)
	newer, _ := store.Create(ctx, "user_1", "newer", t0.Add(time.Hour))
	_, _ = store.Create(ctx, "user_2", "other-owner", t0.Add(2*time.Hour))

	got, err := store.ListByMember(ctx, "user_1")
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 workspaces for user_1, got %d", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("expected newest-first: got [%s,%s]", got[0].ID, got[1].ID)
	}
}

func TestEnsureDefault_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	logger := satarbor.Default()
	now := time.Now().UTC()

	first, err := EnsureDefault(ctx, store, logger, "user_1", now)
	if err != nil {
		t.Fatalf("EnsureDefault first: %v", err)
	}
	if first == "" {
		t.Fatal("EnsureDefault returned empty id")
	}
	second, err := EnsureDefault(ctx, store, logger, "user_1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("EnsureDefault second: %v", err)
	}
	if second != first {
		t.Errorf("EnsureDefault not idempotent: first=%q second=%q", first, second)
	}

	ws, err := store.ListByMember(ctx, "user_1")
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(ws) != 1 {
		t.Fatalf("want exactly one workspace after repeat EnsureDefault, got %d", len(ws))
	}
}

func TestMemoryStore_AddMember_UnknownRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	w, _ := store.Create(ctx, "u1", "ws", time.Now().UTC())
	if err := store.AddMember(ctx, w.ID, "u2", "garbage", "u1", time.Now().UTC()); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("want ErrInvalidRole, got %v", err)
	}
}

func TestMemoryStore_ListMembers_OrderedAndFiltered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w, _ := store.Create(ctx, "u1", "ws", t0)
	if err := store.AddMember(ctx, w.ID, "u2", RoleMember, "u1", t0.Add(time.Hour)); err != nil {
		t.Fatalf("add u2: %v", err)
	}
	if err := store.AddMember(ctx, w.ID, "u3", RoleViewer, "u1", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("add u3: %v", err)
	}
	members, err := store.ListMembers(ctx, w.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("want 3 members, got %d", len(members))
	}
	if members[0].UserID != "u1" || members[2].UserID != "u3" {
		t.Errorf("unexpected order: %+v", members)
	}
}

func TestMemoryStore_ListMembers_UnknownWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.ListMembers(ctx, "wksp_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_UpdateRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	w, _ := store.Create(ctx, "u1", "ws", time.Now().UTC())
	_ = store.AddMember(ctx, w.ID, "u2", RoleMember, "u1", time.Now().UTC())

	if err := store.UpdateRole(ctx, w.ID, "u2", RoleAdmin, time.Now().UTC()); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	role, err := store.GetRole(ctx, w.ID, "u2")
	if err != nil {
		t.Fatalf("GetRole: %v", err)
	}
	if role != RoleAdmin {
		t.Errorf("role = %q, want admin", role)
	}
}

func TestMemoryStore_UpdateRole_InvalidRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	w, _ := store.Create(ctx, "u1", "ws", time.Now().UTC())
	if err := store.UpdateRole(ctx, w.ID, "u1", "garbage", time.Now().UTC()); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("want ErrInvalidRole, got %v", err)
	}
}

func TestMemoryStore_UpdateRole_NotAMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	w, _ := store.Create(ctx, "u1", "ws", time.Now().UTC())
	if err := store.UpdateRole(ctx, w.ID, "u_missing", RoleMember, time.Now().UTC()); !errors.Is(err, ErrMemberNotFound) {
		t.Errorf("want ErrMemberNotFound, got %v", err)
	}
}

func TestMemoryStore_RemoveMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	w, _ := store.Create(ctx, "u1", "ws", time.Now().UTC())
	_ = store.AddMember(ctx, w.ID, "u2", RoleMember, "u1", time.Now().UTC())

	if err := store.RemoveMember(ctx, w.ID, "u2"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	is, _ := store.IsMember(ctx, w.ID, "u2")
	if is {
		t.Error("u2 should not be a member after removal")
	}
	if err := store.RemoveMember(ctx, w.ID, "u2"); !errors.Is(err, ErrMemberNotFound) {
		t.Errorf("second remove: want ErrMemberNotFound, got %v", err)
	}
}

func TestEnsureDefault_PerUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	logger := satarbor.Default()
	now := time.Now().UTC()

	a, err := EnsureDefault(ctx, store, logger, "user_a", now)
	if err != nil {
		t.Fatalf("EnsureDefault user_a: %v", err)
	}
	b, err := EnsureDefault(ctx, store, logger, "user_b", now)
	if err != nil {
		t.Fatalf("EnsureDefault user_b: %v", err)
	}
	if a == b {
		t.Error("different users should get different default workspaces")
	}

	isA, _ := store.IsMember(ctx, a, "user_a")
	isB, _ := store.IsMember(ctx, b, "user_b")
	crossA, _ := store.IsMember(ctx, a, "user_b")
	crossB, _ := store.IsMember(ctx, b, "user_a")

	if !isA || !isB {
		t.Error("owners should be members of their own default workspace")
	}
	if crossA || crossB {
		t.Error("default workspaces must not leak membership across users")
	}
}

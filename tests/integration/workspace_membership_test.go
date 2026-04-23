package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// TestWorkspaceMembershipStore_RoundTrip drives the new ListMembers /
// UpdateRole / RemoveMember verbs end-to-end against SurrealDB. The
// last-admin guard lives at the handler layer in mcpserver; this test
// exercises the store primitives and proves the SQL paths are sound.
func TestWorkspaceMembershipStore_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			WaitingFor:   wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	host, _ := surreal.Host(ctx)
	mapped, _ := surreal.MappedPort(ctx, "8000/tcp")
	dsn := fmt.Sprintf("ws://root:root@%s:%s/rpc/satellites/satellites", host, mapped.Port())
	cfg, err := db.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	conn, err := db.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	store := workspace.NewSurrealStore(conn)
	now := time.Now().UTC()

	w, err := store.Create(ctx, "user_admin", "team", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Add two members at different roles.
	if err := store.AddMember(ctx, w.ID, "user_member", workspace.RoleMember, "user_admin", now.Add(time.Hour)); err != nil {
		t.Fatalf("AddMember member: %v", err)
	}
	if err := store.AddMember(ctx, w.ID, "user_viewer", workspace.RoleViewer, "user_admin", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("AddMember viewer: %v", err)
	}

	// ListMembers returns 3 rows ordered by AddedAt asc.
	members, err := store.ListMembers(ctx, w.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("want 3 members, got %d", len(members))
	}
	if members[0].UserID != "user_admin" || members[0].Role != workspace.RoleAdmin {
		t.Errorf("first should be admin-creator: %+v", members[0])
	}
	if members[1].UserID != "user_member" || members[2].UserID != "user_viewer" {
		t.Errorf("unexpected order: %+v", members)
	}

	// UpdateRole: promote user_member → admin; preserves AddedAt/AddedBy.
	priorAddedAt := members[1].AddedAt
	priorAddedBy := members[1].AddedBy
	if err := store.UpdateRole(ctx, w.ID, "user_member", workspace.RoleAdmin, now.Add(3*time.Hour)); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	role, err := store.GetRole(ctx, w.ID, "user_member")
	if err != nil {
		t.Fatalf("GetRole: %v", err)
	}
	if role != workspace.RoleAdmin {
		t.Errorf("role after update = %q, want admin", role)
	}
	members, err = store.ListMembers(ctx, w.ID)
	if err != nil {
		t.Fatalf("ListMembers after update: %v", err)
	}
	var found bool
	for _, m := range members {
		if m.UserID == "user_member" {
			found = true
			if !m.AddedAt.Equal(priorAddedAt) {
				t.Errorf("AddedAt mutated on role change: got %v, want %v", m.AddedAt, priorAddedAt)
			}
			if m.AddedBy != priorAddedBy {
				t.Errorf("AddedBy mutated on role change: got %q, want %q", m.AddedBy, priorAddedBy)
			}
		}
	}
	if !found {
		t.Error("user_member missing from ListMembers after UpdateRole")
	}

	// UpdateRole unknown role → ErrInvalidRole.
	if err := store.UpdateRole(ctx, w.ID, "user_member", "garbage", now); !errors.Is(err, workspace.ErrInvalidRole) {
		t.Errorf("want ErrInvalidRole, got %v", err)
	}

	// UpdateRole not-a-member → ErrMemberNotFound.
	if err := store.UpdateRole(ctx, w.ID, "user_unknown", workspace.RoleMember, now); !errors.Is(err, workspace.ErrMemberNotFound) {
		t.Errorf("want ErrMemberNotFound, got %v", err)
	}

	// RemoveMember.
	if err := store.RemoveMember(ctx, w.ID, "user_viewer"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if is, _ := store.IsMember(ctx, w.ID, "user_viewer"); is {
		t.Error("user_viewer still a member after RemoveMember")
	}
	// Second remove → ErrMemberNotFound.
	if err := store.RemoveMember(ctx, w.ID, "user_viewer"); !errors.Is(err, workspace.ErrMemberNotFound) {
		t.Errorf("second remove: want ErrMemberNotFound, got %v", err)
	}

	// ListMembers reflects the removal.
	members, _ = store.ListMembers(ctx, w.ID)
	if len(members) != 2 {
		t.Errorf("after remove: want 2 members, got %d", len(members))
	}
}

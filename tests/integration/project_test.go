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
	"github.com/bobmcallan/satellites/internal/project"
)

// TestProjectSurrealStore_RoundTrip boots a SurrealDB container, constructs a
// SurrealStore directly against it, and exercises Create / GetByID /
// ListByOwner / UpdateName end-to-end. No MCP surface is involved at this
// story — the MCP wiring lands in story 1.2.
func TestProjectSurrealStore_RoundTrip(t *testing.T) {
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

	host, err := surreal.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mapped, err := surreal.MappedPort(ctx, "8000/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	dsn := fmt.Sprintf("ws://root:root@%s:%s/rpc/satellites/satellites", host, mapped.Port())
	cfg, err := db.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	conn, err := db.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect surrealdb: %v", err)
	}

	store := project.NewSurrealStore(conn)
	now := time.Now().UTC()

	// Create.
	p1, err := store.Create(ctx, "user_alice", "", "alpha", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p1.ID == "" || p1.Status != project.StatusActive {
		t.Errorf("unexpected Project: %+v", p1)
	}

	p2, err := store.Create(ctx, "user_alice", "", "beta", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	_, err = store.Create(ctx, "user_bob", "", "bob-only", now)
	if err != nil {
		t.Fatalf("Create third: %v", err)
	}

	// GetByID round-trip.
	got, err := store.GetByID(ctx, p1.ID, nil)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "alpha" || got.OwnerUserID != "user_alice" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// GetByID missing → ErrNotFound.
	if _, err := store.GetByID(ctx, "proj_missing", nil); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}

	// ListByOwner filters + newest-first.
	list, err := store.ListByOwner(ctx, "user_alice", nil)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 alice projects, got %d", len(list))
	}
	if list[0].ID != p2.ID || list[1].ID != p1.ID {
		t.Errorf("expected newest-first: got [%s,%s]", list[0].ID, list[1].ID)
	}

	// UpdateName bumps UpdatedAt.
	renamed, err := store.UpdateName(ctx, p1.ID, "alpha-renamed", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("UpdateName: %v", err)
	}
	if renamed.Name != "alpha-renamed" {
		t.Errorf("name = %q, want alpha-renamed", renamed.Name)
	}
	if !renamed.UpdatedAt.After(p1.UpdatedAt) {
		t.Errorf("UpdatedAt should have advanced: got %v vs original %v", renamed.UpdatedAt, p1.UpdatedAt)
	}

	// UpdateName missing → ErrNotFound.
	if _, err := store.UpdateName(ctx, "proj_missing", "x", now); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("UpdateName(missing) want ErrNotFound, got %v", err)
	}
}

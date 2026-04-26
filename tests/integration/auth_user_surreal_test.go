package integration

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/db"
)

// TestSurrealUserStore_RoundTrip exercises Add → GetByID → GetByEmail →
// Update against a real SurrealDB. Story_7512783a AC1+AC4 (interface
// surface + tests pass).
func TestSurrealUserStore_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {"surrealdb"},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })
	host, _ := surreal.Host(ctx)
	port, _ := surreal.MappedPort(ctx, "8000/tcp")

	dsn := "ws://root:root@" + host + ":" + port.Port() + "/rpc/satellites/satellites"
	cfgDB, err := db.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	conn, err := db.Connect(ctx, cfgDB)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	store := auth.NewSurrealUserStore(conn)

	store.Add(auth.User{
		ID:          "u_alice",
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Provider:    "google",
	})

	got, err := store.GetByID("u_alice")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Email != "alice@example.com" || got.DisplayName != "Alice" || got.Provider != "google" {
		t.Errorf("get by id round-trip = %+v", got)
	}

	gotEmail, err := store.GetByEmail("ALICE@example.com") // case-insensitive
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if gotEmail.ID != "u_alice" {
		t.Errorf("get by email returned %+v", gotEmail)
	}

	// Update existing user.
	updated := auth.User{
		ID:          "u_alice",
		Email:       "alice@example.com",
		DisplayName: "Alice Updated",
		Provider:    "google",
	}
	if err := store.Update(updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = store.GetByID("u_alice")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.DisplayName != "Alice Updated" {
		t.Errorf("display_name post-update = %q, want Alice Updated", got.DisplayName)
	}

	// Update unknown user must return ErrNoSuchUser.
	if err := store.Update(auth.User{ID: "u_missing"}); err == nil {
		t.Errorf("update missing user returned nil; want ErrNoSuchUser")
	}

	// GetByID for missing returns ErrNoSuchUser.
	if _, err := store.GetByID("u_missing"); err == nil {
		t.Errorf("get missing returned no error")
	}
}

// TestSurrealUserStore_PersistsAcrossReconnect simulates a satellites
// container restart by tearing down the current SurrealDB connection
// and opening a fresh one against the same database. Story_7512783a
// AC3 (persistence) — paired with story_0ab83f82's session-store work.
func TestSurrealUserStore_PersistsAcrossReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {"surrealdb"},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })
	host, _ := surreal.Host(ctx)
	port, _ := surreal.MappedPort(ctx, "8000/tcp")
	dsn := "ws://root:root@" + host + ":" + port.Port() + "/rpc/satellites/satellites"
	cfgDB, _ := db.ParseDSN(dsn)

	// First "satellites process" — write the user.
	conn1, err := db.Connect(ctx, cfgDB)
	if err != nil {
		t.Fatalf("connect 1: %v", err)
	}
	store1 := auth.NewSurrealUserStore(conn1)
	store1.Add(auth.User{
		ID:          "u_persist",
		Email:       "persist@example.com",
		DisplayName: "Persist",
		Provider:    "github",
	})

	// Simulate satellites restart: drop the connection.
	conn1.Close(ctx)

	// Second "satellites process" — same DB, fresh connection.
	conn2, err := db.Connect(ctx, cfgDB)
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	store2 := auth.NewSurrealUserStore(conn2)
	got, err := store2.GetByID("u_persist")
	if err != nil {
		t.Fatalf("get after reconnect: %v", err)
	}
	if got.Email != "persist@example.com" || got.DisplayName != "Persist" {
		t.Errorf("user did not persist across reconnect; got %+v", got)
	}
}

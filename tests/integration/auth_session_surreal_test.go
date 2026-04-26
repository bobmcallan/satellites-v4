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

// TestSurrealSessionStore_RoundTrip exercises the cookie-session round
// trip against a real SurrealDB: Create, Get, SetActiveWorkspace, Delete.
// Story_0ab83f82 AC1+AC3 (persistence) + AC5 (no cookie format change).
func TestSurrealSessionStore_RoundTrip(t *testing.T) {
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
	host, err := surreal.Host(ctx)
	if err != nil {
		t.Fatalf("surreal host: %v", err)
	}
	port, err := surreal.MappedPort(ctx, "8000/tcp")
	if err != nil {
		t.Fatalf("surreal port: %v", err)
	}

	dsn := "ws://root:root@" + host + ":" + port.Port() + "/rpc/satellites/satellites"
	cfg, err := db.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	conn, err := db.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	store := auth.NewSurrealSessionStore(conn)

	// Create + Get round-trip.
	sess, err := store.Create("u_alice", time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.ID == "" || sess.UserID != "u_alice" {
		t.Fatalf("create returned %+v", sess)
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != "u_alice" || got.ID != sess.ID {
		t.Errorf("get round-trip = %+v, want id=%s user=u_alice", got, sess.ID)
	}

	// SetActiveWorkspace mutates and persists.
	if err := store.SetActiveWorkspace(sess.ID, "wksp_alpha"); err != nil {
		t.Fatalf("set active workspace: %v", err)
	}
	got, err = store.Get(sess.ID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.ActiveWorkspaceID != "wksp_alpha" {
		t.Errorf("active workspace = %q, want wksp_alpha", got.ActiveWorkspaceID)
	}

	// Delete is idempotent (logout).
	if err := store.Delete(sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(sess.ID); err == nil {
		t.Errorf("get after delete returned no error; want ErrSessionNotFound")
	}
	if err := store.Delete(sess.ID); err != nil {
		t.Errorf("idempotent delete returned error: %v", err)
	}
}

// TestSurrealSessionStore_Sweep covers AC4 — expired rows are removed
// when Sweep runs with a cutoff past their ExpiresAt.
func TestSurrealSessionStore_Sweep(t *testing.T) {
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

	store := auth.NewSurrealSessionStore(conn)

	freshTTL := time.Hour
	expiredTTL := time.Millisecond

	fresh, err := store.Create("u_fresh", freshTTL)
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	expired, err := store.Create("u_expired", expiredTTL)
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}

	// Wait so the expired row's expires_at is firmly in the past.
	time.Sleep(50 * time.Millisecond)

	removed, err := store.Sweep(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed < 1 {
		t.Errorf("sweep removed %d rows, want >=1 (the expired one)", removed)
	}

	if _, err := store.Get(expired.ID); err == nil {
		t.Errorf("expired session still readable after sweep")
	}
	if _, err := store.Get(fresh.ID); err != nil {
		t.Errorf("fresh session lost during sweep: %v", err)
	}
}

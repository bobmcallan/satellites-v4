package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// TestLedgerSurrealStore_RoundTrip boots SurrealDB and drives the ledger
// SurrealStore directly: Append + List with type filter + project
// isolation. No MCP surface involved.
func TestLedgerSurrealStore_RoundTrip(t *testing.T) {
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

	store := ledger.NewSurrealStore(conn)
	t0 := time.Now().UTC().Truncate(time.Microsecond)

	for i, spec := range []struct {
		pid, etype string
	}{
		{"proj_a", "story.status_change"},
		{"proj_a", "story.status_change"},
		{"proj_a", "document.ingest"},
		{"proj_b", "story.status_change"},
	} {
		_, err := store.Append(ctx, ledger.LedgerEntry{
			ProjectID: spec.pid,
			Type:      spec.etype,
			Content:   fmt.Sprintf("row %d", i),
			Actor:     "u_test",
		}, t0.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	// List proj_a should be newest-first with 3 entries.
	listA, err := store.List(ctx, "proj_a", ledger.ListOptions{}, nil)
	if err != nil {
		t.Fatalf("List proj_a: %v", err)
	}
	if len(listA) != 3 {
		t.Fatalf("proj_a count = %d, want 3", len(listA))
	}
	if !listA[0].CreatedAt.After(listA[1].CreatedAt) || !listA[1].CreatedAt.After(listA[2].CreatedAt) {
		t.Errorf("proj_a not newest-first: %v", listA)
	}
	for _, e := range listA {
		if e.ID == "" {
			t.Errorf("entry id empty after round-trip: %+v", e)
		}
		if e.ProjectID != "proj_a" {
			t.Errorf("entry leaked: %+v", e)
		}
	}

	// Type filter.
	statusOnly, _ := store.List(ctx, "proj_a", ledger.ListOptions{Type: "story.status_change"}, nil)
	if len(statusOnly) != 2 {
		t.Errorf("type filter: count = %d, want 2", len(statusOnly))
	}
	for _, e := range statusOnly {
		if e.Type != "story.status_change" {
			t.Errorf("type filter leaked %q", e.Type)
		}
	}

	// Limit clamp.
	capped, _ := store.List(ctx, "proj_a", ledger.ListOptions{Limit: 1}, nil)
	if len(capped) != 1 {
		t.Errorf("limit 1 returned %d", len(capped))
	}

	// Project isolation.
	listB, _ := store.List(ctx, "proj_b", ledger.ListOptions{}, nil)
	if len(listB) != 1 {
		t.Errorf("proj_b count = %d, want 1", len(listB))
	}
}

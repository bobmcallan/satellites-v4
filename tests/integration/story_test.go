package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
)

// TestStorySurrealStore_RoundTrip drives the Surreal-backed story Store
// directly end-to-end: Create, UpdateStatus (with ledger emission via a
// real ledger.SurrealStore), List, and rejection of invalid transitions.
func TestStorySurrealStore_RoundTrip(t *testing.T) {
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
		t.Fatalf("surreal: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	host, _ := surreal.Host(ctx)
	mapped, _ := surreal.MappedPort(ctx, "8000/tcp")
	dsn := fmt.Sprintf("ws://root:root@%s:%s/rpc/satellites/satellites", host, mapped.Port())
	cfg, _ := db.ParseDSN(dsn)
	conn, err := db.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("db connect: %v", err)
	}

	led := ledger.NewSurrealStore(conn)
	stories := story.NewSurrealStore(conn, led)
	now := time.Now().UTC()

	s, err := stories.Create(ctx, story.Story{
		ProjectID:   "proj_a",
		Title:       "build the thing",
		Description: "do it",
		Priority:    "high",
		Category:    "feature",
		Tags:        []string{"epic:v4-stories"},
		CreatedBy:   "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(s.ID, "sty_") {
		t.Fatalf("id = %q", s.ID)
	}
	if s.Status != story.StatusBacklog {
		t.Errorf("default status = %q, want backlog", s.Status)
	}

	// Transition through the happy path at strictly increasing times to
	// avoid SurrealDB datetime tie-breaks.
	for i, next := range []string{story.StatusReady, story.StatusInProgress, story.StatusDone} {
		_, err := stories.UpdateStatus(ctx, s.ID, next, "u_alice", now.Add(time.Duration(i+1)*1200*time.Millisecond), nil)
		if err != nil {
			t.Fatalf("UpdateStatus %q: %v", next, err)
		}
	}

	got, err := stories.GetByID(ctx, s.ID, nil)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != story.StatusDone {
		t.Errorf("final status = %q, want done", got.Status)
	}

	// Invalid transition: done → ready is terminal.
	if _, err := stories.UpdateStatus(ctx, s.ID, story.StatusReady, "u_alice", now.Add(10*time.Second), nil); err == nil {
		t.Errorf("terminal state should reject further transitions")
	}

	// Ledger contains exactly 3 rows for this story.
	entries, _ := led.List(ctx, "proj_a", ledger.ListOptions{Type: story.LedgerEntryType}, nil)
	if len(entries) != 3 {
		t.Errorf("ledger rows = %d, want 3", len(entries))
	}
	for _, e := range entries {
		if !strings.Contains(e.Content, s.ID) {
			t.Errorf("ledger entry content missing story id: %q", e.Content)
		}
	}

	// List filters.
	byTag, _ := stories.List(ctx, "proj_a", story.ListOptions{Tag: "epic:v4-stories"}, nil)
	if len(byTag) != 1 {
		t.Errorf("tag filter count = %d", len(byTag))
	}
	byStatus, _ := stories.List(ctx, "proj_a", story.ListOptions{Status: story.StatusDone}, nil)
	if len(byStatus) != 1 {
		t.Errorf("status=done count = %d", len(byStatus))
	}
}

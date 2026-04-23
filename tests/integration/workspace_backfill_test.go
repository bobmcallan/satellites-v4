package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// TestWorkspaceBackfill_AcrossPrimitives is the feature-order:2 load-bearing
// test: it boots SurrealDB, writes project/story/ledger/document rows with
// empty workspace_id (simulating the pre-migration state), runs
// workspace.BackfillPrimitives, and asserts every row is now stamped with
// the owner's default workspace id. A second call is a no-op.
func TestWorkspaceBackfill_AcrossPrimitives(t *testing.T) {
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

	wsStore := workspace.NewSurrealStore(conn)
	projStore := project.NewSurrealStore(conn)
	ledStore := ledger.NewSurrealStore(conn)
	storyStore := story.NewSurrealStore(conn, ledStore)
	docStore := document.NewSurrealStore(conn)
	logger := satarbor.Default()
	now := time.Now().UTC()

	// Simulate pre-migration: create a project for user_alice with empty
	// workspace_id, then a story + ledger row + document row inside it.
	p, err := projStore.Create(ctx, "user_alice", "", "alpha", now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}
	s, err := storyStore.Create(ctx, story.Story{
		ProjectID: p.ID,
		Title:     "implement X",
		Priority:  "medium",
		Category:  "feature",
	}, now)
	if err != nil {
		t.Fatalf("story create: %v", err)
	}
	e, err := ledStore.Append(ctx, ledger.LedgerEntry{
		ProjectID: p.ID,
		Type:      "test.event",
		Content:   "legacy",
		Actor:     "user_alice",
	}, now)
	if err != nil {
		t.Fatalf("ledger append: %v", err)
	}
	d, err := docStore.Upsert(ctx, "", p.ID, "x.md", "architecture", []byte("legacy doc"), now)
	if err != nil {
		t.Fatalf("document upsert: %v", err)
	}

	// Sanity: every row starts with empty workspace_id.
	if p.WorkspaceID != "" {
		t.Fatalf("pre-condition: project workspace_id should be empty, got %q", p.WorkspaceID)
	}
	if s.WorkspaceID != "" {
		t.Fatalf("pre-condition: story workspace_id should be empty, got %q", s.WorkspaceID)
	}
	if e.WorkspaceID != "" {
		t.Fatalf("pre-condition: ledger workspace_id should be empty, got %q", e.WorkspaceID)
	}
	if d.Document.WorkspaceID != "" {
		t.Fatalf("pre-condition: document workspace_id should be empty, got %q", d.Document.WorkspaceID)
	}

	// Run backfill.
	report, err := workspace.BackfillPrimitives(ctx, wsStore, projStore, storyStore, ledStore, docStore, logger, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("BackfillPrimitives: %v", err)
	}
	if report.ProjectsStamped != 1 {
		t.Errorf("projects_stamped = %d, want 1", report.ProjectsStamped)
	}
	if report.StoriesStamped != 1 {
		t.Errorf("stories_stamped = %d, want 1", report.StoriesStamped)
	}
	if report.LedgerStamped != 1 {
		t.Errorf("ledger_stamped = %d, want 1", report.LedgerStamped)
	}
	if report.DocumentsStamped != 1 {
		t.Errorf("documents_stamped = %d, want 1", report.DocumentsStamped)
	}

	// Alice should now have a default workspace.
	wsList, err := wsStore.ListByMember(ctx, "user_alice")
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(wsList) != 1 {
		t.Fatalf("want 1 workspace for alice, got %d", len(wsList))
	}
	wsID := wsList[0].ID

	// All rows should now carry that workspace_id.
	pr, err := projStore.GetByID(ctx, p.ID, nil)
	if err != nil {
		t.Fatalf("re-read project: %v", err)
	}
	if pr.WorkspaceID != wsID {
		t.Errorf("project workspace_id = %q, want %q", pr.WorkspaceID, wsID)
	}
	sr, err := storyStore.GetByID(ctx, s.ID, nil)
	if err != nil {
		t.Fatalf("re-read story: %v", err)
	}
	if sr.WorkspaceID != wsID {
		t.Errorf("story workspace_id = %q, want %q", sr.WorkspaceID, wsID)
	}
	entries, err := ledStore.List(ctx, p.ID, ledger.ListOptions{}, nil)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(entries) == 0 || entries[0].WorkspaceID != wsID {
		t.Errorf("ledger workspace_id = %+v, want %q", entries, wsID)
	}
	docGot, err := docStore.GetByFilename(ctx, p.ID, "x.md", nil)
	if err != nil {
		t.Fatalf("document get: %v", err)
	}
	if docGot.WorkspaceID != wsID {
		t.Errorf("document workspace_id = %q, want %q", docGot.WorkspaceID, wsID)
	}

	// Second call is a no-op.
	report2, err := workspace.BackfillPrimitives(ctx, wsStore, projStore, storyStore, ledStore, docStore, logger, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("BackfillPrimitives second: %v", err)
	}
	if report2.ProjectsStamped != 0 || report2.StoriesStamped != 0 || report2.LedgerStamped != 0 || report2.DocumentsStamped != 0 {
		t.Errorf("second backfill should be a no-op, got %+v", report2)
	}
}

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
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// TestWorkspaceScoping_CrossWorkspaceDenial is the feature-order:3 load-
// bearing test: alice in workspace A, bob in workspace B. Rows created
// inside workspace A are invisible to bob's member list ([wsB]) via every
// read path. Alice's reads with nil memberships still see everything
// (bootstrap/backfill paths). Alice's reads scoped to [wsA] see her rows;
// alice's reads scoped to [wsB] return empty.
func TestWorkspaceScoping_CrossWorkspaceDenial(t *testing.T) {
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
	now := time.Now().UTC()

	wsA, err := wsStore.Create(ctx, "user_alice", "alice-ws", now)
	if err != nil {
		t.Fatalf("ws alice: %v", err)
	}
	wsB, err := wsStore.Create(ctx, "user_bob", "bob-ws", now)
	if err != nil {
		t.Fatalf("ws bob: %v", err)
	}
	aliceMem := []string{wsA.ID}
	bobMem := []string{wsB.ID}

	// Alice creates project/story/ledger/document inside wsA.
	pA, err := projStore.Create(ctx, "user_alice", wsA.ID, "alice-proj", now)
	if err != nil {
		t.Fatalf("alice project: %v", err)
	}
	sA, err := storyStore.Create(ctx, story.Story{WorkspaceID: wsA.ID, ProjectID: pA.ID, Title: "alice-story"}, now)
	if err != nil {
		t.Fatalf("alice story: %v", err)
	}
	if _, err := ledStore.Append(ctx, ledger.LedgerEntry{WorkspaceID: wsA.ID, ProjectID: pA.ID, Type: "alice.event", Actor: "user_alice"}, now); err != nil {
		t.Fatalf("alice ledger: %v", err)
	}
	if _, err := docStore.Upsert(ctx, wsA.ID, pA.ID, "alice.md", "architecture", []byte("alice-body"), now); err != nil {
		t.Fatalf("alice doc: %v", err)
	}

	// Alice's view with her memberships sees her rows.
	pgot, err := projStore.GetByID(ctx, pA.ID, aliceMem)
	if err != nil || pgot.ID != pA.ID {
		t.Errorf("alice GetByID scoped: got %+v err %v", pgot, err)
	}
	aList, err := projStore.ListByOwner(ctx, "user_alice", aliceMem)
	if err != nil || len(aList) != 1 {
		t.Errorf("alice ListByOwner scoped: got %+v err %v", aList, err)
	}
	sGot, err := storyStore.GetByID(ctx, sA.ID, aliceMem)
	if err != nil || sGot.ID != sA.ID {
		t.Errorf("alice story GetByID scoped: got %+v err %v", sGot, err)
	}
	sList, err := storyStore.List(ctx, pA.ID, story.ListOptions{}, aliceMem)
	if err != nil || len(sList) != 1 {
		t.Errorf("alice story List scoped: got %+v err %v", sList, err)
	}
	lList, err := ledStore.List(ctx, pA.ID, ledger.ListOptions{}, aliceMem)
	if err != nil || len(lList) != 1 {
		t.Errorf("alice ledger List scoped: got %+v err %v", lList, err)
	}
	dGot, err := docStore.GetByFilename(ctx, pA.ID, "alice.md", aliceMem)
	if err != nil || dGot.ID == "" {
		t.Errorf("alice doc GetByFilename scoped: got %+v err %v", dGot, err)
	}
	dCount, err := docStore.Count(ctx, pA.ID, aliceMem)
	if err != nil || dCount != 1 {
		t.Errorf("alice doc Count scoped: got %d err %v", dCount, err)
	}

	// Bob's view with his memberships must NOT see alice's rows.
	if _, err := projStore.GetByID(ctx, pA.ID, bobMem); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("bob GetByID on alice project should be not-found; err=%v", err)
	}
	bList, err := projStore.ListByOwner(ctx, "user_alice", bobMem)
	if err != nil || len(bList) != 0 {
		t.Errorf("bob ListByOwner (alice) scoped: got %+v err %v", bList, err)
	}
	if _, err := storyStore.GetByID(ctx, sA.ID, bobMem); !errors.Is(err, story.ErrNotFound) {
		t.Errorf("bob story GetByID should be not-found; err=%v", err)
	}
	bsList, err := storyStore.List(ctx, pA.ID, story.ListOptions{}, bobMem)
	if err != nil || len(bsList) != 0 {
		t.Errorf("bob story List scoped: got %+v err %v", bsList, err)
	}
	blList, err := ledStore.List(ctx, pA.ID, ledger.ListOptions{}, bobMem)
	if err != nil || len(blList) != 0 {
		t.Errorf("bob ledger List scoped: got %+v err %v", blList, err)
	}
	if _, err := docStore.GetByFilename(ctx, pA.ID, "alice.md", bobMem); !errors.Is(err, document.ErrNotFound) {
		t.Errorf("bob doc GetByFilename should be not-found; err=%v", err)
	}
	bdCount, err := docStore.Count(ctx, pA.ID, bobMem)
	if err != nil || bdCount != 0 {
		t.Errorf("bob doc Count scoped: got %d err %v", bdCount, err)
	}

	// Empty memberships slice (caller authenticated but workspace-less) is
	// deny-all across every primitive.
	emptyMem := []string{}
	if _, err := projStore.GetByID(ctx, pA.ID, emptyMem); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("empty memberships project GetByID should be not-found; err=%v", err)
	}
	eList, err := projStore.ListByOwner(ctx, "user_alice", emptyMem)
	if err != nil || len(eList) != 0 {
		t.Errorf("empty memberships ListByOwner should be empty; got %+v err %v", eList, err)
	}
	if _, err := storyStore.GetByID(ctx, sA.ID, emptyMem); !errors.Is(err, story.ErrNotFound) {
		t.Errorf("empty memberships story GetByID should be not-found; err=%v", err)
	}

	// nil memberships (bootstrap/backfill path) sees everything.
	if _, err := projStore.GetByID(ctx, pA.ID, nil); err != nil {
		t.Errorf("nil memberships project GetByID should see the row; err=%v", err)
	}
}

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
	if _, err := ledStore.Append(ctx, ledger.LedgerEntry{WorkspaceID: wsA.ID, ProjectID: pA.ID, Type: ledger.TypeDecision, CreatedBy: "user_alice"}, now); err != nil {
		t.Fatalf("alice ledger: %v", err)
	}
	if _, err := docStore.Upsert(ctx, document.UpsertInput{
		WorkspaceID: wsA.ID,
		ProjectID:   document.StringPtr(pA.ID),
		Type:        document.TypeArtifact,
		Name:        "alice.md",
		Body:        []byte("alice-body"),
		Scope:       document.ScopeProject,
		Actor:       "test",
	}, now); err != nil {
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
	dGot, err := docStore.GetByName(ctx, pA.ID, "alice.md", aliceMem)
	if err != nil || dGot.ID == "" {
		t.Errorf("alice doc GetByName scoped: got %+v err %v", dGot, err)
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
	if _, err := docStore.GetByName(ctx, pA.ID, "alice.md", bobMem); !errors.Is(err, document.ErrNotFound) {
		t.Errorf("bob doc GetByName should be not-found; err=%v", err)
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

// TestResolveByName_HierarchicalSurreal exercises the SurrealDB path of
// Store.ResolveByName end-to-end against testcontainers, mirroring the
// MemoryStore unit cases. Sty_e2bfeffa: hierarchical name lookup
// project → workspace → system.
func TestResolveByName_HierarchicalSurreal(t *testing.T) {
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

	docStore := document.NewSurrealStore(conn)
	now := time.Now().UTC()
	wsID, projID := "wksp_resolve", "proj_resolve"

	sys, err := docStore.Upsert(ctx, document.UpsertInput{
		Type:  document.TypeContract,
		Scope: document.ScopeSystem,
		Name:  "develop",
		Body:  []byte("system body"),
		Actor: "test",
	}, now)
	if err != nil {
		t.Fatalf("seed system: %v", err)
	}
	ws, err := docStore.Upsert(ctx, document.UpsertInput{
		Type:        document.TypeContract,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: wsID,
		Name:        "develop",
		Body:        []byte("workspace body"),
		Actor:       "test",
	}, now)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	proj, err := docStore.Upsert(ctx, document.UpsertInput{
		Type:        document.TypeContract,
		Scope:       document.ScopeProject,
		WorkspaceID: wsID,
		ProjectID:   document.StringPtr(projID),
		Name:        "develop",
		Body:        []byte("project body"),
		Actor:       "test",
	}, now)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Project tier wins when projectID is supplied and the caller has
	// the matching workspace membership.
	got, err := docStore.ResolveByName(ctx, document.TypeContract, "develop", wsID, projID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName project: %v", err)
	}
	if got.ID != proj.Document.ID {
		t.Errorf("project tier expected, got id=%q (sys=%q ws=%q proj=%q)", got.ID, sys.Document.ID, ws.Document.ID, proj.Document.ID)
	}

	// Workspace tier wins when no projectID is supplied.
	got, err = docStore.ResolveByName(ctx, document.TypeContract, "develop", wsID, "", []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName workspace: %v", err)
	}
	if got.ID != ws.Document.ID {
		t.Errorf("workspace tier expected, got id=%q", got.ID)
	}

	// System tier wins when neither workspace nor project context is
	// supplied.
	got, err = docStore.ResolveByName(ctx, document.TypeContract, "develop", "", "", nil)
	if err != nil {
		t.Fatalf("ResolveByName system: %v", err)
	}
	if got.ID != sys.Document.ID {
		t.Errorf("system tier expected, got id=%q", got.ID)
	}

	// Non-member memberships hide the workspace + project tiers; the
	// caller falls through to the system row.
	got, err = docStore.ResolveByName(ctx, document.TypeContract, "develop", wsID, projID, []string{"wksp_other"})
	if err != nil {
		t.Fatalf("ResolveByName non-member: %v", err)
	}
	if got.ID != sys.Document.ID {
		t.Errorf("non-member should fall through to system, got id=%q", got.ID)
	}

	// Empty memberships (deny-all on tenant tiers) still reaches system.
	got, err = docStore.ResolveByName(ctx, document.TypeContract, "develop", wsID, projID, []string{})
	if err != nil {
		t.Fatalf("ResolveByName deny-all: %v", err)
	}
	if got.ID != sys.Document.ID {
		t.Errorf("deny-all should fall through to system, got id=%q", got.ID)
	}

	// Missing name returns ErrNotFound.
	if _, err := docStore.ResolveByName(ctx, document.TypeContract, "missing", wsID, projID, []string{wsID}); !errors.Is(err, document.ErrNotFound) {
		t.Errorf("missing name should be ErrNotFound, got %v", err)
	}
}

// TestResolveList_HierarchicalSurreal exercises Store.ResolveList
// against the SurrealDB path end-to-end, mirroring the four
// ResolveByName probes for the list shape. Sty_08196787: hierarchical
// list lookup project → workspace → system.
func TestResolveList_HierarchicalSurreal(t *testing.T) {
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

	docStore := document.NewSurrealStore(conn)
	now := time.Now().UTC()
	wsID, projID := "wksp_resolve_list", "proj_resolve_list"

	sys, err := docStore.Upsert(ctx, document.UpsertInput{
		Type:  document.TypeContract,
		Scope: document.ScopeSystem,
		Name:  "develop",
		Body:  []byte("system body"),
		Actor: "test",
	}, now)
	if err != nil {
		t.Fatalf("seed system: %v", err)
	}
	ws, err := docStore.Upsert(ctx, document.UpsertInput{
		Type:        document.TypeContract,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: wsID,
		Name:        "develop",
		Body:        []byte("workspace body"),
		Actor:       "test",
	}, now)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	proj, err := docStore.Upsert(ctx, document.UpsertInput{
		Type:        document.TypeContract,
		Scope:       document.ScopeProject,
		WorkspaceID: wsID,
		ProjectID:   document.StringPtr(projID),
		Name:        "develop",
		Body:        []byte("project body"),
		Actor:       "test",
	}, now)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Probe 1: project tier wins on (Name, Type) collision when
	// projectID is supplied and the caller has matching membership.
	rows, err := docStore.ResolveList(ctx, document.ListOptions{
		Type: document.TypeContract, ProjectID: projID,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList project: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != proj.Document.ID {
		t.Errorf("project tier should win, got %+v (sys=%q ws=%q proj=%q)", rows, sys.Document.ID, ws.Document.ID, proj.Document.ID)
	}

	// Probe 2: workspace tier wins when no projectID is supplied.
	rows, err = docStore.ResolveList(ctx, document.ListOptions{
		Type: document.TypeContract,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList workspace: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != ws.Document.ID {
		t.Errorf("workspace tier should win, got %+v", rows)
	}

	// Probe 3: system tier alone when no workspace + nil memberships
	// (bootstrap path).
	rows, err = docStore.ResolveList(ctx, document.ListOptions{
		Type: document.TypeContract,
	}, "", nil)
	if err != nil {
		t.Fatalf("ResolveList system: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != sys.Document.ID {
		t.Errorf("system tier alone should win, got %+v", rows)
	}

	// Probe 4: non-member memberships hide the project + workspace
	// tiers; the caller falls through to the system row.
	rows, err = docStore.ResolveList(ctx, document.ListOptions{
		Type: document.TypeContract, ProjectID: projID,
	}, wsID, []string{"wksp_other"})
	if err != nil {
		t.Fatalf("ResolveList non-member: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != sys.Document.ID {
		t.Errorf("non-member should fall through to system, got %+v", rows)
	}
}

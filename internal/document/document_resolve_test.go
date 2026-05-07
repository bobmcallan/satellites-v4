package document

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedTier upserts a row at the requested scope, returning the resulting
// document. Wraps the boilerplate shared across the resolve tests.
func seedTier(t *testing.T, store Store, in UpsertInput, now time.Time) Document {
	t.Helper()
	res, err := store.Upsert(context.Background(), in, now)
	if err != nil {
		t.Fatalf("seed %s/%s: %v", in.Scope, in.Name, err)
	}
	return res.Document
}

// TestResolveByName_ProjectTierWins seeds three rows named "develop" — at
// system, workspace, and project scope — and confirms the project-tier
// row wins.
func TestResolveByName_ProjectTierWins(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID, projID := "wksp_a", "proj_a"

	sys := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop",
		Body: []byte("system body"), Actor: "system",
	}, now)
	ws := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "develop", Body: []byte("workspace body"), Actor: "system",
	}, now)
	proj := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeProject, WorkspaceID: wsID,
		ProjectID: StringPtr(projID), Name: "develop", Body: []byte("project body"), Actor: "system",
	}, now)

	got, err := store.ResolveByName(context.Background(), TypeContract, "develop", wsID, projID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName: %v", err)
	}
	if got.ID != proj.ID {
		t.Errorf("project tier expected, got id=%q (sys=%q ws=%q proj=%q)", got.ID, sys.ID, ws.ID, proj.ID)
	}
	if got.Scope != ScopeProject {
		t.Errorf("scope=%q, want %q", got.Scope, ScopeProject)
	}
}

// TestResolveByName_WorkspaceFallback drops the project-tier row; the
// workspace-tier row must win over the system-tier row.
func TestResolveByName_WorkspaceFallback(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID, projID := "wksp_a", "proj_a"

	sys := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop",
		Body: []byte("system body"), Actor: "system",
	}, now)
	ws := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "develop", Body: []byte("workspace body"), Actor: "system",
	}, now)

	got, err := store.ResolveByName(context.Background(), TypeContract, "develop", wsID, projID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName: %v", err)
	}
	if got.ID != ws.ID {
		t.Errorf("workspace tier expected, got id=%q (sys=%q ws=%q)", got.ID, sys.ID, ws.ID)
	}
	if got.Scope != ScopeWorkspace {
		t.Errorf("scope=%q, want %q", got.Scope, ScopeWorkspace)
	}
}

// TestResolveByName_SystemFallback only seeds a system row; the project +
// workspace tiers must drop through to the system match.
func TestResolveByName_SystemFallback(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID, projID := "wksp_a", "proj_a"

	sys := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "plan",
		Body: []byte("system body"), Actor: "system",
	}, now)

	got, err := store.ResolveByName(context.Background(), TypeContract, "plan", wsID, projID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName: %v", err)
	}
	if got.ID != sys.ID {
		t.Errorf("system tier expected, got id=%q (sys=%q)", got.ID, sys.ID)
	}
	if got.Scope != ScopeSystem {
		t.Errorf("scope=%q, want %q", got.Scope, ScopeSystem)
	}
}

// TestResolveByName_NotFound — none of the three tiers carry a row of
// the requested name; the resolver returns ErrNotFound.
func TestResolveByName_NotFound(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	wsID, projID := "wksp_a", "proj_a"

	_, err := store.ResolveByName(context.Background(), TypeContract, "missing", wsID, projID, []string{wsID})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestResolveByName_TypeFilter seeds two system rows with the same name
// but different types; the resolver must return the type the caller
// asked for. Uses Create (not Upsert) because Upsert's scope-aware
// identity keys on (scope, name) — two same-name same-scope rows
// would collide there.
func TestResolveByName_TypeFilter(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()

	contract, err := store.Create(ctx, Document{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop", Body: "contract body",
	}, now)
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	skill, err := store.Create(ctx, Document{
		Type: TypeSkill, Scope: ScopeSystem, Name: "develop", Body: "skill body",
	}, now)
	if err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	gotSkill, err := store.ResolveByName(ctx, TypeSkill, "develop", "", "", nil)
	if err != nil {
		t.Fatalf("ResolveByName(skill): %v", err)
	}
	if gotSkill.ID != skill.ID {
		t.Errorf("skill tier expected id=%q, got %q (contract=%q)", skill.ID, gotSkill.ID, contract.ID)
	}

	gotContract, err := store.ResolveByName(ctx, TypeContract, "develop", "", "", nil)
	if err != nil {
		t.Fatalf("ResolveByName(contract): %v", err)
	}
	if gotContract.ID != contract.ID {
		t.Errorf("contract tier expected id=%q, got %q (skill=%q)", contract.ID, gotContract.ID, skill.ID)
	}

	// docType="" returns the first match (memory store iteration order is
	// non-deterministic), but it must at minimum be one of the two seeded.
	gotAny, err := store.ResolveByName(ctx, "", "develop", "", "", nil)
	if err != nil {
		t.Fatalf("ResolveByName(any): %v", err)
	}
	if gotAny.ID != contract.ID && gotAny.ID != skill.ID {
		t.Errorf("any-type resolved unexpected id=%q (want %q or %q)", gotAny.ID, contract.ID, skill.ID)
	}
}

// TestResolveByName_MembershipScoping confirms a workspace-tier row in
// workspace W is invisible to a caller whose memberships are [X], so the
// resolver falls through to the system-tier row. The system tier is
// workspace-blind regardless of memberships.
func TestResolveByName_MembershipScoping(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID := "wksp_owner"

	sys := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop",
		Body: []byte("system body"), Actor: "system",
	}, now)
	ws := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "develop", Body: []byte("workspace body"), Actor: "system",
	}, now)

	// Caller in a different workspace — workspace-tier row hidden, system
	// row still resolves.
	got, err := store.ResolveByName(context.Background(), TypeContract, "develop", wsID, "", []string{"wksp_other"})
	if err != nil {
		t.Fatalf("ResolveByName(non-member): %v", err)
	}
	if got.ID != sys.ID {
		t.Errorf("non-member should fall through to system, got id=%q (sys=%q ws=%q)", got.ID, sys.ID, ws.ID)
	}

	// Empty memberships slice (deny-all) still reaches the system tier.
	got, err = store.ResolveByName(context.Background(), TypeContract, "develop", wsID, "", []string{})
	if err != nil {
		t.Fatalf("ResolveByName(deny-all): %v", err)
	}
	if got.ID != sys.ID {
		t.Errorf("deny-all should fall through to system, got id=%q", got.ID)
	}

	// Member of the workspace gets the workspace-tier row.
	got, err = store.ResolveByName(context.Background(), TypeContract, "develop", wsID, "", []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName(member): %v", err)
	}
	if got.ID != ws.ID {
		t.Errorf("member should resolve workspace-tier row, got id=%q", got.ID)
	}

	// nil memberships (bootstrap path) sees the workspace-tier row.
	got, err = store.ResolveByName(context.Background(), TypeContract, "develop", wsID, "", nil)
	if err != nil {
		t.Fatalf("ResolveByName(nil-memberships): %v", err)
	}
	if got.ID != ws.ID {
		t.Errorf("nil memberships should see workspace-tier row, got id=%q", got.ID)
	}
}

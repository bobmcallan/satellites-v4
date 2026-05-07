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

// TestResolveList_PerTierVisibility seeds three rows of type=contract
// at distinct names — one each at the project, workspace, and system
// tier — and confirms ResolveList returns all three for a caller with
// the matching memberships, but only the system row when probed with
// a sibling project id outside the caller's project tier and through
// a different workspace key.
func TestResolveList_PerTierVisibility(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID, projID := "wksp_a", "proj_a"

	projOnly := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeProject, WorkspaceID: wsID,
		ProjectID: StringPtr(projID), Name: "proj_only", Body: []byte("p"), Actor: "system",
	}, now)
	wsOnly := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "wksp_only", Body: []byte("w"), Actor: "system",
	}, now)
	sysOnly := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "sys_only",
		Body: []byte("s"), Actor: "system",
	}, now)

	got, err := store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, ProjectID: projID,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList three-tier: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range got {
		ids[d.ID] = true
	}
	if !ids[projOnly.ID] || !ids[wsOnly.ID] || !ids[sysOnly.ID] {
		t.Errorf("expected all three tiers, got %v (proj=%q wsk=%q sys=%q)", ids, projOnly.ID, wsOnly.ID, sysOnly.ID)
	}

	// Probe with a sibling project id for which the caller has no
	// matching workspace context: project tier yields nothing,
	// workspace tier yields nothing (workspaceID empty), system tier
	// still resolves.
	got, err = store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, ProjectID: "proj_other",
	}, "", []string{"wksp_other"})
	if err != nil {
		t.Fatalf("ResolveList sibling-project: %v", err)
	}
	if len(got) != 1 || got[0].ID != sysOnly.ID {
		t.Errorf("sibling-project probe expected only system row, got %+v", got)
	}
}

// TestResolveList_ProjectWinsOnNameConflict seeds three rows all named
// "develop" of type=contract — at project, workspace, and system. The
// resolver must dedupe by (Name, Type) with project precedence; drop
// each tier in turn and the next-highest must win.
func TestResolveList_ProjectWinsOnNameConflict(t *testing.T) {
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

	rows, err := store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, ProjectID: projID,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList all tiers: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != proj.ID {
		t.Errorf("project should win, got %+v (proj=%q ws=%q sys=%q)", rows, proj.ID, ws.ID, sys.ID)
	}

	if err := store.Delete(context.Background(), proj.ID, DeleteHard, nil); err != nil {
		t.Fatalf("drop project row: %v", err)
	}
	rows, err = store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, ProjectID: projID,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList after dropping project: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != ws.ID {
		t.Errorf("workspace should win after project drop, got %+v", rows)
	}

	if err := store.Delete(context.Background(), ws.ID, DeleteHard, nil); err != nil {
		t.Fatalf("drop workspace row: %v", err)
	}
	rows, err = store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, ProjectID: projID,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList after dropping workspace: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != sys.ID {
		t.Errorf("system should win after workspace drop, got %+v", rows)
	}
}

// TestResolveList_MembershipScoping mirrors TestResolveByName_MembershipScoping
// for the list shape: a workspace-tier row is invisible to non-members,
// the system tier is always reached regardless of memberships, and a
// member sees both.
func TestResolveList_MembershipScoping(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID := "wksp_owner"

	sys := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop_sys",
		Body: []byte("system body"), Actor: "system",
	}, now)
	ws := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "develop_ws", Body: []byte("workspace body"), Actor: "system",
	}, now)

	collect := func(rows []Document) map[string]bool {
		ids := map[string]bool{}
		for _, d := range rows {
			ids[d.ID] = true
		}
		return ids
	}

	rows, err := store.ResolveList(context.Background(), ListOptions{Type: TypeContract}, wsID, []string{"wksp_other"})
	if err != nil {
		t.Fatalf("ResolveList non-member: %v", err)
	}
	got := collect(rows)
	if got[ws.ID] || !got[sys.ID] {
		t.Errorf("non-member should see only system row, got %+v", rows)
	}

	rows, err = store.ResolveList(context.Background(), ListOptions{Type: TypeContract}, wsID, []string{})
	if err != nil {
		t.Fatalf("ResolveList deny-all: %v", err)
	}
	got = collect(rows)
	if got[ws.ID] || !got[sys.ID] {
		t.Errorf("deny-all should see only system row, got %+v", rows)
	}

	rows, err = store.ResolveList(context.Background(), ListOptions{Type: TypeContract}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList member: %v", err)
	}
	got = collect(rows)
	if !got[ws.ID] || !got[sys.ID] {
		t.Errorf("member should see both rows, got %+v", rows)
	}

	rows, err = store.ResolveList(context.Background(), ListOptions{Type: TypeContract}, wsID, nil)
	if err != nil {
		t.Fatalf("ResolveList nil-memberships: %v", err)
	}
	got = collect(rows)
	if !got[ws.ID] || !got[sys.ID] {
		t.Errorf("nil memberships should see both rows, got %+v", rows)
	}
}

// TestResolveList_CrossTierIdentity locks in sty_92271886's cross-tier
// identity invariant for the list shape: same-name rows at different
// scopes are distinct rows, no row leaks across tiers, no row is
// dropped by the deduper.
func TestResolveList_CrossTierIdentity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID, projID := "wksp_a", "proj_a"

	sys := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop",
		Body: []byte("system"), Actor: "system",
	}, now)
	ws := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "develop", Body: []byte("workspace"), Actor: "system",
	}, now)

	rows, err := store.ResolveList(context.Background(), ListOptions{Type: TypeContract}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList no-project: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != ws.ID {
		t.Errorf("workspace should beat system, got %+v (sys=%q ws=%q)", rows, sys.ID, ws.ID)
	}

	proj := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeProject, WorkspaceID: wsID,
		ProjectID: StringPtr(projID), Name: "develop", Body: []byte("project"), Actor: "system",
	}, now)

	rows, err = store.ResolveList(context.Background(), ListOptions{Type: TypeContract, ProjectID: projID}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList all tiers: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != proj.ID {
		t.Errorf("project should beat both tiers, got %+v", rows)
	}

	// All three rows still exist post-resolve — the resolver dedupes,
	// it does not delete.
	if _, err := store.GetByID(context.Background(), sys.ID, nil); err != nil {
		t.Errorf("system row missing after resolve: %v", err)
	}
	if _, err := store.GetByID(context.Background(), ws.ID, nil); err != nil {
		t.Errorf("workspace row missing after resolve: %v", err)
	}
	if _, err := store.GetByID(context.Background(), proj.ID, nil); err != nil {
		t.Errorf("project row missing after resolve: %v", err)
	}
}

// TestResolveList_ScopeFilter confirms opts.Scope acts as a tier
// filter: only the named tier participates in the merge, and the rest
// are skipped.
func TestResolveList_ScopeFilter(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	wsID, projID := "wksp_a", "proj_a"

	projRow := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeProject, WorkspaceID: wsID,
		ProjectID: StringPtr(projID), Name: "proj_only", Body: []byte("p"), Actor: "system",
	}, now)
	wsRow := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeWorkspace, WorkspaceID: wsID,
		Name: "wksp_only", Body: []byte("w"), Actor: "system",
	}, now)
	sysRow := seedTier(t, store, UpsertInput{
		Type: TypeContract, Scope: ScopeSystem, Name: "sys_only",
		Body: []byte("s"), Actor: "system",
	}, now)

	rows, err := store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, Scope: ScopeProject, ProjectID: projID,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList project-only: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != projRow.ID {
		t.Errorf("scope=project should return only project row, got %+v", rows)
	}

	rows, err = store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, Scope: ScopeWorkspace,
	}, wsID, []string{wsID})
	if err != nil {
		t.Fatalf("ResolveList workspace-only: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != wsRow.ID {
		t.Errorf("scope=workspace should return only workspace row, got %+v", rows)
	}

	rows, err = store.ResolveList(context.Background(), ListOptions{
		Type: TypeContract, Scope: ScopeSystem,
	}, "", nil)
	if err != nil {
		t.Fatalf("ResolveList system-only: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != sysRow.ID {
		t.Errorf("scope=system should return only system row, got %+v", rows)
	}
}

// TestResolveList_TypeFilter confirms the (Name, Type) dedupe key
// keeps two same-name same-tier rows of different types both visible
// when opts.Type is unset; setting opts.Type narrows to one.
func TestResolveList_TypeFilter(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()

	contract, err := store.Create(ctx, Document{
		Type: TypeContract, Scope: ScopeSystem, Name: "develop", Body: "contract",
	}, now)
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	skill, err := store.Create(ctx, Document{
		Type: TypeSkill, Scope: ScopeSystem, Name: "develop", Body: "skill",
	}, now)
	if err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	rows, err := store.ResolveList(ctx, ListOptions{Type: TypeContract}, "", nil)
	if err != nil {
		t.Fatalf("ResolveList type=contract: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != contract.ID {
		t.Errorf("type=contract should return only contract, got %+v", rows)
	}

	rows, err = store.ResolveList(ctx, ListOptions{}, "", nil)
	if err != nil {
		t.Fatalf("ResolveList type-empty: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range rows {
		ids[d.ID] = true
	}
	if !ids[contract.ID] || !ids[skill.ID] {
		t.Errorf("type-empty should keep both rows under (Name,Type) dedupe, got %+v", rows)
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

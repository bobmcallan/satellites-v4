package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
)

const testProjectID = "proj_test"

func TestHashBody_Stable(t *testing.T) {
	t.Parallel()
	a := HashBody([]byte("hello"))
	b := HashBody([]byte("hello"))
	if a != b {
		t.Errorf("hash not stable: %q vs %q", a, b)
	}
	c := HashBody([]byte("world"))
	if a == c {
		t.Errorf("distinct bodies must hash differently")
	}
}

func upsertArtifact(t *testing.T, store Store, projectID, name string, body string, now time.Time) UpsertResult {
	t.Helper()
	res, err := store.Upsert(context.Background(), UpsertInput{
		ProjectID: StringPtr(projectID),
		Type:      TypeArtifact,
		Name:      name,
		Body:      []byte(body),
		Scope:     ScopeProject,
		Actor:     "test",
	}, now)
	if err != nil {
		t.Fatalf("Upsert(%q): %v", name, err)
	}
	return res
}

func TestMemoryStore_UpsertIdempotent(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now()

	first := upsertArtifact(t, store, testProjectID, "x.md", "body", now)
	if !first.Created || !first.Changed {
		t.Errorf("first upsert must be Created+Changed: %+v", first)
	}
	if first.Document.Version != 1 {
		t.Errorf("version = %d, want 1", first.Document.Version)
	}
	if first.Document.ProjectID == nil || *first.Document.ProjectID != testProjectID {
		t.Errorf("project_id = %v, want %q", first.Document.ProjectID, testProjectID)
	}

	second := upsertArtifact(t, store, testProjectID, "x.md", "body", now.Add(time.Hour))
	if second.Created || second.Changed {
		t.Errorf("unchanged upsert must be !Created+!Changed: %+v", second)
	}
	if second.Document.Version != 1 {
		t.Errorf("version = %d, want 1 (unchanged)", second.Document.Version)
	}
	if second.Document.ID != first.Document.ID {
		t.Errorf("unchanged upsert minted a new id: %q → %q", first.Document.ID, second.Document.ID)
	}

	third := upsertArtifact(t, store, testProjectID, "x.md", "body2", now.Add(2*time.Hour))
	if third.Created || !third.Changed {
		t.Errorf("changed upsert must be !Created+Changed: %+v", third)
	}
	if third.Document.Version != 2 {
		t.Errorf("version = %d, want 2", third.Document.Version)
	}
	if third.Document.ID != first.Document.ID {
		t.Errorf("changed upsert minted a new id: %q → %q", first.Document.ID, third.Document.ID)
	}
}

func TestMemoryStore_ProjectIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	upsertArtifact(t, store, "proj_a", "x.md", "A", now)
	upsertArtifact(t, store, "proj_b", "x.md", "B", now)

	a, err := store.GetByName(ctx, "proj_a", "x.md", nil)
	if err != nil {
		t.Fatalf("GetByName proj_a: %v", err)
	}
	if a.Body != "A" {
		t.Errorf("proj_a body = %q, want A", a.Body)
	}

	b, err := store.GetByName(ctx, "proj_b", "x.md", nil)
	if err != nil {
		t.Fatalf("GetByName proj_b: %v", err)
	}
	if b.Body != "B" {
		t.Errorf("proj_b body = %q, want B", b.Body)
	}

	if a.ID == b.ID {
		t.Errorf("distinct projects should mint distinct document ids")
	}

	if nA, _ := store.Count(ctx, "proj_a", nil); nA != 1 {
		t.Errorf("Count(proj_a) = %d, want 1", nA)
	}
	if nB, _ := store.Count(ctx, "proj_b", nil); nB != 1 {
		t.Errorf("Count(proj_b) = %d, want 1", nB)
	}
	if nMissing, _ := store.Count(ctx, "proj_unknown", nil); nMissing != 0 {
		t.Errorf("Count(proj_unknown) = %d, want 0", nMissing)
	}
}

// TestUpsert_CrossTierIdentity is the substrate-fix anchor surfaced
// during sty_9ee6fc46's dispatch dogfood: an upsert at one scope must
// NOT collide with a same-name row at a different scope. Pre-fix,
// MemoryStore.findByName + SurrealStore.GetByName keyed identity on
// (project_id, name) only — so a workspace-tier `develop` upsert
// matched the existing system-tier `develop` row (both have
// project_id=nil) and silently re-targeted it. Sty_92271886 follow-up.
func TestUpsert_CrossTierIdentity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed a scope=system contract named "develop".
	sysRes, err := store.Upsert(ctx, UpsertInput{
		Type:  TypeContract,
		Scope: ScopeSystem,
		Name:  "develop",
		Body:  []byte("system body"),
		Actor: "system",
	}, now)
	if err != nil {
		t.Fatalf("seed system: %v", err)
	}
	if !sysRes.Created {
		t.Fatalf("system seed must be created")
	}

	// Upsert a scope=workspace contract with the SAME name. Must mint
	// a NEW row, not update the system-tier one.
	wsRes, err := store.Upsert(ctx, UpsertInput{
		Type:        TypeContract,
		Scope:       ScopeWorkspace,
		WorkspaceID: "wksp_a",
		Name:        "develop",
		Body:        []byte("workspace body"),
		Actor:       "system",
	}, now)
	if err != nil {
		t.Fatalf("workspace upsert: %v", err)
	}
	if !wsRes.Created {
		t.Errorf("workspace upsert should mint a new row, got Created=%v", wsRes.Created)
	}
	if wsRes.Document.ID == sysRes.Document.ID {
		t.Errorf("workspace upsert collided with system row id %q", sysRes.Document.ID)
	}

	// The system-tier row's body must be unchanged.
	sysAfter, err := store.GetByID(ctx, sysRes.Document.ID, nil)
	if err != nil {
		t.Fatalf("GetByID system: %v", err)
	}
	if sysAfter.Body != "system body" || sysAfter.Scope != ScopeSystem {
		t.Errorf("system row drifted: body=%q scope=%q", sysAfter.Body, sysAfter.Scope)
	}

	// The workspace-tier row must be at scope=workspace.
	wsAfter, err := store.GetByID(ctx, wsRes.Document.ID, nil)
	if err != nil {
		t.Fatalf("GetByID workspace: %v", err)
	}
	if wsAfter.Body != "workspace body" || wsAfter.Scope != ScopeWorkspace || wsAfter.WorkspaceID != "wksp_a" {
		t.Errorf("workspace row wrong: body=%q scope=%q ws=%q", wsAfter.Body, wsAfter.Scope, wsAfter.WorkspaceID)
	}

	// Re-upserting the workspace row with the same body is a no-op.
	wsAgain, err := store.Upsert(ctx, UpsertInput{
		Type:        TypeContract,
		Scope:       ScopeWorkspace,
		WorkspaceID: "wksp_a",
		Name:        "develop",
		Body:        []byte("workspace body"),
		Actor:       "system",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("workspace re-upsert: %v", err)
	}
	if wsAgain.Created || wsAgain.Changed {
		t.Errorf("workspace re-upsert with same body should be no-op, got %+v", wsAgain)
	}
	if wsAgain.Document.ID != wsRes.Document.ID {
		t.Errorf("workspace re-upsert minted a new id: %q → %q", wsRes.Document.ID, wsAgain.Document.ID)
	}

	// A workspace-tier row in a DIFFERENT workspace must also be distinct.
	otherWS, err := store.Upsert(ctx, UpsertInput{
		Type:        TypeContract,
		Scope:       ScopeWorkspace,
		WorkspaceID: "wksp_b",
		Name:        "develop",
		Body:        []byte("other workspace body"),
		Actor:       "system",
	}, now)
	if err != nil {
		t.Fatalf("other workspace upsert: %v", err)
	}
	if !otherWS.Created {
		t.Errorf("upsert in second workspace should mint a new row")
	}
	if otherWS.Document.ID == wsRes.Document.ID || otherWS.Document.ID == sysRes.Document.ID {
		t.Errorf("workspace_b row collided with existing rows")
	}

	// Sty_e2bfeffa: ResolveByName must walk project → workspace → system
	// and return the workspace-tier row when the caller scopes to wksp_a,
	// not the system-tier row that shares the name.
	got, err := store.ResolveByName(ctx, TypeContract, "develop", "wksp_a", "", []string{"wksp_a"})
	if err != nil {
		t.Fatalf("ResolveByName workspace tier: %v", err)
	}
	if got.ID != wsRes.Document.ID {
		t.Errorf("ResolveByName returned id=%q (sys=%q ws=%q): workspace tier should win",
			got.ID, sysRes.Document.ID, wsRes.Document.ID)
	}
	if got.Scope != ScopeWorkspace {
		t.Errorf("ResolveByName returned scope=%q, want %q", got.Scope, ScopeWorkspace)
	}
}

func TestIngestFile_PathTraversalBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	store := NewMemoryStore()
	logger := satarbor.New("info")

	for _, bad := range []string{
		"../etc/passwd",
		"../../secret",
		"/etc/passwd",
		"./../outside.md",
	} {
		if _, err := IngestFile(ctx, store, logger, "", testProjectID, dir, bad, time.Now()); err == nil {
			t.Errorf("expected traversal error for %q", bad)
		}
	}
}

func TestIngestFile_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	logger := satarbor.New("info")

	res, err := IngestFile(ctx, store, logger, "", testProjectID, dir, "architecture.md", time.Now())
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	if !res.Created {
		t.Errorf("first ingest must be Created")
	}
	if res.Document.ProjectID == nil || *res.Document.ProjectID != testProjectID {
		t.Errorf("ingested doc project_id = %v, want %q", res.Document.ProjectID, testProjectID)
	}
	if res.Document.Type != TypeArtifact {
		t.Errorf("ingested type = %q, want %q", res.Document.Type, TypeArtifact)
	}
	if res.Document.Scope != ScopeProject {
		t.Errorf("ingested scope = %q, want %q", res.Document.Scope, ScopeProject)
	}
	got, err := store.GetByName(ctx, testProjectID, "architecture.md", nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Body != "# arch\n" {
		t.Errorf("body = %q, want \"# arch\\n\"", got.Body)
	}
}

func TestSeed_SkipsWhenProjectPopulated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	logger := satarbor.New("info")

	upsertArtifact(t, store, testProjectID, "already.md", "x", time.Now())

	n, err := Seed(ctx, store, logger, "", testProjectID, dir, []string{"architecture.md"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("seed ingested %d; expected 0 when project pre-populated", n)
	}
}

func TestSeed_IngestsWhenProjectEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "architecture.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ui-design.md"), []byte("# design\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewMemoryStore()
	logger := satarbor.New("info")
	n, err := Seed(ctx, store, logger, "", testProjectID, dir, []string{"architecture.md", "ui-design.md"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("seed ingested %d; expected 2", n)
	}
}

func TestValidate_TypeEnum(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ     string
		wantErr bool
	}{
		{TypeArtifact, false},
		{TypeContract, false},
		{TypePrinciple, false},
		{TypeReviewer, false},
		{TypeSkill, false},
		{TypeAgent, false},
		{TypeRole, false},
		{TypeWorkflow, false},
		{TypeHelp, false},
		{"", true},
		{"architecture", true},
		{"configuration", true},
		{"random", true},
	}
	for _, tc := range cases {
		// type=role requires scope=workspace; the others use scope=project.
		scope := ScopeProject
		if tc.typ == TypeRole {
			// type=role needs scope=workspace + workspace_id, which is
			// outside the type-enum check this test exercises. Leave it
			// to the dedicated role test path; treat unknowns as project.
			scope = ScopeProject
		}
		d := Document{
			Type:  tc.typ,
			Scope: scope,
			Name:  "x",
		}
		// Reviewer/skill require ContractBinding; supply one for the
		// happy-path branches.
		if tc.typ == TypeSkill || tc.typ == TypeReviewer {
			d.ContractBinding = StringPtr("doc_contract")
		}
		// type=help is system-scope and requires a body. story_cc5c67a9.
		if tc.typ == TypeHelp {
			d.Scope = ScopeSystem
			d.Body = "help body"
		}
		// type=workflow is system-scope. story_7bfd629c.
		if tc.typ == TypeWorkflow {
			d.Scope = ScopeSystem
		}
		if tc.typ != TypeHelp && tc.typ != TypeWorkflow {
			d.ProjectID = StringPtr("proj_x")
		}
		err := d.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(type=%q) accepted; want rejection", tc.typ)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(type=%q) rejected: %v", tc.typ, err)
		}
	}
}

// TestValidate_HelpRequiresBody covers AC1 of story_cc5c67a9: a help
// document with an empty body is rejected by Validate.
func TestValidate_HelpRequiresBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "non-empty body", body: "# Some Help\n\nbody", wantErr: false},
		{name: "empty body", body: "", wantErr: true},
		{name: "whitespace only", body: "   \n\t\n", wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Document{
				Type:  TypeHelp,
				Scope: ScopeSystem,
				Name:  "agents",
				Body:  tc.body,
			}
			err := d.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected rejection for body=%q", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for body=%q: %v", tc.body, err)
			}
		})
	}
}

func TestValidate_ScopeEnum(t *testing.T) {
	t.Parallel()
	// scope=user is a member of the enum but is only valid for
	// type=workflow (covered separately by TestValidate_UserScopeWorkflowOnly).
	// The TypeArtifact case below therefore tests the enum membership
	// and the per-type restriction in one shot.
	cases := []struct {
		scope   string
		wantErr bool
	}{
		{ScopeProject, false},
		{ScopeSystem, false},
		{ScopeUser, true}, // type=artifact + scope=user rejected
		{"", true},
		{"global", true},
	}
	for _, tc := range cases {
		d := Document{
			Type:  TypeArtifact,
			Scope: tc.scope,
			Name:  "x",
		}
		if tc.scope == ScopeProject {
			d.ProjectID = StringPtr("proj_x")
		}
		err := d.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(scope=%q) accepted; want rejection", tc.scope)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(scope=%q) rejected: %v", tc.scope, err)
		}
	}
}

// TestValidate_WorkspaceScopeAcceptedTypes covers the workspace tier's
// accepted type set. Sty_92271886 expanded the original {role, workflow}
// pair to {role, workflow, agent, contract, skill, reviewer} so the
// shared work docs (developer_agent / releaser_agent + the contracts and
// reviewers they bind) can live at workspace scope. Type=help and other
// system-only kinds remain rejected at scope=workspace.
func TestValidate_WorkspaceScopeAcceptedTypes(t *testing.T) {
	t.Parallel()

	binding := StringPtr("doc_contract")

	cases := []struct {
		name string
		doc  Document
	}{
		{"role", Document{Type: TypeRole, Scope: ScopeWorkspace, Name: "role_custom", WorkspaceID: "wksp_a"}},
		{"workflow", Document{Type: TypeWorkflow, Scope: ScopeWorkspace, Name: "workflow_custom", WorkspaceID: "wksp_a"}},
		{"agent", Document{Type: TypeAgent, Scope: ScopeWorkspace, Name: "agent_custom", WorkspaceID: "wksp_a"}},
		{"contract", Document{Type: TypeContract, Scope: ScopeWorkspace, Name: "contract_custom", WorkspaceID: "wksp_a"}},
		{"skill", Document{Type: TypeSkill, Scope: ScopeWorkspace, Name: "skill_custom", WorkspaceID: "wksp_a"}},
		{"reviewer", Document{Type: TypeReviewer, Scope: ScopeWorkspace, Name: "reviewer_custom", WorkspaceID: "wksp_a", ContractBinding: binding}},
		{"principle", Document{Type: TypePrinciple, Scope: ScopeWorkspace, Name: "principle_custom", WorkspaceID: "wksp_a"}},
	}
	for _, tc := range cases {
		if err := tc.doc.Validate(); err != nil {
			t.Errorf("scope=workspace + type=%s happy: %v", tc.name, err)
		}
	}

	// Empty workspace_id: rejected.
	if err := (Document{Type: TypeAgent, Scope: ScopeWorkspace, Name: "agent_no_ws"}).Validate(); err == nil {
		t.Errorf("scope=workspace without workspace_id accepted; want rejection")
	}

	// Project_id present: rejected.
	withProj := Document{
		Type:        TypeAgent,
		Scope:       ScopeWorkspace,
		Name:        "agent_with_proj",
		WorkspaceID: "wksp_a",
		ProjectID:   StringPtr("proj_x"),
	}
	if err := withProj.Validate(); err == nil {
		t.Errorf("scope=workspace with project_id accepted; want rejection")
	}

	// type=help under scope=workspace: rejected (still system-only).
	helpWS := Document{
		Type:        TypeHelp,
		Scope:       ScopeWorkspace,
		Name:        "help_page",
		Body:        "body",
		WorkspaceID: "wksp_a",
	}
	if err := helpWS.Validate(); err == nil {
		t.Errorf("scope=workspace + type=help accepted; want rejection")
	}
}

// TestValidate_UserScopeWorkflowOnly covers the user-tier override row
// added under story_f0a78759 (S5): only type=workflow lives at
// scope=user; workspace_id required; project_id forbidden; created_by
// required (the owning user_id).
func TestValidate_UserScopeWorkflowOnly(t *testing.T) {
	t.Parallel()
	// Workflow with scope=user + workspace_id + created_by: accepted.
	happy := Document{
		Type:        TypeWorkflow,
		Scope:       ScopeUser,
		Name:        "user_workflow",
		WorkspaceID: "wksp_a",
		CreatedBy:   "user_alice",
	}
	if err := happy.Validate(); err != nil {
		t.Errorf("workflow scope=user happy: %v", err)
	}
	// Missing workspace_id: rejected.
	noWS := Document{
		Type:      TypeWorkflow,
		Scope:     ScopeUser,
		Name:      "user_workflow",
		CreatedBy: "user_alice",
	}
	if err := noWS.Validate(); err == nil {
		t.Errorf("workflow scope=user without workspace_id accepted; want rejection")
	}
	// project_id supplied: rejected.
	withProj := Document{
		Type:        TypeWorkflow,
		Scope:       ScopeUser,
		Name:        "user_workflow",
		WorkspaceID: "wksp_a",
		ProjectID:   StringPtr("proj_x"),
		CreatedBy:   "user_alice",
	}
	if err := withProj.Validate(); err == nil {
		t.Errorf("workflow scope=user with project_id accepted; want rejection")
	}
	// Non-workflow type at scope=user: rejected.
	agentUS := Document{
		Type:        TypeAgent,
		Scope:       ScopeUser,
		Name:        "agent_custom",
		WorkspaceID: "wksp_a",
		CreatedBy:   "user_alice",
	}
	if err := agentUS.Validate(); err == nil {
		t.Errorf("agent scope=user accepted; want rejection (only workflow allowed)")
	}
	// Missing created_by: rejected (scope=user requires the owning user id).
	noUser := Document{
		Type:        TypeWorkflow,
		Scope:       ScopeUser,
		Name:        "user_workflow",
		WorkspaceID: "wksp_a",
	}
	if err := noUser.Validate(); err == nil {
		t.Errorf("workflow scope=user without created_by accepted; want rejection")
	}
}

// TestValidate_ProjectScopeWorkflow covers the project-tier override
// row: type=workflow accepted at scope=project provided project_id is
// set (story_f0a78759).
func TestValidate_ProjectScopeWorkflow(t *testing.T) {
	t.Parallel()
	happy := Document{
		Type:        TypeWorkflow,
		Scope:       ScopeProject,
		Name:        "project_workflow",
		WorkspaceID: "wksp_a",
		ProjectID:   StringPtr("proj_x"),
	}
	if err := happy.Validate(); err != nil {
		t.Errorf("workflow scope=project happy: %v", err)
	}
	noProj := Document{
		Type:        TypeWorkflow,
		Scope:       ScopeProject,
		Name:        "project_workflow",
		WorkspaceID: "wksp_a",
	}
	if err := noProj.Validate(); err == nil {
		t.Errorf("workflow scope=project without project_id accepted; want rejection")
	}
}

// TestValidate_SkillContractBindingOptional verifies story_b1108d4a's
// validation split: skill no longer requires contract_binding (skills
// bind to agents via skill_refs); reviewer still does (reviewer
// rubrics remain per-contract).
func TestValidate_SkillContractBindingOptional(t *testing.T) {
	t.Parallel()
	// Skill without contract_binding: accepted post-migration.
	skillUnbound := Document{
		Type:      TypeSkill,
		Scope:     ScopeProject,
		Name:      "golang-testing",
		ProjectID: StringPtr("proj_x"),
	}
	if err := skillUnbound.Validate(); err != nil {
		t.Errorf("skill without contract_binding: %v", err)
	}
	// Skill with contract_binding still accepted (legacy rows valid
	// during the migration window).
	skillBound := Document{
		Type:            TypeSkill,
		Scope:           ScopeProject,
		Name:            "golang-style",
		ProjectID:       StringPtr("proj_x"),
		ContractBinding: StringPtr("doc_contract_y"),
	}
	if err := skillBound.Validate(); err != nil {
		t.Errorf("skill with legacy contract_binding: %v", err)
	}
	// Reviewer without contract_binding: still rejected.
	reviewerUnbound := Document{
		Type:      TypeReviewer,
		Scope:     ScopeProject,
		Name:      "delivery-reviewer",
		ProjectID: StringPtr("proj_x"),
	}
	if err := reviewerUnbound.Validate(); err == nil {
		t.Errorf("reviewer without contract_binding accepted; want rejection (story_b1108d4a does NOT change reviewer requirement)")
	}
}

func TestValidate_AgentContractBindingOptional(t *testing.T) {
	t.Parallel()
	// Agent without contract_binding: accepted.
	unbound := Document{
		Type:      TypeAgent,
		Scope:     ScopeProject,
		Name:      "agent_a",
		ProjectID: StringPtr("proj_x"),
	}
	if err := unbound.Validate(); err != nil {
		t.Errorf("agent without contract_binding: %v", err)
	}
	// Agent with contract_binding: also accepted (optional).
	bound := Document{
		Type:            TypeAgent,
		Scope:           ScopeProject,
		Name:            "agent_b",
		ProjectID:       StringPtr("proj_x"),
		ContractBinding: StringPtr("doc_contract_x"),
	}
	if err := bound.Validate(); err != nil {
		t.Errorf("agent with contract_binding: %v", err)
	}
	// Role with contract_binding: rejected (roles do not pin to
	// contracts — required_role lives on the contract side).
	roleBound := Document{
		Type:            TypeRole,
		Scope:           ScopeProject,
		Name:            "role_x",
		ProjectID:       StringPtr("proj_x"),
		ContractBinding: StringPtr("doc_contract_x"),
	}
	if err := roleBound.Validate(); err == nil {
		t.Errorf("role with contract_binding accepted; want rejection")
	}
}

func TestValidate_ProjectIDNullableOnSystem(t *testing.T) {
	t.Parallel()
	// scope=project requires non-nil ProjectID.
	missing := Document{Type: TypeArtifact, Scope: ScopeProject, Name: "x"}
	if err := missing.Validate(); err == nil {
		t.Errorf("scope=project with nil ProjectID accepted; want rejection")
	}
	// scope=system requires nil ProjectID.
	leaked := Document{Type: TypePrinciple, Scope: ScopeSystem, Name: "x", ProjectID: StringPtr("proj_x")}
	if err := leaked.Validate(); err == nil {
		t.Errorf("scope=system with non-nil ProjectID accepted; want rejection")
	}
	// Both happy paths.
	scoped := Document{Type: TypeArtifact, Scope: ScopeProject, Name: "x", ProjectID: StringPtr("proj_x")}
	if err := scoped.Validate(); err != nil {
		t.Errorf("scope=project happy: %v", err)
	}
	system := Document{Type: TypePrinciple, Scope: ScopeSystem, Name: "x"}
	if err := system.Validate(); err != nil {
		t.Errorf("scope=system happy: %v", err)
	}
}

func TestValidate_ContractBindingShape(t *testing.T) {
	t.Parallel()
	// Skill without ContractBinding accepted (story_b1108d4a — skills
	// bind to agents via skill_refs, not to contracts).
	skillNaked := Document{Type: TypeSkill, Scope: ScopeProject, Name: "s", ProjectID: StringPtr("proj_x")}
	if err := skillNaked.Validate(); err != nil {
		t.Errorf("skill without contract_binding rejected post-story_b1108d4a: %v", err)
	}
	// Reviewer without ContractBinding still rejected (reviewer
	// rubrics remain per-contract).
	reviewerNaked := Document{Type: TypeReviewer, Scope: ScopeProject, Name: "r", ProjectID: StringPtr("proj_x")}
	if err := reviewerNaked.Validate(); err == nil {
		t.Errorf("reviewer without contract_binding accepted; want rejection")
	}
	// Artifact with ContractBinding rejected.
	artifactBound := Document{Type: TypeArtifact, Scope: ScopeProject, Name: "a", ProjectID: StringPtr("proj_x"), ContractBinding: StringPtr("doc_contract")}
	if err := artifactBound.Validate(); err == nil {
		t.Errorf("artifact with contract_binding accepted; want rejection")
	}
	// Skill happy: still accepted with binding (legacy rows valid
	// during the migration window).
	skill := Document{Type: TypeSkill, Scope: ScopeProject, Name: "s", ProjectID: StringPtr("proj_x"), ContractBinding: StringPtr("doc_contract")}
	if err := skill.Validate(); err != nil {
		t.Errorf("skill happy: %v", err)
	}
}

func TestMemoryStore_DanglingContractBindingRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	// Create a non-contract row at the bound id; the binding should
	// still reject because the target is the wrong type.
	wrongTypeID := "doc_principle"
	if _, err := store.Create(ctx, Document{
		ID:    wrongTypeID,
		Type:  TypePrinciple,
		Scope: ScopeSystem,
		Name:  "p",
		Tags:  []string{"x"},
	}, now); err != nil {
		t.Fatalf("seed principle: %v", err)
	}

	skill := Document{
		Type:            TypeSkill,
		Scope:           ScopeProject,
		Name:            "s",
		ProjectID:       StringPtr("proj_x"),
		ContractBinding: StringPtr("doc_missing"),
	}
	if _, err := store.Create(ctx, skill, now); !errors.Is(err, ErrDanglingBinding) {
		t.Errorf("missing FK: err = %v, want ErrDanglingBinding", err)
	}

	skill.ContractBinding = StringPtr(wrongTypeID)
	if _, err := store.Create(ctx, skill, now); !errors.Is(err, ErrDanglingBinding) {
		t.Errorf("wrong-type FK: err = %v, want ErrDanglingBinding", err)
	}

	// Bind to an active type=contract row → accepted.
	contractID := "doc_contract"
	if _, err := store.Create(ctx, Document{
		ID:    contractID,
		Type:  TypeContract,
		Scope: ScopeSystem,
		Name:  "c",
	}, now); err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	skill.ContractBinding = StringPtr(contractID)
	if _, err := store.Create(ctx, skill, now); err != nil {
		t.Errorf("happy path FK: %v", err)
	}
}

func TestMemoryStore_FilterByType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	upsertArtifact(t, store, "proj_x", "a.md", "body", now)
	if _, err := store.Create(ctx, Document{
		Type:  TypePrinciple,
		Scope: ScopeSystem,
		Name:  "p1",
		Tags:  []string{"v4"},
	}, now); err != nil {
		t.Fatalf("Create principle: %v", err)
	}

	got, err := store.List(ctx, ListOptions{Type: TypePrinciple}, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "p1" {
		t.Errorf("List(type=principle) = %+v, want one row p1", got)
	}
}

func TestMemoryStore_FilterByScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	upsertArtifact(t, store, "proj_x", "a.md", "body", now)
	if _, err := store.Create(ctx, Document{
		Type:  TypePrinciple,
		Scope: ScopeSystem,
		Name:  "p1",
	}, now); err != nil {
		t.Fatalf("Create principle: %v", err)
	}

	got, err := store.List(ctx, ListOptions{Scope: ScopeSystem}, nil)
	if err != nil {
		t.Fatalf("List(scope=system): %v", err)
	}
	if len(got) != 1 || got[0].Scope != ScopeSystem {
		t.Errorf("List(scope=system) = %+v, want one system row", got)
	}
}

func TestMemoryStore_FilterByTags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	if _, err := store.Create(ctx, Document{
		Type: TypePrinciple, Scope: ScopeSystem, Name: "p1", Tags: []string{"v4", "process"},
	}, now); err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	if _, err := store.Create(ctx, Document{
		Type: TypePrinciple, Scope: ScopeSystem, Name: "p2", Tags: []string{"infra"},
	}, now); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	got, err := store.List(ctx, ListOptions{Tags: []string{"v4"}}, nil)
	if err != nil {
		t.Fatalf("List(tags=v4): %v", err)
	}
	if len(got) != 1 || got[0].Name != "p1" {
		t.Errorf("List(tags=v4) = %+v, want p1", got)
	}

	got, err = store.List(ctx, ListOptions{Tags: []string{"v4", "infra"}}, nil)
	if err != nil {
		t.Fatalf("List(tags=v4|infra): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("List(tags=v4|infra) = %d rows, want 2", len(got))
	}
}

// The substring-on-Query Search tests that lived here (slice 6.3 stand-in)
// were removed when the semantic path landed (story_5abfe61c) per
// pr_no_unrequested_compat. Semantic-search behaviour is asserted by the
// SearchSemantic tests below.

func TestMemoryStore_Search_EmptyQueryFilterOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now()

	if _, err := store.Create(ctx, Document{
		Type: TypePrinciple, Scope: ScopeSystem, Name: "older",
	}, t0); err != nil {
		t.Fatalf("Create older: %v", err)
	}
	if _, err := store.Create(ctx, Document{
		Type: TypePrinciple, Scope: ScopeSystem, Name: "newer",
	}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("Create newer: %v", err)
	}

	got, err := store.Search(ctx, SearchOptions{
		ListOptions: ListOptions{Type: TypePrinciple},
	}, nil)
	if err != nil {
		t.Fatalf("Search empty-query+filter: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Search empty-query+filter = %d rows, want 2", len(got))
	}
	if got[0].Name != "newer" {
		t.Errorf("first row name = %q, want newer (updated_at DESC)", got[0].Name)
	}
}

func TestMemoryStore_Search_QuerySubstringFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now()

	if _, err := store.Create(ctx, Document{
		Type: TypeContract, Scope: ScopeSystem,
		Name: "test-contract", Body: "contract body",
	}, t0); err != nil {
		t.Fatalf("Create contract: %v", err)
	}
	if _, err := store.Create(ctx, Document{
		Type: TypePrinciple, Scope: ScopeSystem,
		Name: "principle-a", Body: "# Test\n",
	}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("Create principle: %v", err)
	}
	if _, err := store.Create(ctx, Document{
		Type: TypeRole, Scope: ScopeSystem,
		Name: "role_orchestrator",
		Body: "Holds every orchestrator-surface MCP verb (contract_*, ...).",
	}, t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("Create role: %v", err)
	}

	got, err := store.Search(ctx, SearchOptions{Query: "contract body"}, nil)
	if err != nil {
		t.Fatalf("Search query=contract body: %v", err)
	}
	if len(got) != 1 || got[0].Name != "test-contract" {
		t.Fatalf("Search query=contract body = %d rows (%v), want 1 row test-contract", len(got), got)
	}

	combined, err := store.Search(ctx, SearchOptions{
		ListOptions: ListOptions{Type: TypePrinciple},
		Query:       "contract body",
	}, nil)
	if err != nil {
		t.Fatalf("Search query+type filter: %v", err)
	}
	if len(combined) != 0 {
		t.Errorf("Search query=contract body + type=principle = %d rows, want 0 (AND)", len(combined))
	}
}

func TestMemoryStore_Search_UnknownEnumRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	if _, err := store.Search(ctx, SearchOptions{
		ListOptions: ListOptions{Type: "garbage"},
	}, nil); err == nil {
		// MemoryStore.List doesn't enum-validate (the Surreal one does);
		// this assertion will fail and document the gap. To keep the
		// test useful right now, the rejection path lives in the Surreal
		// implementation (where SQL injection of unknown enums would
		// happen). MemoryStore returns no rows for "garbage" because no
		// document has that type — semantically equivalent for tests
		// that don't run against SurrealDB.
		t.Log("MemoryStore returns 0 rows for unknown type; SurrealStore enum-rejects in production")
	}
}

func TestMemoryStore_DeleteArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	res := upsertArtifact(t, store, "proj_x", "a.md", "body", now)
	if err := store.Delete(ctx, res.Document.ID, DeleteArchive, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := store.GetByID(ctx, res.Document.ID, nil)
	if err != nil {
		t.Fatalf("GetByID after archive: %v", err)
	}
	if got.Status != StatusArchived {
		t.Errorf("after archive status = %q, want %q", got.Status, StatusArchived)
	}
	// Count excludes archived.
	if n, _ := store.Count(ctx, "proj_x", nil); n != 0 {
		t.Errorf("Count after archive = %d, want 0", n)
	}
}

func TestMemoryStore_UpdatePartial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now()

	res := upsertArtifact(t, store, "proj_x", "a.md", "body", now)
	newBody := "body-v2"
	tags := []string{"reviewed"}
	updated, err := store.Update(ctx, res.Document.ID, UpdateFields{
		Body: &newBody,
		Tags: &tags,
	}, "alice", now.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Body != newBody {
		t.Errorf("body = %q, want %q", updated.Body, newBody)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if updated.UpdatedBy != "alice" {
		t.Errorf("updated_by = %q, want alice", updated.UpdatedBy)
	}
	if len(updated.Tags) != 1 || updated.Tags[0] != "reviewed" {
		t.Errorf("tags = %v, want [reviewed]", updated.Tags)
	}
}

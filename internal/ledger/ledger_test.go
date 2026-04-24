package ledger

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewID_Format(t *testing.T) {
	t.Parallel()
	id := NewID()
	if !strings.HasPrefix(id, "ldg_") || len(id) != len("ldg_")+8 {
		t.Errorf("id %q has wrong shape", id)
	}
	if NewID() == id {
		t.Error("NewID must mint unique ids")
	}
}

// TestStoreInterface_Surface pins the Store surface so a future addition
// (e.g. a hard-delete verb) is forced to update this test and the
// compile-time `var _ Store = ...` assertions in store.go / surreal.go.
//
// Append is the sole creation path; Dereference is the sole status
// mutation (writes a new audit row + flips the target's Status). No
// hard-delete or arbitrary-update verb exists.
func TestStoreInterface_Surface(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"Append": true, "GetByID": true, "List": true, "Search": true,
		"Recall": true, "Dereference": true, "BackfillWorkspaceID": true,
	}
	typ := reflect.TypeOf((*Store)(nil)).Elem()
	if typ.NumMethod() != len(want) {
		t.Fatalf("Store declares %d methods; want exactly %d (%v)", typ.NumMethod(), len(want), want)
	}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i).Name
		if !want[m] {
			t.Errorf("unexpected method on Store: %q", m)
		}
	}
}

func TestMemoryStore_AppendStampsIDAndTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	e, err := store.Append(ctx, LedgerEntry{
		ProjectID: "proj_a",
		Type:      TypeDecision,
		Content:   "hello",
		CreatedBy: "u_1",
	}, now)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !strings.HasPrefix(e.ID, "ldg_") {
		t.Errorf("id %q not stamped", e.ID)
	}
	if !e.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", e.CreatedAt, now)
	}
	if e.ProjectID != "proj_a" || e.Type != TypeDecision || e.Content != "hello" || e.CreatedBy != "u_1" {
		t.Errorf("fields round-trip mismatch: %+v", e)
	}
	if e.Durability != DurabilityDurable || e.SourceType != SourceAgent || e.Status != StatusActive {
		t.Errorf("defaults missing: %+v", e)
	}
}

func TestMemoryStore_ListNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypePlan, CreatedBy: "u_1"}, t0)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeArtifact, CreatedBy: "u_1"}, t0.Add(time.Hour))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeEvidence, CreatedBy: "u_1"}, t0.Add(2*time.Hour))

	got, err := store.List(ctx, "proj_a", ListOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Type != TypeEvidence || got[1].Type != TypeArtifact || got[2].Type != TypePlan {
		t.Errorf("unexpected order: %v", []string{got[0].Type, got[1].Type, got[2].Type})
	}
}

func TestMemoryStore_ListTypeFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision}, now.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeEvidence}, now.Add(2*time.Second))

	got, _ := store.List(ctx, "proj_a", ListOptions{Type: TypeDecision}, nil)
	if len(got) != 2 {
		t.Errorf("type filter returned %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Type != TypeDecision {
			t.Errorf("leaked %q", e.Type)
		}
	}
}

func TestMemoryStore_ListLimitClamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	for i := 0; i < 600; i++ {
		_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision}, now.Add(time.Duration(i)*time.Microsecond))
	}

	got, _ := store.List(ctx, "proj_a", ListOptions{}, nil)
	if len(got) != DefaultListLimit {
		t.Errorf("default limit returned %d, want %d", len(got), DefaultListLimit)
	}
	got, _ = store.List(ctx, "proj_a", ListOptions{Limit: 2}, nil)
	if len(got) != 2 {
		t.Errorf("limit 2 returned %d, want 2", len(got))
	}
	got, _ = store.List(ctx, "proj_a", ListOptions{Limit: 9999}, nil)
	if len(got) != MaxListLimit {
		t.Errorf("ceiling clamp returned %d, want %d", len(got), MaxListLimit)
	}
}

func TestMemoryStore_ProjectIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_b", Type: TypeDecision}, now)

	a, _ := store.List(ctx, "proj_a", ListOptions{}, nil)
	b, _ := store.List(ctx, "proj_b", ListOptions{}, nil)
	c, _ := store.List(ctx, "proj_missing", ListOptions{}, nil)
	if len(a) != 1 || len(b) != 1 {
		t.Errorf("per-project counts wrong: a=%d b=%d", len(a), len(b))
	}
	if len(c) != 0 {
		t.Errorf("missing project should return empty, got %d", len(c))
	}
}

func TestValidate_TypeEnum(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{TypePlan, TypeActionClaim, TypeArtifact, TypeEvidence, TypeDecision, TypeCloseRequest, TypeVerdict, TypeWorkflowClaim, TypeKV} {
		e := LedgerEntry{Type: typ, Durability: DurabilityDurable, SourceType: SourceAgent, Status: StatusActive}
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(type=%q) rejected: %v", typ, err)
		}
	}
	for _, bad := range []string{"", "story.status_change", "garbage"} {
		e := LedgerEntry{Type: bad, Durability: DurabilityDurable, SourceType: SourceAgent, Status: StatusActive}
		if err := e.Validate(); err == nil {
			t.Errorf("Validate(type=%q) accepted; want rejection", bad)
		}
	}
}

func TestValidate_DurabilityEnum(t *testing.T) {
	t.Parallel()
	for _, dur := range []string{DurabilityPipeline, DurabilityDurable} {
		e := LedgerEntry{Type: TypeDecision, Durability: dur, SourceType: SourceAgent, Status: StatusActive}
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(durability=%q) rejected: %v", dur, err)
		}
	}
	for _, bad := range []string{"", "permanent"} {
		e := LedgerEntry{Type: TypeDecision, Durability: bad, SourceType: SourceAgent, Status: StatusActive}
		if err := e.Validate(); err == nil {
			t.Errorf("Validate(durability=%q) accepted; want rejection", bad)
		}
	}
}

func TestValidate_ExpiresAtRequiredWhenEphemeral(t *testing.T) {
	t.Parallel()
	naked := LedgerEntry{Type: TypeDecision, Durability: DurabilityEphemeral, SourceType: SourceAgent, Status: StatusActive}
	if err := naked.Validate(); err == nil {
		t.Error("ephemeral without expires_at accepted; want rejection")
	}
	expiry := time.Now().Add(time.Hour)
	ok := naked
	ok.ExpiresAt = &expiry
	if err := ok.Validate(); err != nil {
		t.Errorf("ephemeral with expires_at rejected: %v", err)
	}
	leaked := LedgerEntry{Type: TypeDecision, Durability: DurabilityDurable, SourceType: SourceAgent, Status: StatusActive, ExpiresAt: &expiry}
	if err := leaked.Validate(); err == nil {
		t.Error("durable with expires_at accepted; want rejection")
	}
}

func TestValidate_SourceTypeEnum(t *testing.T) {
	t.Parallel()
	for _, src := range []string{SourceManifest, SourceFeedback, SourceAgent, SourceUser, SourceSystem, SourceMigration} {
		e := LedgerEntry{Type: TypeDecision, Durability: DurabilityDurable, SourceType: src, Status: StatusActive}
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(source_type=%q) rejected: %v", src, err)
		}
	}
	for _, bad := range []string{"", "robot"} {
		e := LedgerEntry{Type: TypeDecision, Durability: DurabilityDurable, SourceType: bad, Status: StatusActive}
		if err := e.Validate(); err == nil {
			t.Errorf("Validate(source_type=%q) accepted; want rejection", bad)
		}
	}
}

func TestMemoryStore_Append_RejectsInvalidEnum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: "garbage"}, time.Now()); err == nil {
		t.Error("Append accepted bogus type; want rejection")
	}
}

func TestMemoryStore_GetByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	written, _ := store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, CreatedBy: "u_1"}, now)
	got, err := store.GetByID(ctx, written.ID, nil)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != written.ID {
		t.Errorf("got id %q, want %q", got.ID, written.ID)
	}
	if _, err := store.GetByID(ctx, "ldg_missing", nil); err == nil {
		t.Error("GetByID(missing) accepted; want ErrNotFound")
	}
}

func TestMemoryStore_FilterByStoryAndContractAndTags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	storyA := "story_a"
	contractZ := "ci_z"
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, StoryID: &storyA, Tags: []string{"phase:plan"}}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, ContractID: &contractZ, Tags: []string{"phase:develop"}}, now.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"phase:plan", "phase:develop"}}, now.Add(2*time.Second))

	if got, _ := store.List(ctx, "proj_a", ListOptions{StoryID: storyA}, nil); len(got) != 1 {
		t.Errorf("StoryID filter = %d, want 1", len(got))
	}
	if got, _ := store.List(ctx, "proj_a", ListOptions{ContractID: contractZ}, nil); len(got) != 1 {
		t.Errorf("ContractID filter = %d, want 1", len(got))
	}
	if got, _ := store.List(ctx, "proj_a", ListOptions{Tags: []string{"phase:plan"}}, nil); len(got) != 2 {
		t.Errorf("Tags filter = %d, want 2", len(got))
	}
}

func TestMemoryStore_Search_QueryAndFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Content: "plan-shipped"}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeArtifact, Content: "PLAN-shipped"}, now.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Content: "unrelated"}, now.Add(2*time.Second))

	got, _ := store.Search(ctx, "proj_a", SearchOptions{
		ListOptions: ListOptions{Type: TypeDecision},
		Query:       "plan",
	}, nil)
	if len(got) != 1 || got[0].Content != "plan-shipped" {
		t.Errorf("Search(type=decision, query=plan) = %+v", got)
	}

	// Empty query + filter: returns ordered by CreatedAt DESC.
	allDecisions, _ := store.Search(ctx, "proj_a", SearchOptions{
		ListOptions: ListOptions{Type: TypeDecision},
	}, nil)
	if len(allDecisions) != 2 {
		t.Errorf("empty-query+filter = %d, want 2", len(allDecisions))
	}
}

func TestMemoryStore_Recall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	root, _ := store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision}, now)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeEvidence, Tags: []string{"recall_root:" + root.ID}}, now.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeArtifact, Tags: []string{"recall_root:" + root.ID}}, now.Add(2*time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"recall_root:other"}}, now.Add(3*time.Second))

	chain, err := store.Recall(ctx, root.ID, nil)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(chain) != 3 {
		t.Errorf("chain length = %d, want 3", len(chain))
	}
	for i := 1; i < len(chain); i++ {
		if !chain[i-1].CreatedAt.Before(chain[i].CreatedAt) && !chain[i-1].CreatedAt.Equal(chain[i].CreatedAt) {
			t.Errorf("chain not CreatedAt ASC: %v", chain)
		}
	}
}

func TestMemoryStore_Dereference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	target, _ := store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Content: "old plan"}, now)

	audit, err := store.Dereference(ctx, target.ID, "superseded by new plan", "u_alice", now.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("Dereference: %v", err)
	}
	if audit.Type != TypeDecision {
		t.Errorf("audit type = %q, want decision", audit.Type)
	}
	if audit.Content != "superseded by new plan" {
		t.Errorf("audit content = %q", audit.Content)
	}
	hasKindTag, hasTargetTag := false, false
	for _, t := range audit.Tags {
		if t == "kind:dereference" {
			hasKindTag = true
		}
		if t == "target:"+target.ID {
			hasTargetTag = true
		}
	}
	if !hasKindTag || !hasTargetTag {
		t.Errorf("audit tags missing kind:dereference or target: %v", audit.Tags)
	}
	// Target row is now status=dereferenced; default list excludes it.
	listed, _ := store.List(ctx, "proj_a", ListOptions{}, nil)
	for _, e := range listed {
		if e.ID == target.ID {
			t.Errorf("default list still includes dereferenced row %q", target.ID)
		}
	}
	// Explicit status filter returns it.
	derefd, _ := store.List(ctx, "proj_a", ListOptions{Status: StatusDereferenced}, nil)
	if len(derefd) != 1 || derefd[0].ID != target.ID {
		t.Errorf("explicit status filter = %+v, want target", derefd)
	}
}

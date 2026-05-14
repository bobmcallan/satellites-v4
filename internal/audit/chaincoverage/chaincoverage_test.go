package chaincoverage

import (
	"context"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestCanonicalContracts asserts the canonical chain shape order
// matches the system-tier default workflow's Shape prose: plan →
// develop → commit → merge_to_main → story_review. Order is part
// of the API; changes here must move with the workflow body.
func TestCanonicalContracts(t *testing.T) {
	got := CanonicalContracts()
	want := []string{"plan", "develop", "commit", "merge_to_main", "story_review"}
	if len(got) != len(want) {
		t.Fatalf("len(canonical) = %d, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("canonical[%d] = %q, want %q", i, got[i], name)
		}
	}
}

// TestIsCanonicalContractAction covers the canonical-action lint
// the audit verb (sty_2f0db922) consumes via this shared package.
func TestIsCanonicalContractAction(t *testing.T) {
	if !IsCanonicalContractAction("contract:commit") {
		t.Errorf("contract:commit must be canonical")
	}
	if IsCanonicalContractAction("contract:push") {
		t.Errorf("contract:push must NOT be canonical (renamed to commit)")
	}
	if IsCanonicalContractAction("contract:story_close") {
		t.Errorf("contract:story_close must NOT be canonical (replaced by mechanical verb)")
	}
}

// TestResolve_MissingContractSurfaces ensures the loop surfaces names
// that did not resolve at any tier. Wire-shape regression for the
// satellites_init chain_coverage field.
func TestResolve_MissingContractSurfaces(t *testing.T) {
	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	// Seed only `plan` at the system tier; everything else is missing.
	_, err := docs.Create(ctx, document.Document{
		Type: document.TypeContract, Scope: document.ScopeSystem,
		Name: "plan", Body: "stub", Status: document.StatusActive,
	}, now)
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	res, err := Resolve(ctx, docs, "wksp_x", "", []string{"wksp_x"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.MissingContracts) == 0 {
		t.Fatalf("expected missing contracts, got none (res=%+v)", res)
	}
	wantMissing := map[string]bool{"develop": true, "commit": true, "merge_to_main": true, "story_review": true}
	for _, n := range res.MissingContracts {
		if !wantMissing[n] {
			t.Errorf("unexpected missing entry %q", n)
		}
		delete(wantMissing, n)
	}
	for n := range wantMissing {
		t.Errorf("expected %q in missing_contracts", n)
	}
	if got := RecommendationText("wksp_x", res); got == "" {
		t.Errorf("RecommendationText must produce non-empty prose on missing contracts")
	}
}

// TestResolve_AllPresent — no missing entries when every chain
// contract resolves; recommendation text reflects the clean state.
func TestResolve_AllPresent(t *testing.T) {
	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	for _, name := range CanonicalContracts() {
		_, err := docs.Create(ctx, document.Document{
			Type: document.TypeContract, Scope: document.ScopeSystem,
			Name: name, Body: "stub", Status: document.StatusActive,
		}, now)
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	res, err := Resolve(ctx, docs, "wksp_x", "", []string{"wksp_x"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.MissingContracts) != 0 {
		t.Errorf("expected zero missing contracts, got %+v", res.MissingContracts)
	}
	rec := RecommendationText("wksp_x", res)
	if rec == "" {
		t.Errorf("RecommendationText must be non-empty")
	}
}

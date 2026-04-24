package ledger

import (
	"context"
	"testing"
	"time"
)

func TestKVProjection_LatestPerKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now().UTC()

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeKV, Tags: []string{"key:active_branch"}, Content: "main"}, t0)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeKV, Tags: []string{"key:active_branch"}, Content: "feature/x"}, t0.Add(time.Hour))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeKV, Tags: []string{"key:max_retries"}, Content: "5"}, t0.Add(2*time.Hour))

	kv, err := KVProjection(ctx, store, "proj_a", nil)
	if err != nil {
		t.Fatalf("KVProjection: %v", err)
	}
	if len(kv) != 2 {
		t.Fatalf("len = %d, want 2", len(kv))
	}
	if kv["active_branch"].Value != "feature/x" {
		t.Errorf("active_branch = %q, want feature/x (newest wins)", kv["active_branch"].Value)
	}
	if kv["max_retries"].Value != "5" {
		t.Errorf("max_retries = %q, want 5", kv["max_retries"].Value)
	}
}

func TestKVProjection_RederivedAfterMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now().UTC()

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeKV, Tags: []string{"key:foo"}, Content: "v1"}, t0)
	first, _ := KVProjection(ctx, store, "proj_a", nil)
	if first["foo"].Value != "v1" {
		t.Fatalf("first projection foo = %q, want v1", first["foo"].Value)
	}

	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeKV, Tags: []string{"key:foo"}, Content: "v2"}, t0.Add(time.Hour))
	second, _ := KVProjection(ctx, store, "proj_a", nil)
	if second["foo"].Value != "v2" {
		t.Errorf("second projection foo = %q, want v2 (re-derived)", second["foo"].Value)
	}
}

func TestStoryTimeline_AscendingByCreatedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now().UTC()
	storyID := "sty_1"
	otherStory := "sty_2"

	// Insert out of order on purpose; derivation must sort ASC.
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, StoryID: &storyID, Content: "c"}, t0.Add(2*time.Hour))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, StoryID: &storyID, Content: "a"}, t0)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, StoryID: &storyID, Content: "b"}, t0.Add(time.Hour))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, StoryID: &otherStory, Content: "other"}, t0.Add(30*time.Minute))

	timeline, err := StoryTimeline(ctx, store, storyID, nil)
	if err != nil {
		t.Fatalf("StoryTimeline: %v", err)
	}
	if len(timeline) != 3 {
		t.Fatalf("len = %d, want 3 (other story excluded)", len(timeline))
	}
	if timeline[0].Content != "a" || timeline[1].Content != "b" || timeline[2].Content != "c" {
		t.Errorf("timeline order = %v, want a,b,c", []string{timeline[0].Content, timeline[1].Content, timeline[2].Content})
	}
}

func TestCostRollup_SumsLLMUsageOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now().UTC()

	// Two llm-usage rows with structured cost; one llm-usage row with
	// invalid JSON; one untagged row that must be excluded.
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"kind:llm-usage"}, Structured: []byte(`{"cost_usd":0.012,"input_tokens":1000,"output_tokens":500}`)}, t0)
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"kind:llm-usage"}, Structured: []byte(`{"cost_usd":0.034,"input_tokens":2500,"output_tokens":750}`)}, t0.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"kind:llm-usage"}, Structured: []byte(`not-json`)}, t0.Add(2*time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"kind:other"}, Structured: []byte(`{"cost_usd":99.0}`)}, t0.Add(3*time.Second))

	summary, err := CostRollup(ctx, store, "proj_a", nil)
	if err != nil {
		t.Fatalf("CostRollup: %v", err)
	}
	if summary.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3 (other-tagged row excluded)", summary.RowCount)
	}
	if summary.SkippedRows != 1 {
		t.Errorf("SkippedRows = %d, want 1 (invalid JSON)", summary.SkippedRows)
	}
	wantCost := 0.012 + 0.034
	if summary.CostUSD < wantCost-1e-9 || summary.CostUSD > wantCost+1e-9 {
		t.Errorf("CostUSD = %v, want %v", summary.CostUSD, wantCost)
	}
	if summary.InputTokens != 3500 {
		t.Errorf("InputTokens = %d, want 3500", summary.InputTokens)
	}
	if summary.OutputTokens != 1250 {
		t.Errorf("OutputTokens = %d, want 1250", summary.OutputTokens)
	}
}

func TestDerivations_ReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	t0 := time.Now().UTC()

	// Seed a few rows.
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeKV, Tags: []string{"key:foo"}, Content: "v"}, t0)
	storyID := "sty_x"
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, StoryID: &storyID, Content: "evt"}, t0.Add(time.Second))
	_, _ = store.Append(ctx, LedgerEntry{ProjectID: "proj_a", Type: TypeDecision, Tags: []string{"kind:llm-usage"}, Structured: []byte(`{"cost_usd":1}`)}, t0.Add(2*time.Second))

	before, _ := store.List(ctx, "proj_a", ListOptions{Limit: 500, IncludeDerefd: true}, nil)
	beforeCount := len(before)

	if _, err := KVProjection(ctx, store, "proj_a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := StoryTimeline(ctx, store, storyID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CostRollup(ctx, store, "proj_a", nil); err != nil {
		t.Fatal(err)
	}

	after, _ := store.List(ctx, "proj_a", ListOptions{Limit: 500, IncludeDerefd: true}, nil)
	if len(after) != beforeCount {
		t.Errorf("derivations wrote rows: before=%d after=%d", beforeCount, len(after))
	}
}

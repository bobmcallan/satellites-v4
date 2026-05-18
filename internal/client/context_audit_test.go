// Unit tests for internal/client/context_audit.go (sty_9f658001 slice 1).
//
// Covers:
//   - TestR1Matcher — regex matches proj_*/sty_*/wksp_*/task_* literals.
//   - TestR1Evaluator_SystemTierWithRefs — system-tier section with embedded
//     refs returns audit:r1_fail.
//   - TestR1Evaluator_SystemTierClean — clean system-tier section returns
//     audit:r1_pass.
//   - TestR1Evaluator_WorkspaceTierWithRefs — workspace-tier section with
//     embedded refs is recorded but does NOT fire R1.
//   - TestEmitContextFetch_EndToEnd — emitContextFetch writes one row;
//     FlushContextAudit blocks until the worker is idle.
package client

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

func TestR1Matcher(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{name: "story_id", body: "see sty_4db0e025 for context", want: []string{"sty_4db0e025"}},
		{name: "project_id", body: "proj_7a62aedb owns this", want: []string{"proj_7a62aedb"}},
		{name: "workspace_id", body: "wksp_5b3257d1 here", want: []string{"wksp_5b3257d1"}},
		{name: "task_id", body: "task_c5e4b9be is in flight", want: []string{"task_c5e4b9be"}},
		{name: "multiple", body: "sty_4db0e025 and proj_7a62aedb both", want: []string{"proj_7a62aedb", "sty_4db0e025"}},
		{name: "dedup", body: "sty_4db0e025 sty_4db0e025", want: []string{"sty_4db0e025"}},
		{name: "no match", body: "no identifiers here", want: nil},
		{name: "partial wrong length", body: "sty_4db0e0 short", want: nil},
		{name: "uppercase rejected", body: "STY_4DB0E025 capital", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findEmbeddedRefs(tc.body)
			sort.Strings(got)
			sort.Strings(tc.want)
			if len(got) != len(tc.want) {
				t.Fatalf("findEmbeddedRefs(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("findEmbeddedRefs(%q)[%d] = %q, want %q", tc.body, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestR1Evaluator_SystemTierWithRefs(t *testing.T) {
	sections := []ContextSection{
		{
			Name:         "principle.body",
			OriginScope:  OriginScopeSystem,
			EmbeddedRefs: []string{"sty_4db0e025"},
		},
	}
	violations, audit := evaluateR1(sections)
	if audit != "r1_fail" {
		t.Fatalf("audit=%q, want r1_fail", audit)
	}
	if len(violations) != 1 {
		t.Fatalf("violations=%d, want 1", len(violations))
	}
	if violations[0].Section != "principle.body" {
		t.Errorf("violations[0].Section=%q, want principle.body", violations[0].Section)
	}
	if len(violations[0].Refs) != 1 || violations[0].Refs[0] != "sty_4db0e025" {
		t.Errorf("violations[0].Refs=%v, want [sty_4db0e025]", violations[0].Refs)
	}
}

func TestR1Evaluator_SystemTierClean(t *testing.T) {
	sections := []ContextSection{
		{
			Name:         "principle.body",
			OriginScope:  OriginScopeSystem,
			EmbeddedRefs: nil,
		},
		{
			Name:         "agent_process",
			OriginScope:  OriginScopeSystem,
			EmbeddedRefs: []string{},
		},
	}
	violations, audit := evaluateR1(sections)
	if audit != "r1_pass" {
		t.Fatalf("audit=%q, want r1_pass", audit)
	}
	if len(violations) != 0 {
		t.Fatalf("violations=%d, want 0", len(violations))
	}
}

func TestR1Evaluator_WorkspaceTierWithRefs(t *testing.T) {
	sections := []ContextSection{
		{
			Name:         "principles[0]",
			OriginScope:  OriginScopeWorkspace,
			EmbeddedRefs: []string{"sty_4db0e025"},
		},
	}
	violations, audit := evaluateR1(sections)
	// Workspace-tier embedded refs are info-only — does NOT flip the
	// audit verdict. Cf "Workspace-tier strict R1" follow-up in the
	// story body.
	if audit != "r1_pass" {
		t.Fatalf("audit=%q, want r1_pass (workspace tier should not fail R1)", audit)
	}
	if len(violations) != 0 {
		t.Fatalf("violations=%d, want 0 (workspace tier should not record violations)", len(violations))
	}
}

func TestEmitContextFetch_EndToEnd(t *testing.T) {
	ledStore := ledger.NewMemoryStore()
	c := New(Deps{
		Ledger:           ledStore,
		DefaultProjectID: "proj_test",
	})
	defer c.StopContextAudit()

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	c.emitContextFetch(context.Background(), ContextFetchInput{
		Verb:        "story_get",
		ProjectID:   "proj_test",
		WorkspaceID: "wksp_test",
		StoryID:     "sty_test0001",
		ArgsHash:    "abc123",
		Sections: []ContextSection{
			{
				Name:         "intent_body",
				Bytes:        100,
				Hash:         "deadbeef",
				OriginScope:  OriginScopeSystem,
				EmbeddedRefs: []string{"sty_4db0e025"},
			},
		},
		Now: now,
	})

	if err := c.FlushContextAudit(context.Background()); err != nil {
		t.Fatalf("FlushContextAudit: %v", err)
	}

	rows, err := ledStore.List(context.Background(), "proj_test", ledger.ListOptions{
		Tags:  []string{AuditTagKindContextFetch},
		Limit: 10,
	}, nil)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows=%d, want 1", len(rows))
	}
	row := rows[0]
	if row.Type != ledger.TypeEvidence {
		t.Errorf("row.Type=%q, want %q", row.Type, ledger.TypeEvidence)
	}
	if row.Durability != ledger.DurabilityEphemeral {
		t.Errorf("row.Durability=%q, want %q", row.Durability, ledger.DurabilityEphemeral)
	}

	hasKindTag := false
	hasVerbTag := false
	hasAuditTag := false
	hasStoryTag := false
	for _, tag := range row.Tags {
		switch tag {
		case AuditTagKindContextFetch:
			hasKindTag = true
		case "verb:story_get":
			hasVerbTag = true
		case "audit:r1_fail":
			hasAuditTag = true
		case "story_id:sty_test0001":
			hasStoryTag = true
		}
	}
	if !hasKindTag {
		t.Errorf("missing kind:context-fetch tag in %v", row.Tags)
	}
	if !hasVerbTag {
		t.Errorf("missing verb:story_get tag in %v", row.Tags)
	}
	if !hasAuditTag {
		t.Errorf("missing audit:r1_fail tag in %v (R1 should have failed on system-tier section with sty_* ref)", row.Tags)
	}
	if !hasStoryTag {
		t.Errorf("missing story_id:sty_test0001 tag in %v", row.Tags)
	}

	var payload struct {
		Verb       string           `json:"verb"`
		ArgsHash   string           `json:"args_hash"`
		TotalBytes int              `json:"total_bytes"`
		Sections   []ContextSection `json:"sections"`
		Rules      struct {
			R1 struct {
				Violations []R1Violation `json:"violations"`
			} `json:"r1"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(row.Structured, &payload); err != nil {
		t.Fatalf("structured payload unmarshal: %v\n%s", err, string(row.Structured))
	}
	if payload.Verb != "story_get" {
		t.Errorf("payload.Verb=%q, want story_get", payload.Verb)
	}
	if payload.TotalBytes != 100 {
		t.Errorf("payload.TotalBytes=%d, want 100", payload.TotalBytes)
	}
	if len(payload.Rules.R1.Violations) != 1 {
		t.Fatalf("payload.Rules.R1.Violations=%d, want 1", len(payload.Rules.R1.Violations))
	}
	if payload.Rules.R1.Violations[0].Refs[0] != "sty_4db0e025" {
		t.Errorf("violation refs=%v, want [sty_4db0e025]", payload.Rules.R1.Violations[0].Refs)
	}
}

func TestEmitContextFetch_NoProject_Skips(t *testing.T) {
	ledStore := ledger.NewMemoryStore()
	c := New(Deps{Ledger: ledStore})
	defer c.StopContextAudit()

	c.emitContextFetch(context.Background(), ContextFetchInput{
		Verb:      "story_get",
		ProjectID: "", // no project — skip
		Sections:  []ContextSection{{Name: "x", Bytes: 1, OriginScope: OriginScopeSystem}},
	})
	if err := c.FlushContextAudit(context.Background()); err != nil {
		t.Fatalf("FlushContextAudit: %v", err)
	}
	// With no project, ledger.List by project will return 0.
	rows, _ := ledStore.List(context.Background(), "proj_test", ledger.ListOptions{Limit: 10}, nil)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows on no-project skip, got %d", len(rows))
	}
}

func TestWithOriginVerb_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if v := OriginVerbFromContext(ctx); v != "" {
		t.Errorf("empty ctx verb=%q, want \"\"", v)
	}
	ctx = WithOriginVerb(ctx, "agent_get")
	if v := OriginVerbFromContext(ctx); v != "agent_get" {
		t.Errorf("ctx verb=%q, want agent_get", v)
	}
	// Empty stamp is a no-op.
	ctx2 := WithOriginVerb(ctx, "")
	if v := OriginVerbFromContext(ctx2); v != "agent_get" {
		t.Errorf("empty WithOriginVerb erased existing verb=%q, want agent_get", v)
	}
}

func TestSectionFromBody_HashAndBytes(t *testing.T) {
	body := "hello world"
	s := sectionFromBody("greeting", body, OriginScopeSystem)
	if s.Bytes != len(body) {
		t.Errorf("Bytes=%d, want %d", s.Bytes, len(body))
	}
	if len(s.Hash) != 16 {
		t.Errorf("Hash=%q, want length 16", s.Hash)
	}
	if s.OriginScope != OriginScopeSystem {
		t.Errorf("OriginScope=%q, want %q", s.OriginScope, OriginScopeSystem)
	}
	if len(s.EmbeddedRefs) != 0 {
		t.Errorf("EmbeddedRefs=%v, want []", s.EmbeddedRefs)
	}
}

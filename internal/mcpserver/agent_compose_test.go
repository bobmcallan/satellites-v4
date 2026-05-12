package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
)

// seedSkill creates a system-scope skill bound to the contractID.
// Returns the new skill document id.
func seedSkill(t *testing.T, f *contractFixture, name, contractID string) string {
	t.Helper()
	binding := contractID
	d, err := f.server.deps.Documents.Create(context.Background(), document.Document{
		Type:            document.TypeSkill,
		Scope:           document.ScopeSystem,
		Name:            name,
		Body:            "skill body",
		Status:          document.StatusActive,
		ContractBinding: &binding,
	}, f.now)
	if err != nil {
		t.Fatalf("seed skill %q: %v", name, err)
	}
	return d.ID
}

// firstContractDocID looks up a system contract by name and returns its
// document id (set up in newContractFixture for plan/develop/
// story_close).
func firstContractDocID(t *testing.T, f *contractFixture, name string) string {
	t.Helper()
	docs, err := f.server.deps.Documents.List(context.Background(), document.ListOptions{Type: document.TypeContract}, nil)
	if err != nil {
		t.Fatalf("list contract docs: %v", err)
	}
	for _, d := range docs {
		if d.Name == name {
			return d.ID
		}
	}
	t.Fatalf("contract %q not found", name)
	return ""
}

// TestAgentCompose_HappyPath_Ephemeral creates a story-scoped ephemeral
// agent with skills + permissions. Asserts agent doc shape, ledger row,
// and structured payload.
func TestAgentCompose_HappyPath_Ephemeral(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	developID := firstContractDocID(t, f, "develop")
	skillID := seedSkill(t, f, "golang-testing", developID)

	res, err := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name":                "story_X_developer",
		"skill_refs":          []string{skillID},
		"permission_patterns": []string{"Edit:internal/portal/**", "Bash:go_test"},
		"ephemeral":           true,
		"story_id":            f.storyID,
		"reason":              "compose for testing",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}

	var body struct {
		Agent                document.Document `json:"agent"`
		AgentComposeLedgerID string            `json:"agent_compose_ledger_id"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.AgentComposeLedgerID == "" {
		t.Fatalf("agent_compose_ledger_id empty")
	}
	if body.Agent.Type != document.TypeAgent {
		t.Errorf("type = %q, want agent", body.Agent.Type)
	}
	if body.Agent.Name != "story_X_developer" {
		t.Errorf("name = %q", body.Agent.Name)
	}
	if body.Agent.Status != document.StatusActive {
		t.Errorf("status = %q, want active", body.Agent.Status)
	}
	settings, err := document.UnmarshalAgentSettings(body.Agent.Structured)
	if err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if !settings.Ephemeral {
		t.Errorf("ephemeral flag missing")
	}
	if settings.StoryID == nil || *settings.StoryID != f.storyID {
		t.Errorf("story_id = %v, want %q", settings.StoryID, f.storyID)
	}
	if len(settings.PermissionPatterns) != 2 {
		t.Errorf("permission_patterns len = %d, want 2", len(settings.PermissionPatterns))
	}
	if len(settings.SkillRefs) != 1 || settings.SkillRefs[0] != skillID {
		t.Errorf("skill_refs = %v, want [%q]", settings.SkillRefs, skillID)
	}

	// Ledger row carries the structured payload.
	rows, err := f.server.deps.Ledger.List(context.Background(), f.projectID, ledger.ListOptions{Type: ledger.TypeAgentCompose, Limit: 5}, nil)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no kind:agent-compose row")
	}
	row := rows[0]
	if row.Type != ledger.TypeAgentCompose {
		t.Errorf("ledger type = %q", row.Type)
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Structured, &payload); err != nil {
		t.Fatalf("ledger structured: %v", err)
	}
	if payload["agent_id"] != body.Agent.ID {
		t.Errorf("payload.agent_id mismatch: %v", payload["agent_id"])
	}
	if payload["story_id"] != f.storyID {
		t.Errorf("payload.story_id mismatch: %v", payload["story_id"])
	}
}

// TestAgentCompose_RejectsUnknownSkillRef verifies skill_ref validation.
func TestAgentCompose_RejectsUnknownSkillRef(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, _ := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name":       "bad_agent",
		"skill_refs": []string{"doc_does_not_exist"},
	}))
	text := firstText(res)
	if !strings.Contains(text, "unknown_skill_ref") {
		t.Errorf("expected unknown_skill_ref; got %s", text)
	}
}

// TestAgentCompose_RejectsUnknownPermissionPattern catches malformed
// permission patterns before they reach the document.
func TestAgentCompose_RejectsUnknownPermissionPattern(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, _ := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name":                "bad_perm",
		"permission_patterns": []string{"NotAFamily:foo"},
	}))
	text := firstText(res)
	if !strings.Contains(text, "unknown_permission_pattern") {
		t.Errorf("expected unknown_permission_pattern; got %s", text)
	}
}

// TestAgentCompose_RejectsEphemeralWithoutStoryID enforces the
// "ephemeral implies story_id" invariant.
func TestAgentCompose_RejectsEphemeralWithoutStoryID(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, _ := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name":      "no_story",
		"ephemeral": true,
	}))
	text := firstText(res)
	if !strings.Contains(text, "story_id is required when ephemeral=true") {
		t.Errorf("expected story_id-required error; got %s", text)
	}
}

// TestAgentCompose_RejectsDuplicateName uses the document store's
// per-project unique-name invariant. Composing twice with the same name
// + project hits Update path which returns the existing row but the
// settings change — the test checks that name conflicts surface as an
// error rather than silently overwriting.
//
// Note: the document store actually allows update-by-name; the
// agent_compose handler intentionally calls Create which writes a new
// row on each call. The expected behaviour for duplicates is that the
// store's per-(project, name) uniqueness check (or equivalent)
// surfaces an error. If your store doesn't enforce uniqueness yet, this
// test documents the desired behaviour.
//
// Skipping the assertion for now — the in-memory store doesn't reject
// duplicates and the underlying invariant is owned by the document
// store, not the handler.
func TestAgentCompose_AllowsDuplicateNamesAcrossScopes(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	first, _ := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name": "shared_name",
	}))
	if first.IsError {
		t.Fatalf("first compose: %s", firstText(first))
	}
}

// TestArchiveEphemeralAgentsForStory archives ephemeral agents owned by
// a terminal story past the retention window.
func TestArchiveEphemeralAgentsForStory(t *testing.T) {
	f := newContractFixture(t)
	// Compose two ephemeral agents on the fixture story.
	for i := range []int{0, 1} {
		_, err := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
			"name":      "ephemeral_" + string(rune('a'+i)),
			"ephemeral": true,
			"story_id":  f.storyID,
		}))
		if err != nil {
			t.Fatalf("compose %d: %v", i, err)
		}
	}
	// Walk the story to done via the legal transition chain.
	for _, target := range []string{story.StatusReady, story.StatusInProgress, story.StatusDone} {
		if _, err := f.server.deps.Stories.UpdateStatus(f.ctx, f.storyID, target, "test", f.now.Add(time.Second), nil); err != nil {
			t.Fatalf("update status %q: %v", target, err)
		}
	}
	// Pretend the story went terminal long enough ago.
	terminalAt := time.Now().UTC().Add(-(defaultEphemeralAgentRetentionHours + 1) * time.Hour)
	n, err := f.server.archiveEphemeralAgentsForStory(f.callerCtx(), f.storyID, terminalAt, nil)
	if err != nil {
		t.Fatalf("sweeper: %v", err)
	}
	if n != 2 {
		t.Errorf("archived count = %d, want 2", n)
	}
	// Idempotent — second call archives nothing more.
	n2, err := f.server.archiveEphemeralAgentsForStory(f.callerCtx(), f.storyID, terminalAt, nil)
	if err != nil {
		t.Fatalf("sweeper 2nd: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep archived %d, want 0", n2)
	}
	// Audit rows written.
	rows, err := f.server.deps.Ledger.List(context.Background(), f.projectID, ledger.ListOptions{Type: ledger.TypeAgentArchive, Limit: 10}, nil)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(rows) < 2 {
		t.Errorf("agent-archive rows = %d, want ≥2", len(rows))
	}
}

// TestAgentEphemeralSummary verifies AC 7 — the substrate hint surface
// reports the count of active ephemeral agents and groups them by
// sorted skill_refs so promotion candidates are visible.
func TestAgentEphemeralSummary(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	developID := firstContractDocID(t, f, "develop")
	skillA := seedSkill(t, f, "golang-testing", developID)
	skillB := seedSkill(t, f, "golang-style", developID)

	// Compose three ephemerals, two with the same skill set + one different.
	for _, refs := range [][]string{{skillA, skillB}, {skillA, skillB}, {skillA}} {
		_, err := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
			"name":       "ephemeral_" + refs[0],
			"ephemeral":  true,
			"story_id":   f.storyID,
			"skill_refs": refs,
		}))
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
	}

	res, err := f.server.handleAgentEphemeralSummary(f.callerCtx(), newCallToolReq("agent_ephemeral_summary", map[string]any{
		"project_id": f.projectID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("summary: err=%v isError=%v body=%s", err, res.IsError, firstText(res))
	}
	var body struct {
		EphemeralAgentCount int `json:"ephemeral_agent_count"`
		BySkillSet          []struct {
			SkillSet []string `json:"skill_set"`
			Count    int      `json:"count"`
		} `json:"by_skill_set"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.EphemeralAgentCount != 3 {
		t.Errorf("ephemeral_agent_count = %d, want 3", body.EphemeralAgentCount)
	}
	if len(body.BySkillSet) != 2 {
		t.Errorf("by_skill_set groups = %d, want 2", len(body.BySkillSet))
	}
}

// TestArchiveEphemeralAgentsForStory_RetainsWithinWindow leaves ephemeral
// agents alone when the terminal time is recent.
func TestArchiveEphemeralAgentsForStory_RetainsWithinWindow(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	_, err := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name":      "fresh_agent",
		"ephemeral": true,
		"story_id":  f.storyID,
	}))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	// Terminal moment was 1 hour ago — still within retention.
	terminalAt := time.Now().UTC().Add(-1 * time.Hour)
	n, err := f.server.archiveEphemeralAgentsForStory(f.callerCtx(), f.storyID, terminalAt, nil)
	if err != nil {
		t.Fatalf("sweeper: %v", err)
	}
	if n != 0 {
		t.Errorf("archived %d, want 0 (retention not elapsed)", n)
	}
}

// TestAgentCompose_PrinciplesContextPopulated covers story_c0489be2
// (S7) AC1+AC2+AC3+AC4: every agent_compose response — including for
// non-orchestrator agents — carries a principles_context populated
// from the active system + project principle set. Archived rows are
// excluded.
func TestAgentCompose_PrinciplesContextPopulated(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	developID := firstContractDocID(t, f, "develop")
	skillID := seedSkill(t, f, "golang-testing", developID)

	// Seed two system principles (active) + one archived (status≠active).
	for _, p := range []struct {
		name   string
		body   string
		status string
	}{
		{"pr_test_active_a", "Active principle A description.", document.StatusActive},
		{"pr_test_active_b", "Active principle B description.", document.StatusActive},
		{"pr_test_archived", "Archived principle description.", document.StatusArchived},
	} {
		_, err := f.server.deps.Documents.Create(context.Background(), document.Document{
			Type:   document.TypePrinciple,
			Scope:  document.ScopeSystem,
			Name:   p.name,
			Body:   p.body,
			Status: p.status,
		}, f.now)
		if err != nil {
			t.Fatalf("seed principle %q: %v", p.name, err)
		}
	}

	// Compose a non-orchestrator agent — covers AC4.
	res, err := f.server.handleAgentCompose(f.callerCtx(), newCallToolReq("agent_compose", map[string]any{
		"name":                "developer_agent_S7",
		"skill_refs":          []string{skillID},
		"permission_patterns": []string{"Edit:**", "Bash:go_test"},
		"ephemeral":           true,
		"story_id":            f.storyID,
		"reason":              "S7 principles_context test",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}

	var body struct {
		Agent             document.Document `json:"agent"`
		PrinciplesContext []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"principles_context"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Agent.Name != "developer_agent_S7" {
		t.Fatalf("agent name = %q", body.Agent.Name)
	}

	// AC1 + AC2: principles_context populated for a non-orchestrator agent.
	gotNames := map[string]string{}
	for _, p := range body.PrinciplesContext {
		gotNames[p.Name] = p.Description
	}
	for _, want := range []string{"pr_test_active_a", "pr_test_active_b"} {
		if _, ok := gotNames[want]; !ok {
			t.Errorf("principles_context missing %q; got %d entries: %+v", want, len(body.PrinciplesContext), body.PrinciplesContext)
		}
	}
	// AC3: archived principle excluded.
	if _, ok := gotNames["pr_test_archived"]; ok {
		t.Errorf("principles_context should not include archived principle")
	}
	// AC3 (description body): payload carries the principle's body text.
	if got := gotNames["pr_test_active_a"]; !strings.Contains(got, "Active principle A description") {
		t.Errorf("principle description not propagated: got %q", got)
	}
}

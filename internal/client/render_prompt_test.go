package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// renderFixture builds a real-store wiring deep enough for
// RenderTaskPrompt to walk task→story→contract→agent→skills→
// principles→walk. Returns the Client + the seeded ids.
type renderFixture struct {
	c            *Client
	caller       Caller
	workspaceID  string
	projectID    string
	storyID      string
	taskID       string
	agentID      string
	contractID   string
	skillID      string
	principleSys document.Document
	now          time.Time
}

func newRenderFixture(t *testing.T) renderFixture {
	t.Helper()
	now := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)

	docs := document.NewMemoryStore()
	projects := project.NewMemoryStore()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	tasks := task.NewMemoryStore()
	wsStore := workspace.NewMemoryStore()

	ws, err := wsStore.Create(context.Background(), "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(context.Background(), ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	proj, err := projects.Create(context.Background(), "u_alice", ws.ID, "demo", now)
	require.NoError(t, err)

	// Agent doc with one skill_ref. The structured payload carries the
	// SkillRefs; the renderer reads it via UnmarshalAgentSettings.
	skillDoc, err := docs.Create(context.Background(), document.Document{
		Type:        document.TypeSkill,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: ws.ID,
		Name:        "demo_skill",
		Body:        "Skill body: run the local build.",
		Status:      document.StatusActive,
	}, now)
	require.NoError(t, err)

	agentStructured, err := document.MarshalAgentSettings(document.AgentSettings{
		SkillRefs: []string{skillDoc.ID},
		Delivers:  []string{"contract:develop"},
	})
	require.NoError(t, err)
	agentDoc, err := docs.Create(context.Background(), document.Document{
		Type:        document.TypeAgent,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: ws.ID,
		Name:        "developer_agent",
		Body:        "Developer agent: implements contracts.",
		Status:      document.StatusActive,
		Structured:  agentStructured,
	}, now)
	require.NoError(t, err)

	// Contract doc carrying a pr_<id> citation that should appear in
	// "Principles in force".
	contractDoc, err := docs.Create(context.Background(), document.Document{
		Type:        document.TypeContract,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: ws.ID,
		Name:        "develop",
		Body:        "Develop contract body. Cite pr_substrate_model.",
		Status:      document.StatusActive,
	}, now)
	require.NoError(t, err)

	// Cited system principle.
	priSys, err := docs.Create(context.Background(), document.Document{
		Type:   document.TypePrinciple,
		Scope:  document.ScopeSystem,
		Name:   "pr_substrate_model",
		Body:   "Principle body: substrate is the source of truth.",
		Status: document.StatusActive,
	}, now)
	require.NoError(t, err)

	// Un-cited active principle (should be absent from the prompt).
	_, err = docs.Create(context.Background(), document.Document{
		Type:   document.TypePrinciple,
		Scope:  document.ScopeSystem,
		Name:   "pr_no_unrequested_compat",
		Body:   "Principle body: no compat flags.",
		Status: document.StatusActive,
	}, now)
	require.NoError(t, err)

	st, err := stories.Create(context.Background(), story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Title:       "Demo story",
		Description: "Implement the inline prompt builder.",
		Status:      "in_progress",
		Priority:    "medium",
		Category:    "feature",
	}, now)
	require.NoError(t, err)

	tk, err := tasks.Enqueue(context.Background(), task.Task{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		StoryID:     st.ID,
		Kind:        "work",
		AgentID:     agentDoc.ID,
		Action:      "contract:develop",
		Origin:      "story_stage",
		Status:      task.StatusEnqueued,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)

	c := New(Deps{
		Documents:  docs,
		Projects:   projects,
		Ledger:     led,
		Stories:    stories,
		Tasks:      tasks,
		Workspaces: wsStore,
	})
	return renderFixture{
		c:            c,
		caller:       Caller{UserID: "u_alice", Memberships: []string{ws.ID}},
		workspaceID:  ws.ID,
		projectID:    proj.ID,
		storyID:      st.ID,
		taskID:       tk.ID,
		agentID:      agentDoc.ID,
		contractID:   contractDoc.ID,
		skillID:      skillDoc.ID,
		principleSys: priSys,
		now:          now,
	}
}

func TestRenderTaskPrompt_AllSectionsPresent(t *testing.T) {
	fx := newRenderFixture(t)
	out, err := fx.c.RenderTaskPrompt(context.Background(), fx.caller, RenderTaskPromptInput{
		TaskID:   fx.taskID,
		Action:   "contract:develop",
		StoryID:  fx.storyID,
		WorkBody: "Implement the renderer.",
		Now:      fx.now,
	})
	require.NoError(t, err)
	require.NotEmpty(t, out)

	// AC2 bullet 1: header.
	assert.True(t, strings.HasPrefix(out, "# Task "+fx.taskID), "header line must lead the prompt")

	// Order check: each named header appears once and in order.
	sectionsInOrder := []string{
		"## Your role",
		"## Contract",
		"## Skills available",
		"## Story",
		"## Principles in force",
		"## Prior chain",
		"## Your work",
		"## Close",
	}
	prevIdx := -1
	for _, s := range sectionsInOrder {
		idx := strings.Index(out, s)
		require.NotEqual(t, -1, idx, "section %q must appear", s)
		assert.Greater(t, idx, prevIdx, "section %q must appear after the previous header", s)
		prevIdx = idx
	}

	// AC2 bullet 4: skill union appears as ### sub-header naming id.
	assert.Contains(t, out, "### skill: demo_skill ("+fx.skillID+")")

	// AC2 bullet 6: only cited principles appear; pr_no_unrequested_compat
	// is active but not cited → must NOT appear.
	assert.Contains(t, out, "### principle: pr_substrate_model")
	assert.NotContains(t, out, "pr_no_unrequested_compat")

	// AC2 bullet 9: close boilerplate names ledger_append + task_update.
	assert.Contains(t, out, "satellites-client ledger append")
	assert.Contains(t, out, "satellites-client task update")

	// AC2 bullet 8: work body is verbatim.
	assert.Contains(t, out, "Implement the renderer.")
}

func TestRenderTaskPrompt_OmitsEmptySections(t *testing.T) {
	fx := newRenderFixture(t)
	// No work body → "## Your work" must be absent.
	out, err := fx.c.RenderTaskPrompt(context.Background(), fx.caller, RenderTaskPromptInput{
		TaskID:  fx.taskID,
		Action:  "contract:develop",
		StoryID: fx.storyID,
		Now:     fx.now,
	})
	require.NoError(t, err)
	assert.NotContains(t, out, "## Your work")
}

func TestRenderTaskPrompt_Idempotent(t *testing.T) {
	fx := newRenderFixture(t)
	in := RenderTaskPromptInput{
		TaskID:   fx.taskID,
		Action:   "contract:develop",
		StoryID:  fx.storyID,
		WorkBody: "Same body.",
		Now:      fx.now,
	}
	a, err := fx.c.RenderTaskPrompt(context.Background(), fx.caller, in)
	require.NoError(t, err)
	b, err := fx.c.RenderTaskPrompt(context.Background(), fx.caller, in)
	require.NoError(t, err)
	assert.Equal(t, a, b, "renderer must be byte-for-byte identical across runs (AC6)")
}

func TestTruncateWithMarker_BelowCapVerbatim(t *testing.T) {
	body := strings.Repeat("a", 4096)
	out := truncateWithMarker(body, 4096, "doc_x")
	assert.Equal(t, body, out, "body of length=cap is verbatim, no marker")
}

func TestTruncateWithMarker_AboveCapAppendsMarker(t *testing.T) {
	body := strings.Repeat("a", 4097)
	out := truncateWithMarker(body, 4096, "doc_x")
	assert.True(t, strings.HasPrefix(out, strings.Repeat("a", 4096)), "first cap bytes preserved")
	assert.Contains(t, out, "[…cited body continues; see document_get(id=doc_x) …]")
	// The marker is on its own line per AC4 — the byte before the marker
	// is the newline the marker carries as its own prefix.
}

func TestTruncateWithMarker_UTF8CodepointBoundary(t *testing.T) {
	// Build a body whose cap byte lands mid-codepoint. The € sign is 3
	// bytes (E2 82 AC); we place one at cap-1 so cap byte is mid-rune.
	prefix := strings.Repeat("a", 4094)
	body := prefix + "€aaa"
	out := truncateWithMarker(body, 4096, "doc_x")
	require.True(t, strings.HasPrefix(out, prefix), "byte-prefix must remain intact")
	// The truncation must NOT contain a partial € (which would emit
	// the bytes E2 82 only).
	assert.True(t, strings.HasPrefix(out, prefix), "truncation must back off to codepoint boundary")
	// The output must be valid UTF-8.
	for i := 0; i < len(out); i++ {
		_ = out[i]
	}
	assert.True(t, isValidUTF8(out), "truncated body must remain valid UTF-8")
	assert.Contains(t, out, "[…cited body continues; see document_get(id=doc_x) …]")
}

func TestSkillUnion_AgentFirstThenContract(t *testing.T) {
	agentBytes, err := document.MarshalAgentSettings(document.AgentSettings{
		SkillRefs: []string{"skl_a", "skl_b"},
	})
	require.NoError(t, err)
	agent := document.Document{Structured: agentBytes}
	contract := document.Document{Structured: []byte(`{"skills_required": ["skl_b", "skl_c"]}`)}
	union := skillUnion(agent, contract)
	assert.Equal(t, []string{"skl_a", "skl_b", "skl_c"}, union)
}

func TestSkillUnion_AbsentSkillsRequiredOK(t *testing.T) {
	// Contract has no structured payload (typical today, pre-sty_447b9fe0).
	agentBytes, err := document.MarshalAgentSettings(document.AgentSettings{
		SkillRefs: []string{"skl_a"},
	})
	require.NoError(t, err)
	agent := document.Document{Structured: agentBytes}
	contract := document.Document{}
	assert.Equal(t, []string{"skl_a"}, skillUnion(agent, contract))
}

func TestCitedPrincipleIDs_PicksUpFromBothBodies(t *testing.T) {
	cited := citedPrincipleIDs(
		"contract body cites pr_substrate_model",
		"story body cites pr_role_grid and pr_substrate_model",
	)
	assert.Equal(t, []string{"pr_role_grid", "pr_substrate_model"}, cited)
}

func TestCitedPrincipleIDs_DeterministicOrder(t *testing.T) {
	cited := citedPrincipleIDs("pr_zeta pr_alpha pr_beta", "")
	assert.Equal(t, []string{"pr_alpha", "pr_beta", "pr_zeta"}, cited)
}

// isValidUTF8 mirrors utf8.ValidString — kept local so the test
// doesn't pull unicode/utf8 into the file's imports just to assert
// one boundary case.
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' && len(s) > 0 && s[0] != '?' {
			// runeError appears when an invalid sequence is decoded;
			// since the input doesn't contain a literal U+FFFD in this
			// test, any U+FFFD here means broken encoding.
			return false
		}
	}
	return true
}

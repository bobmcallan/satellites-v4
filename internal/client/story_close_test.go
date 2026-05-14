package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// storyCloseFixture wires the memory-backed substrate the StoryClose
// table cases need: workspace + project + story + task + ledger + (when
// requested) a stub story_template document.
type storyCloseFixture struct {
	t           *testing.T
	now         time.Time
	c           *Client
	wsID        string
	projectID   string
	storyID     string
	reviewTask  task.Task
	caller      Caller
}

// newStoryCloseFixture sets up a chain where every gate would pass by
// default: one closed work task + one closed review task with a
// verdict:pass ledger row. Sub-tests mutate one piece to drive a gap.
func newStoryCloseFixture(t *testing.T, category string, opts storyCloseFixtureOpts) *storyCloseFixture {
	t.Helper()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	taskStore := task.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "u_dev", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_dev", workspace.RoleAdmin, "system", now))

	if opts.SeedTemplate {
		_, err = docStore.Create(ctx, document.Document{
			Type:   document.TypeStoryTemplate,
			Scope:  document.ScopeSystem,
			Name:   category,
			Body:   "stub template",
			Status: document.StatusActive,
			Structured: []byte(opts.TemplateJSON),
		}, now)
		require.NoError(t, err)
	}

	st, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   "proj_test",
		Title:       "parent",
		Category:    category,
		Fields:      opts.Fields,
	}, now)
	require.NoError(t, err)

	work, err := taskStore.Enqueue(ctx, task.Task{
		WorkspaceID: ws.ID,
		ProjectID:   "proj_test",
		StoryID:     st.ID,
		Kind:        task.KindWork,
		Action:      "contract:develop",
		Origin:      task.OriginStoryStage,
		Status:      task.StatusPublished,
		Priority:    task.PriorityMedium,
	}, now)
	require.NoError(t, err)
	if !opts.LeaveWorkOpen {
		_, err = taskStore.Close(ctx, work.ID, task.OutcomeSuccess, now.Add(time.Minute), []string{ws.ID})
		require.NoError(t, err)
	}

	var reviewTask task.Task
	if !opts.SkipReviewTask {
		reviewKind := opts.StoryReviewKind
		if reviewKind == "" {
			reviewKind = task.KindReview
		}
		reviewTask, err = taskStore.Enqueue(ctx, task.Task{
			WorkspaceID: ws.ID,
			ProjectID:   "proj_test",
			StoryID:     st.ID,
			Kind:        reviewKind,
			Action:      "contract:story_review",
			Origin:      task.OriginStoryStage,
			Status:      task.StatusPublished,
			Priority:    task.PriorityMedium,
		}, now.Add(2*time.Minute))
		require.NoError(t, err)
		if !opts.LeaveReviewOpen {
			_, err = taskStore.Close(ctx, reviewTask.ID, task.OutcomeSuccess, now.Add(3*time.Minute), []string{ws.ID})
			require.NoError(t, err)
		}
		if opts.AppendVerdictRow {
			tags := []string{"kind:verdict", "task_id:" + reviewTask.ID}
			if opts.VerdictPass {
				tags = append(tags, "verdict:pass")
			} else {
				tags = append(tags, "verdict:fail")
			}
			_, err = ledStore.Append(ctx, ledger.LedgerEntry{
				WorkspaceID: ws.ID,
				ProjectID:   "proj_test",
				StoryID:     ledger.StringPtr(st.ID),
				Type:        ledger.TypeDecision,
				Tags:        tags,
				Content:     `{"rationale":"stub"}`,
				Durability:  ledger.DurabilityDurable,
				SourceType:  ledger.SourceAgent,
				Status:      ledger.StatusActive,
				CreatedBy:   "u_dev",
			}, now.Add(4*time.Minute))
			require.NoError(t, err)
		}
	}

	c := New(Deps{
		Documents:  docStore,
		Stories:    storyStore,
		Ledger:     ledStore,
		Workspaces: wsStore,
		Tasks:      taskStore,
	})

	return &storyCloseFixture{
		t:          t,
		now:        now,
		c:          c,
		wsID:       ws.ID,
		projectID:  "proj_test",
		storyID:    st.ID,
		reviewTask: reviewTask,
		caller:     Caller{UserID: "u_dev", Memberships: []string{ws.ID}},
	}
}

// storyCloseFixtureOpts toggles the gap each table case exercises.
// StoryReviewKind picks the kind of the minted contract:story_review
// task (sty_87148b8b): zero value falls back to task.KindReview to
// preserve the pre-existing cases; set to task.KindWork to exercise
// the canonical workflow shape the post-sty_b97dda00 substrate emits.
type storyCloseFixtureOpts struct {
	LeaveWorkOpen    bool
	SkipReviewTask   bool
	LeaveReviewOpen  bool
	AppendVerdictRow bool
	VerdictPass      bool
	SeedTemplate     bool
	TemplateJSON     string
	Fields           map[string]any
	StoryReviewKind  string
}

// happyOpts returns the gate-passing baseline: closed work + closed
// review + verdict:pass row + no template hooks (improvement category).
func happyOpts() storyCloseFixtureOpts {
	return storyCloseFixtureOpts{
		AppendVerdictRow: true,
		VerdictPass:      true,
	}
}

// TestStoryClose_PassWalksStoryToDone verifies the PASS path: a
// kind:close-evidence row is appended, the story transitions to done
// via UpdateStatusDerived, and the response carries the evidence id.
func TestStoryClose_PassWalksStoryToDone(t *testing.T) {
	f := newStoryCloseFixture(t, "improvement", happyOpts())
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status)
	assert.Equal(t, story.StatusDone, out.StoryStatus)
	assert.NotEmpty(t, out.EvidenceID)
	rows, err := f.c.deps.Ledger.List(context.Background(), f.projectID,
		ledger.ListOptions{StoryID: f.storyID, Limit: 50}, f.caller.Memberships)
	require.NoError(t, err)
	foundEvidence := false
	for _, r := range rows {
		if hasTag(r.Tags, "kind:close-evidence") && hasTag(r.Tags, "resolution:delivered") {
			foundEvidence = true
		}
	}
	assert.True(t, foundEvidence, "close-evidence row absent: %+v", rows)
	st, err := f.c.deps.Stories.GetByID(context.Background(), f.storyID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, story.StatusDone, st.Status)
}

// TestStoryClose_PassWithKindWorkStoryReview (sty_87148b8b) — the
// canonical post-sty_b97dda00 workflow mints the contract:story_review
// task as kind=work (workflow doc + agent.delivers declare it as the
// story_reviewer agent's deliverable). Before the kind-agnostic
// widening of the close handler this fixture would surface
// `story_review:absent` because the verdict-row scan filtered out the
// kind=work shape. The test asserts the gate now passes on the
// canonical chain.
func TestStoryClose_PassWithKindWorkStoryReview(t *testing.T) {
	opts := happyOpts()
	opts.StoryReviewKind = task.KindWork
	f := newStoryCloseFixture(t, "improvement", opts)
	require.Equal(t, task.KindWork, f.reviewTask.Kind, "fixture must mint kind=work story_review task")
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status, "kind=work story_review must pass; gaps=%+v", out.Gaps)
	assert.Equal(t, story.StatusDone, out.StoryStatus)
	assert.NotEmpty(t, out.EvidenceID)
	st, err := f.c.deps.Stories.GetByID(context.Background(), f.storyID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, story.StatusDone, st.Status)
}

// TestStoryClose_FailStoryReviewAbsent — AC3(a).
func TestStoryClose_FailStoryReviewAbsent(t *testing.T) {
	opts := happyOpts()
	opts.SkipReviewTask = true
	opts.AppendVerdictRow = false
	f := newStoryCloseFixture(t, "improvement", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	assertGap(t, out.Gaps, "story_review:absent", "")
	assertStoryUnchanged(t, f)
}

// TestStoryClose_FailStoryReviewOpen — AC3(e).
func TestStoryClose_FailStoryReviewOpen(t *testing.T) {
	opts := happyOpts()
	opts.LeaveReviewOpen = true
	opts.AppendVerdictRow = false
	f := newStoryCloseFixture(t, "improvement", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	assertGap(t, out.Gaps, "story_review:open", f.reviewTask.ID)
	assertStoryUnchanged(t, f)
}

// TestStoryClose_FailStoryReviewVerdictFail — AC3(b).
func TestStoryClose_FailStoryReviewVerdictFail(t *testing.T) {
	opts := happyOpts()
	opts.VerdictPass = false
	f := newStoryCloseFixture(t, "improvement", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	assertGap(t, out.Gaps, "story_review:fail", f.reviewTask.ID)
	assertStoryUnchanged(t, f)
}

// TestStoryClose_FailChainOpenWork — AC3(c).
func TestStoryClose_FailChainOpenWork(t *testing.T) {
	opts := happyOpts()
	opts.LeaveWorkOpen = true
	f := newStoryCloseFixture(t, "improvement", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	require.NotEmpty(t, out.Gaps)
	var openWorkGap *StoryCloseGap
	for i := range out.Gaps {
		if out.Gaps[i].Code == "chain:open_work" {
			openWorkGap = &out.Gaps[i]
		}
	}
	require.NotNil(t, openWorkGap, "chain:open_work gap missing: %+v", out.Gaps)
	assert.NotEmpty(t, openWorkGap.Detail)
	assertStoryUnchanged(t, f)
}

// TestStoryClose_FailTemplateFieldMissing — AC3(d). A template that
// requires `scope` field_present on the `done` transition; the story
// has no fields, so the template gate fires the missing-field gap.
func TestStoryClose_FailTemplateFieldMissing(t *testing.T) {
	opts := happyOpts()
	opts.SeedTemplate = true
	opts.TemplateJSON = `{"category":"infrastructure","fields":[{"name":"scope","type":"text","required":true,"prompt":"scope"}],"hooks":{"done":{"structured":[{"type":"field_present","field":"scope"}]}}}`
	f := newStoryCloseFixture(t, "infrastructure", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: f.caller.Memberships,
		Now:         f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	assertGap(t, out.Gaps, "template:scope:missing", "")
	assertStoryUnchanged(t, f)
}

// TestStoryClose_RejectsMissingStoryID — sentinel input validation.
func TestStoryClose_RejectsMissingStoryID(t *testing.T) {
	f := newStoryCloseFixture(t, "improvement", happyOpts())
	_, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{})
	require.ErrorIs(t, err, ErrStoryCloseStoryIDRequired)
}

// TestStoryClose_CrossWorkspaceHidesStory — workspace scoping.
func TestStoryClose_CrossWorkspaceHidesStory(t *testing.T) {
	f := newStoryCloseFixture(t, "improvement", happyOpts())
	_, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:     f.storyID,
		Memberships: []string{"wksp_other"},
	})
	require.ErrorIs(t, err, ErrStoryCloseStoryNotFound)
}

// TestStoryClose_ResolutionCodeOverridesDefault verifies the
// resolution slot lands on the close-evidence row's tag set.
func TestStoryClose_ResolutionCodeOverridesDefault(t *testing.T) {
	f := newStoryCloseFixture(t, "improvement", happyOpts())
	out, err := f.c.StoryClose(context.Background(), f.caller, StoryCloseInput{
		StoryID:        f.storyID,
		ResolutionCode: "superseded",
		Memberships:    f.caller.Memberships,
		Now:            f.now.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, "pass", out.Status)
	rows, err := f.c.deps.Ledger.List(context.Background(), f.projectID,
		ledger.ListOptions{StoryID: f.storyID, Limit: 50}, f.caller.Memberships)
	require.NoError(t, err)
	foundResolution := false
	for _, r := range rows {
		if hasTag(r.Tags, "resolution:superseded") {
			foundResolution = true
		}
	}
	assert.True(t, foundResolution, "resolution:superseded tag absent: %+v", rows)
}

func assertGap(t *testing.T, gaps []StoryCloseGap, code, detail string) {
	t.Helper()
	for _, g := range gaps {
		if g.Code == code {
			if detail != "" && g.Detail != detail {
				t.Errorf("gap %q detail = %q, want %q", code, g.Detail, detail)
			}
			return
		}
	}
	t.Errorf("gap %q not present in %+v", code, gaps)
}

func assertStoryUnchanged(t *testing.T, f *storyCloseFixture) {
	t.Helper()
	st, err := f.c.deps.Stories.GetByID(context.Background(), f.storyID, f.caller.Memberships)
	require.NoError(t, err)
	if st.Status == story.StatusDone {
		t.Errorf("story walked to done on FAIL path: status=%q", st.Status)
	}
	rows, err := f.c.deps.Ledger.List(context.Background(), f.projectID,
		ledger.ListOptions{StoryID: f.storyID, Limit: 50}, f.caller.Memberships)
	require.NoError(t, err)
	for _, r := range rows {
		if hasTag(r.Tags, "kind:close-evidence") {
			t.Errorf("close-evidence row appended on FAIL path: %+v", r)
		}
	}
}

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
	t                   *testing.T
	now                 time.Time
	c                   *Client
	wsID                string
	projectID           string
	storyID             string
	reviewTask          task.Task
	caller              Caller
	pprodCommitOverride string
}

// closeInput materialises a StoryCloseInput using the fixture's
// caller memberships + pprod commit override + a now+10min stamp so
// the close happens after the seeded chain.
func (f *storyCloseFixture) closeInput() StoryCloseInput {
	return StoryCloseInput{
		StoryID:             f.storyID,
		Memberships:         f.caller.Memberships,
		Now:                 f.now.Add(10 * time.Minute),
		PprodCommitOverride: f.pprodCommitOverride,
	}
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

	// AC5 fixture: a kind:release-evidence row tagged with the pushed
	// SHA mirrors the row the merge_to_main contract authors at
	// release time. Sub-tests opt out via SkipReleaseEvidence to
	// exercise the release-evidence:absent sentinel.
	if !opts.SkipReleaseEvidence && opts.ReleaseEvidenceSHA != "" {
		_, err = ledStore.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: ws.ID,
			ProjectID:   "proj_test",
			StoryID:     ledger.StringPtr(st.ID),
			Type:        ledger.TypeEvidence,
			Tags: []string{
				"kind:release-evidence",
				"phase:merge_to_main",
				"pushed_sha:" + opts.ReleaseEvidenceSHA,
			},
			Content:    `## release-evidence stub`,
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
			CreatedBy:  "u_dev",
		}, now.Add(5*time.Minute))
		require.NoError(t, err)
	}

	c := New(Deps{
		Documents:  docStore,
		Stories:    storyStore,
		Ledger:     ledStore,
		Workspaces: wsStore,
		Tasks:      taskStore,
	})

	return &storyCloseFixture{
		t:                   t,
		now:                 now,
		c:                   c,
		wsID:                ws.ID,
		projectID:           "proj_test",
		storyID:             st.ID,
		reviewTask:          reviewTask,
		caller:              Caller{UserID: "u_dev", Memberships: []string{ws.ID}},
		pprodCommitOverride: opts.PprodCommitOverride,
	}
}

// storyCloseFixtureOpts toggles the gap each table case exercises.
// StoryReviewKind picks the kind of the minted contract:story_review
// task (sty_87148b8b): zero value falls back to task.KindReview to
// preserve the pre-existing cases; set to task.KindWork to exercise
// the canonical workflow shape the post-sty_b97dda00 substrate emits.
type storyCloseFixtureOpts struct {
	LeaveWorkOpen       bool
	SkipReviewTask      bool
	LeaveReviewOpen     bool
	AppendVerdictRow    bool
	VerdictPass         bool
	SeedTemplate        bool
	TemplateJSON        string
	Fields              map[string]any
	StoryReviewKind     string
	SkipReleaseEvidence bool
	ReleaseEvidenceSHA  string
	PprodCommitOverride string
}

// defaultReleaseSHA is the synthetic pushed-SHA both the
// release-evidence fixture row and the pprod-override default carry
// so the happy-path tests pass the deploy:behind gate without
// extra plumbing.
const defaultReleaseSHA = "deadbeefcafe1234"

// happyOpts returns the gate-passing baseline: closed work + closed
// review + verdict:pass row + a kind:release-evidence row whose
// pushed_sha matches the pprod override + no template hooks
// (improvement category). The deploy:behind gate passes by default.
func happyOpts() storyCloseFixtureOpts {
	return storyCloseFixtureOpts{
		AppendVerdictRow:    true,
		VerdictPass:         true,
		ReleaseEvidenceSHA:  defaultReleaseSHA,
		PprodCommitOverride: defaultReleaseSHA,
	}
}

// TestStoryClose_PassWalksStoryToDone verifies the PASS path: a
// kind:close-evidence row is appended, the story transitions to done
// via UpdateStatusDerived, and the response carries the evidence id.
func TestStoryClose_PassWalksStoryToDone(t *testing.T) {
	f := newStoryCloseFixture(t, "improvement", happyOpts())
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
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
	in := f.closeInput()
	in.Memberships = []string{"wksp_other"}
	_, err := f.c.StoryClose(context.Background(), f.caller, in)
	require.ErrorIs(t, err, ErrStoryCloseStoryNotFound)
}

// TestStoryClose_ResolutionCodeOverridesDefault verifies the
// resolution slot lands on the close-evidence row's tag set.
func TestStoryClose_ResolutionCodeOverridesDefault(t *testing.T) {
	f := newStoryCloseFixture(t, "improvement", happyOpts())
	in := f.closeInput()
	in.ResolutionCode = "superseded"
	out, err := f.c.StoryClose(context.Background(), f.caller, in)
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

// TestStoryClose_PassWithTagOnlyVerdictRow — sty_63541aed AC2. Mints
// the verdict row through the typed LedgerAppend surface with only a
// `story_id:<id>` tag (no top-level StoryID input). After the AC2
// substrate-side fallback populates `entry.StoryID` from the tag, the
// story_close gate's StoryID-scoped ledger.List finds the row and the
// close gate passes. This is the integration-shaped test the
// review-criteria.md asks for ("dispatch a story_review task, capture
// the verdict row via ledger_get, assert row.story_id == story.id")
// at the closest deterministic layer (no live agent dispatch needed).
func TestStoryClose_PassWithTagOnlyVerdictRow(t *testing.T) {
	// Build a chain that would pass story_close — except we'll add the
	// verdict row through the LedgerAppend surface (not the ledger
	// store directly) so the substrate-side fallback runs.
	opts := happyOpts()
	opts.AppendVerdictRow = false // don't seed via the raw store
	f := newStoryCloseFixture(t, "improvement", opts)

	// Append the verdict row through the typed LedgerAppend surface,
	// passing the story binding ONLY via the tag.
	_, err := f.c.LedgerAppend(context.Background(), f.caller, LedgerAppendInput{
		ResolvedProjectID: f.projectID,
		WorkspaceID:       f.wsID,
		// StoryID intentionally omitted — the fallback must populate it.
		EventType: ledger.TypeVerdict,
		Content:   `{"rationale":"stub"}`,
		Tags: []string{
			"kind:verdict",
			"verdict:pass",
			"story_id:" + f.storyID,
			"task_id:" + f.reviewTask.ID,
		},
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceAgent,
		Now:        f.now.Add(4 * time.Minute),
	})
	require.NoError(t, err)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status, "tag-only verdict row must pass the story_close gate after the AC2 fallback; gaps=%+v", out.Gaps)
	assert.Equal(t, story.StatusDone, out.StoryStatus)
}

// TestStoryClose_FailDeployBehind — AC5. The fixture authors a
// kind:release-evidence row with one pushed_sha; the pprod override
// reports a different commit. The verb surfaces a `deploy:behind`
// gap whose Detail contains both SHAs, refuses to mutate, and does
// NOT append a close-evidence row.
func TestStoryClose_FailDeployBehind(t *testing.T) {
	opts := happyOpts()
	opts.ReleaseEvidenceSHA = "release-sha-aaa"
	opts.PprodCommitOverride = "pprod-sha-bbb"
	f := newStoryCloseFixture(t, "improvement", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	var dbGap *StoryCloseGap
	for i := range out.Gaps {
		if out.Gaps[i].Code == "deploy:behind" {
			dbGap = &out.Gaps[i]
		}
	}
	require.NotNil(t, dbGap, "deploy:behind gap missing: %+v", out.Gaps)
	assert.Contains(t, dbGap.Detail, "release-sha-aaa")
	assert.Contains(t, dbGap.Detail, "pprod-sha-bbb")
	assertStoryUnchanged(t, f)
}

// TestStoryClose_FailReleaseEvidenceAbsent — AC5 sentinel. With no
// kind:release-evidence row on the chain, the verb surfaces
// `release-evidence:absent` (so the operator sees *why* the deploy
// converge check could not run) and does not mutate.
func TestStoryClose_FailReleaseEvidenceAbsent(t *testing.T) {
	opts := happyOpts()
	opts.SkipReleaseEvidence = true
	f := newStoryCloseFixture(t, "improvement", opts)
	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	assertGap(t, out.Gaps, "release-evidence:absent", "")
	assertStoryUnchanged(t, f)
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

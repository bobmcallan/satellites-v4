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
	skipDeployCheck     bool
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
		SkipDeployCheck:     f.skipDeployCheck,
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
		storyRowTags := []string{
			"kind:release-evidence",
			"phase:merge_to_main",
			"pushed_sha:" + opts.ReleaseEvidenceSHA,
		}
		if opts.ReleaseEvidenceUnverified {
			storyRowTags = append(storyRowTags, "release:pushed_unverified")
		}
		_, err = ledStore.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: ws.ID,
			ProjectID:   "proj_test",
			StoryID:     ledger.StringPtr(st.ID),
			Type:        ledger.TypeEvidence,
			Tags:        storyRowTags,
			Content:     `## release-evidence stub`,
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			Status:      ledger.StatusActive,
			CreatedBy:   "u_dev",
		}, now.Add(5*time.Minute))
		require.NoError(t, err)
	}

	// sty_1478a1df fixture: extra project-scoped kind:release-evidence
	// rows seeded BEFORE / AFTER the story's own row to exercise the
	// ancestry-via-ledger-walk. Each entry creates a row at
	// now.Add(OffsetMin*time.Minute) carrying pushed_sha:<PushedSHA>;
	// Unverified=true adds the release:pushed_unverified tag the walk
	// excludes from tip candidacy.
	for _, extra := range opts.ExtraReleaseEvidence {
		tags := []string{
			"kind:release-evidence",
			"phase:merge_to_main",
			"pushed_sha:" + extra.PushedSHA,
		}
		if extra.Unverified {
			tags = append(tags, "release:pushed_unverified")
		}
		_, err = ledStore.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: ws.ID,
			ProjectID:   "proj_test",
			// No StoryID — these rows represent OTHER stories' releases
			// on the same project, the realistic shape the walk sees.
			Type:       ledger.TypeEvidence,
			Tags:       tags,
			Content:    `## extra release-evidence stub`,
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
			CreatedBy:  "u_dev",
		}, now.Add(time.Duration(extra.OffsetMin)*time.Minute))
		require.NoError(t, err)
	}

	c := New(Deps{
		Documents:     docStore,
		Stories:       storyStore,
		Ledger:        ledStore,
		Workspaces:    wsStore,
		Tasks:         taskStore,
		SelfProjectID: opts.SelfProjectID,
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
		skipDeployCheck:     opts.SkipDeployCheck,
	}
}

// storyCloseFixtureOpts toggles the gap each table case exercises.
// StoryReviewKind picks the kind of the minted contract:story_review
// task (sty_87148b8b): zero value falls back to task.KindReview to
// preserve the pre-existing cases; set to task.KindWork to exercise
// the canonical workflow shape the post-sty_b97dda00 substrate emits.
type storyCloseFixtureOpts struct {
	LeaveWorkOpen             bool
	SkipReviewTask            bool
	LeaveReviewOpen           bool
	AppendVerdictRow          bool
	VerdictPass               bool
	SeedTemplate              bool
	TemplateJSON              string
	Fields                    map[string]any
	StoryReviewKind           string
	SkipReleaseEvidence       bool
	ReleaseEvidenceSHA        string
	ReleaseEvidenceUnverified bool
	ExtraReleaseEvidence      []extraReleaseEvidence
	PprodCommitOverride       string
	SkipDeployCheck           bool
	SelfProjectID             string
}

// extraReleaseEvidence seeds an additional project-scoped
// kind:release-evidence row for the ancestry-walk tests (sty_1478a1df).
// OffsetMin is the minute offset from the fixture's `now` anchor; the
// fixture's own story-scoped row lands at +5min, so OffsetMin < 5 places
// a row before the story's release, OffsetMin > 5 places one after.
type extraReleaseEvidence struct {
	PushedSHA  string
	OffsetMin  int
	Unverified bool
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

// TestStoryClose_DeployBehind_AcceptsShortSHA — sty_1e2f7ae7 AC2.
// pprod's satellites_info returns the running commit truncated to
// the 8-char short form; the deploy:behind gate must accept it
// when it is a non-empty prefix of the full release SHA and is
// >= 7 chars (git's collision-safe floor). The close must walk
// through to PASS with no deploy:behind gap.
func TestStoryClose_DeployBehind_AcceptsShortSHA(t *testing.T) {
	opts := happyOpts()
	full := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	opts.ReleaseEvidenceSHA = full
	opts.PprodCommitOverride = full[:8] // "ae12fe78"
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status,
		"short pprod SHA prefixing the release SHA must pass deploy:behind; gaps=%+v", out.Gaps)
	for _, g := range out.Gaps {
		assert.NotEqual(t, "deploy:behind", g.Code,
			"deploy:behind gap must NOT appear on a converged short-SHA pprod; gap=%+v", g)
	}
}

// TestStoryClose_DeployBehind_FullSHAExactMatch — sty_1e2f7ae7 AC2.
// The non-truncated case still passes (a full-SHA pprod report is
// trivially a prefix of itself); the new predicate must not
// regress the existing exact-match acceptance.
func TestStoryClose_DeployBehind_FullSHAExactMatch(t *testing.T) {
	opts := happyOpts()
	full := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	opts.ReleaseEvidenceSHA = full
	opts.PprodCommitOverride = full
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status, "exact full-SHA match must pass; gaps=%+v", out.Gaps)
}

// TestStoryClose_DeployBehind_RejectsTooShortPrefix — sty_1e2f7ae7
// AC2 guard. A pprod commit shorter than 7 chars is collision-prone
// and MUST surface the deploy:behind gap even when it is a valid
// prefix of the release SHA.
func TestStoryClose_DeployBehind_RejectsTooShortPrefix(t *testing.T) {
	opts := happyOpts()
	full := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	opts.ReleaseEvidenceSHA = full
	opts.PprodCommitOverride = full[:6] // 6 chars, below floor
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	assertGap(t, out.Gaps, "deploy:behind", "")
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

// TestStoryClose_DeployBehind_PprodCommitMatch — sty_224774f0 AC1.
// Consumer-project flow: caller supplies pprod_commit equal to the
// release-evidence pushed_sha; the resolver short-circuits to the
// override and the deploy:behind gate passes regardless of any
// SelfProjectID configuration. Anchors the path-(b) "caller proves
// convergence" contract.
func TestStoryClose_DeployBehind_PprodCommitMatch(t *testing.T) {
	opts := happyOpts()
	full := "ae12fe781d27c7f677dd8e7479debad6aeb37875"
	opts.ReleaseEvidenceSHA = full
	opts.PprodCommitOverride = full
	// Explicitly leave SelfProjectID empty — the consumer flow must not
	// depend on satellites-self configuration when the caller proves
	// the commit.
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status,
		"caller-supplied pprod_commit matching release SHA must pass deploy:behind on a consumer project; gaps=%+v", out.Gaps)
	for _, g := range out.Gaps {
		assert.NotEqual(t, "deploy:behind", g.Code, "deploy:behind must not appear: %+v", g)
		assert.NotEqual(t, "release-evidence:no-deploy-endpoint", g.Code, "no-deploy-endpoint must not appear when pprod_commit supplied: %+v", g)
	}
}

// TestStoryClose_DeployBehind_PprodCommitMismatch — sty_224774f0 AC1.
// Caller-supplied pprod_commit that does NOT match the release SHA
// surfaces a deploy:behind gap citing both shas (regression coverage
// for the sty_1e2f7ae7 prefix-match comparator on the consumer-project
// path).
func TestStoryClose_DeployBehind_PprodCommitMismatch(t *testing.T) {
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

// TestStoryClose_DeployBehind_SatellitesSelfPath — sty_224774f0 AC4
// regression guard. With SelfProjectID matching the test story's
// project_id and no caller-supplied override, the resolver falls
// back to c.SatellitesInfo. SatellitesInfo at test time returns
// config.GitCommit ("unknown" in the test binary); the seeded
// release-evidence row carries the same value, so the gate passes.
// Locks the satellites-self path against future regressions.
func TestStoryClose_DeployBehind_SatellitesSelfPath(t *testing.T) {
	opts := happyOpts()
	opts.SelfProjectID = "proj_test"
	opts.PprodCommitOverride = "" // satellites-self must work without an override
	opts.ReleaseEvidenceSHA = "unknown"
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status,
		"satellites-self project must fall back to SatellitesInfo when SelfProjectID matches; gaps=%+v", out.Gaps)
	for _, g := range out.Gaps {
		assert.NotEqual(t, "release-evidence:no-deploy-endpoint", g.Code,
			"no-deploy-endpoint must not appear when SelfProjectID matches: %+v", g)
	}
}

// TestStoryClose_DeployBehind_NoDeployEndpoint — sty_224774f0 AC2 anchor.
// Consumer project (proj_test ≠ unconfigured SelfProjectID), no
// pprod_commit override, no skip_deploy_check → resolver returns
// ErrNoDeployEndpoint and the gate emits release-evidence:no-deploy-endpoint
// (NOT deploy:behind) so the operator sees the missing-configuration
// root cause. The Detail names the project_id and the two operator-
// fixable knobs.
func TestStoryClose_DeployBehind_NoDeployEndpoint(t *testing.T) {
	opts := happyOpts()
	opts.PprodCommitOverride = ""    // no caller proof
	opts.SelfProjectID = ""          // no satellites-self match
	opts.ReleaseEvidenceSHA = "release-sha-aaa"
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status)
	var sentinel *StoryCloseGap
	for i := range out.Gaps {
		if out.Gaps[i].Code == "release-evidence:no-deploy-endpoint" {
			sentinel = &out.Gaps[i]
		}
		assert.NotEqual(t, "deploy:behind", out.Gaps[i].Code,
			"deploy:behind must NOT fire on the consumer-no-config path; got %+v", out.Gaps[i])
	}
	require.NotNil(t, sentinel,
		"release-evidence:no-deploy-endpoint missing: %+v", out.Gaps)
	assert.Contains(t, sentinel.Detail, "proj_test")
	assert.Contains(t, sentinel.Detail, "pprod_commit")
	assert.Contains(t, sentinel.Detail, "skip_deploy_check")
	assertStoryUnchanged(t, f)
}

// TestStoryClose_DeployBehind_SkipDeployCheck — sty_224774f0 AC2
// opt-in opt-out. skip_deploy_check=true suppresses the deploy:behind
// and release-evidence:no-deploy-endpoint comparisons entirely; the
// gate proceeds even on a consumer project with no override and no
// SelfProjectID. release-evidence:absent still fires when no row
// exists (verified separately) — this test asserts the per-commit
// check is what's gated, not the row-presence one.
func TestStoryClose_DeployBehind_SkipDeployCheck(t *testing.T) {
	opts := happyOpts()
	opts.PprodCommitOverride = ""
	opts.SelfProjectID = ""
	opts.SkipDeployCheck = true
	opts.ReleaseEvidenceSHA = "release-sha-aaa"
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status,
		"skip_deploy_check=true must suppress the deploy comparison entirely; gaps=%+v", out.Gaps)
	for _, g := range out.Gaps {
		assert.NotEqual(t, "deploy:behind", g.Code, "deploy:behind must not appear when skip_deploy_check=true: %+v", g)
		assert.NotEqual(t, "release-evidence:no-deploy-endpoint", g.Code, "no-deploy-endpoint must not appear when skip_deploy_check=true: %+v", g)
	}
}

// TestStoryClose_DeployBehind_AncestorAccepted — sty_1478a1df AC1+AC2
// anchor. The story's release-evidence row is older than the project's
// pprod tip row (another story has since shipped past this one). Under
// the trunk-based ff-only flow the story's pushed_sha IS in pprod's
// history, so the gate must accept. Fails the pre-sty_1478a1df
// prefix-match comparator (releaseSHA != pprodCommit fast-path), passes
// the new ledger-walk ancestry comparator.
func TestStoryClose_DeployBehind_AncestorAccepted(t *testing.T) {
	opts := happyOpts()
	storySHA := "aaaaaaaa1111111111111111111111111111aaaa"
	tipSHA := "bbbbbbbb2222222222222222222222222222bbbb"
	opts.ReleaseEvidenceSHA = storySHA
	opts.PprodCommitOverride = tipSHA[:8] // short-form pprod report, prefix of tipSHA
	opts.ExtraReleaseEvidence = []extraReleaseEvidence{
		{PushedSHA: tipSHA, OffsetMin: 7}, // newer than story's row at +5min
	}
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "pass", out.Status,
		"story release row older than pprod tip row must pass deploy:behind via ancestry walk; gaps=%+v", out.Gaps)
	for _, g := range out.Gaps {
		assert.NotEqual(t, "deploy:behind", g.Code,
			"deploy:behind gap must not appear on the ancestry-accepted path: %+v", g)
	}
}

// TestStoryClose_DeployBehind_NonAncestorRejected — sty_1478a1df guard.
// pprod's reported commit doesn't appear on ANY converged
// release-evidence row in the project (divergent branch, or a commit
// the substrate never recorded). The ancestry walk cannot prove
// reachability, so the gate must reject with deploy:behind. Detail
// cites both the story release SHA and the unknown pprod commit so the
// operator sees the root cause.
func TestStoryClose_DeployBehind_NonAncestorRejected(t *testing.T) {
	opts := happyOpts()
	opts.ReleaseEvidenceSHA = "aaaaaaaa1111111111111111111111111111aaaa"
	opts.PprodCommitOverride = "deadbeefcafe9999" // no row carries this SHA
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
	assert.Contains(t, dbGap.Detail, "aaaaaaaa1111111111111111111111111111aaaa")
	assert.Contains(t, dbGap.Detail, "deadbeefcafe9999")
	assertStoryUnchanged(t, f)
}

// TestStoryClose_DeployBehind_UnverifiedSuppressed — sty_1478a1df rule.
// A release-evidence row tagged release:pushed_unverified is NOT a
// valid pprodTipRow — the row records that a push landed on origin but
// pprod's converge poll timed out. The walk skips such rows when
// looking for the project's pprod tip. With only an unverified row at
// the matching SHA, the walk produces no pprodTipRow and the gate
// rejects with deploy:behind. Locks the unverified-exclusion rule.
func TestStoryClose_DeployBehind_UnverifiedSuppressed(t *testing.T) {
	opts := happyOpts()
	unverifiedSHA := "cccccccc3333333333333333333333333333cccc"
	convergedOlderSHA := "dddddddd4444444444444444444444444444dddd"
	opts.ReleaseEvidenceSHA = convergedOlderSHA // story's own row (older, converged)
	opts.PprodCommitOverride = unverifiedSHA[:8]
	opts.ExtraReleaseEvidence = []extraReleaseEvidence{
		{PushedSHA: unverifiedSHA, OffsetMin: 7, Unverified: true},
	}
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status,
		"unverified row must not serve as pprodTipRow; gate must reject; gaps=%+v", out.Gaps)
	var dbGap *StoryCloseGap
	for i := range out.Gaps {
		if out.Gaps[i].Code == "deploy:behind" {
			dbGap = &out.Gaps[i]
		}
	}
	require.NotNil(t, dbGap, "deploy:behind gap missing: %+v", out.Gaps)
	assert.Contains(t, dbGap.Detail, unverifiedSHA[:8],
		"detail must cite the unmatched pprod commit so the operator sees the root cause: %+v", dbGap)
	assertStoryUnchanged(t, f)
}

// TestStoryClose_DeployBehind_PprodOlderThanRelease — sty_1478a1df
// "deploy genuinely behind" case. The story's release row is NEWER
// than the project's current pprod tip row (the story shipped but
// pprod hasn't rolled forward yet). The ancestry walk finds a tip but
// it precedes the story's row, so the gate must reject — pprod has not
// advanced past this release. Detail cites both the release SHA and
// the older pprod tip so the operator sees pprod is genuinely behind,
// not on a divergent branch.
func TestStoryClose_DeployBehind_PprodOlderThanRelease(t *testing.T) {
	opts := happyOpts()
	olderConvergedSHA := "eeeeeeee5555555555555555555555555555eeee"
	storyReleaseSHA := "ffffffff6666666666666666666666666666ffff"
	opts.ReleaseEvidenceSHA = storyReleaseSHA // story's row at +5min (newer)
	opts.PprodCommitOverride = olderConvergedSHA[:8]
	opts.ExtraReleaseEvidence = []extraReleaseEvidence{
		{PushedSHA: olderConvergedSHA, OffsetMin: 3}, // older converged row
	}
	f := newStoryCloseFixture(t, "improvement", opts)

	out, err := f.c.StoryClose(context.Background(), f.caller, f.closeInput())
	require.NoError(t, err)
	assert.Equal(t, "fail", out.Status,
		"story release row newer than pprod tip row must fail deploy:behind; gaps=%+v", out.Gaps)
	var dbGap *StoryCloseGap
	for i := range out.Gaps {
		if out.Gaps[i].Code == "deploy:behind" {
			dbGap = &out.Gaps[i]
		}
	}
	require.NotNil(t, dbGap, "deploy:behind gap missing: %+v", out.Gaps)
	assert.Contains(t, dbGap.Detail, storyReleaseSHA,
		"detail must cite the story's release SHA: %+v", dbGap)
	assert.Contains(t, dbGap.Detail, olderConvergedSHA,
		"detail must cite the older pprod tip SHA: %+v", dbGap)
	assert.Contains(t, dbGap.Detail, "newer than pprod tip",
		"detail must distinguish the genuine-behind case from the divergent-branch case: %+v", dbGap)
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

// Package client — `story_close` mechanical close verb (sty_b97dda00 slice 1).
//
// Structural-only verb. Reads the story's task chain + the story_review
// verdict ledger row + the resolved category template's `done` transition
// hooks, returns the gap list on failure, and on PASS appends a
// `kind:close-evidence` ledger row + walks the story to `done` via
// UpdateStatusDerived.
//
// The verb performs no LLM call, no shell-out, no agent dispatch — same
// tier as pr_story_terminal_gate's in-Go enforcement in
// internal/story/store.go.
//
// Contract resolution (sty_44fdf9f4). The gate's named steps live in
// the `story_close` contract prose at
// `config/seed/system/contracts/story_close.md`. The gate resolves
// the contract via `Documents.ResolveByName` (project-scope-first,
// workspace-scope, system-scope fallback) and records the resolved
// document id on the `kind:close-evidence` ledger row payload's
// `contract_id` field, so a project-scope override authored via
// `contract_add(scope=project, name=story_close, project_id=<…>)`
// becomes observable on the close-evidence row without any Go change.
// The structural invariants the gate enforces remain in Go — the
// contract body is the authoritative prose surface, kept in lock-step
// with this implementation per `pr_substrate_model` (configuration-
// over-code mandate, framing alias `pr_mandate_configuration_over_code`).

package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// storyCloseContractName is the document name the close gate resolves
// at close time. The same name is honoured at every scope tier — a
// project-scope row at this name overrides the system-scope seed
// without any Go change.
const storyCloseContractName = "story_close"

// StoryCloseInput names the story to close. ResolutionCode is the slot
// stored on the kind:close-evidence ledger row; empty falls back to
// "delivered" per the story_review contract's evidence_required prose.
//
// PprodCommitOverride is the caller-supplied pprod commit (the
// `pprod_commit` MCP arg). When non-empty it is the authoritative
// answer for the deploy:behind comparison and the resolver short-
// circuits to it. Consumer-project callers (vire, magentus-forge,
// resumere, …) supply this; for the satellites-self project the
// resolver falls back to c.SatellitesInfo when SelfProjectID is
// configured (sty_224774f0).
//
// SkipDeployCheck is the operator-supplied opt-out for the
// deploy:behind / release-evidence:no-deploy-endpoint comparison
// (the `skip_deploy_check` MCP arg). When true the gate emits no
// deploy-related gap regardless of resolver state. The
// release-evidence:absent gap still fires when no
// kind:release-evidence row exists — SkipDeployCheck only suppresses
// the per-commit comparison, not the row-presence gate.
type StoryCloseInput struct {
	StoryID             string
	ResolutionCode      string
	Memberships         []string
	Now                 time.Time
	PprodCommitOverride string
	SkipDeployCheck     bool
}

// StoryCloseGap is one cited gap the gate refused on. Code is the
// machine-readable category — one of:
//
//   - story_review:absent | story_review:open | story_review:fail
//   - chain:open_work
//   - template:<field>:missing
//   - deploy:behind                            (pprod commit lags release SHA)
//   - release-evidence:absent                  (no kind:release-evidence row)
//   - release-evidence:no-deploy-endpoint      (consumer project, no
//     pprod_commit, no skip_deploy_check, no SelfProjectID match —
//     sty_224774f0)
//
// Detail carries the row id(s), task id(s), or field name the gap
// resolves to.
type StoryCloseGap struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// StoryCloseOutput is the response envelope. Status is "pass" or
// "fail"; on "pass", the story walked to done and StoryStatus is the
// post-transition status; on "fail", Gaps is non-empty and no
// mutation occurred (StoryStatus matches pre-call state).
type StoryCloseOutput struct {
	Status      string          `json:"status"`
	StoryID     string          `json:"story_id"`
	StoryStatus string          `json:"story_status"`
	EvidenceID  string          `json:"evidence_id,omitempty"`
	Gaps        []StoryCloseGap `json:"gaps,omitempty"`
}

// Sentinel errors mapped by wire handlers to per-envelope shapes.
var (
	ErrStoryCloseStoryIDRequired = errors.New("story_id is required")
	ErrStoryCloseStoryNotFound   = errors.New("story not found")
	// ErrNoDeployEndpoint is the resolver's signal that the bound
	// project has no path to a pprod commit: no caller-supplied
	// pprod_commit, and the project_id does not match the
	// SelfProjectID configured for the satellites-self deployment.
	// The gate maps this to release-evidence:no-deploy-endpoint
	// rather than deploy:behind so consumer projects see the
	// missing-configuration root cause. sty_224774f0.
	ErrNoDeployEndpoint = errors.New("no deploy endpoint configured for project")
)

// resolutionCodeDefault is the slot recorded on the close-evidence row
// when the caller does not supply ResolutionCode.
const resolutionCodeDefault = "delivered"

// StoryClose is the mechanical close gate. On gap-free chain it appends
// a kind:close-evidence ledger row and walks the story to done via
// UpdateStatusDerived; on any gap it returns {status:"fail", gaps:[…]}
// without mutation.
func (c *Client) StoryClose(ctx context.Context, caller Caller, in StoryCloseInput) (StoryCloseOutput, error) {
	if c.deps.Stories == nil || c.deps.Tasks == nil || c.deps.Ledger == nil {
		return StoryCloseOutput{}, errors.New("story_close unavailable: required stores not configured")
	}
	if in.StoryID == "" {
		return StoryCloseOutput{}, ErrStoryCloseStoryIDRequired
	}
	memberships := in.Memberships
	if memberships == nil {
		memberships = c.ResolveCallerMemberships(ctx, caller)
	}
	st, err := c.deps.Stories.GetByID(ctx, in.StoryID, memberships)
	if err != nil {
		return StoryCloseOutput{}, ErrStoryCloseStoryNotFound
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if st.Status == story.StatusDone {
		return StoryCloseOutput{
			Status:      "fail",
			StoryID:     st.ID,
			StoryStatus: st.Status,
			Gaps:        []StoryCloseGap{{Code: "story:already_done"}},
		}, nil
	}

	tasks, err := c.deps.Tasks.List(ctx, task.ListOptions{StoryID: st.ID, Limit: 500}, memberships)
	if err != nil {
		return StoryCloseOutput{}, err
	}
	rows, err := c.deps.Ledger.List(ctx, st.ProjectID, ledger.ListOptions{StoryID: st.ID, Limit: ledger.MaxListLimit}, memberships)
	if err != nil {
		return StoryCloseOutput{}, err
	}

	var gaps []StoryCloseGap

	// AC3(c) — no open kind=work task on chain.
	var openWork []string
	for _, t := range tasks {
		if t.Kind != task.KindWork {
			continue
		}
		switch t.Status {
		case task.StatusPlanned, task.StatusPublished, task.StatusEnqueued, task.StatusClaimed, task.StatusInFlight:
			openWork = append(openWork, t.ID)
		}
	}
	if len(openWork) > 0 {
		gaps = append(gaps, StoryCloseGap{Code: "chain:open_work", Detail: strings.Join(openWork, ",")})
	}

	// AC3(a,b,e) — story_review task + verdict.
	reviewTask := latestStoryReviewTask(tasks)
	var verdictRow *ledger.LedgerEntry
	switch {
	case reviewTask == nil:
		gaps = append(gaps, StoryCloseGap{Code: "story_review:absent"})
	case reviewTask.Status != task.StatusClosed:
		gaps = append(gaps, StoryCloseGap{Code: "story_review:open", Detail: reviewTask.ID})
	default:
		verdictRow = latestVerdictRow(rows, reviewTask.ID)
		if verdictRow == nil {
			gaps = append(gaps, StoryCloseGap{Code: "story_review:fail", Detail: "no verdict row"})
		} else if !hasTag(verdictRow.Tags, "verdict:pass") {
			gaps = append(gaps, StoryCloseGap{Code: "story_review:fail", Detail: reviewTask.ID})
		}
	}

	// AC3(d) — template fields per the resolved infrastructure template.
	if tmpl, ok := c.loadStoryTemplate(ctx, st.Category); ok {
		// sty_4885dc89 — template-field carrier roll-up. A develop slice
		// (or any in-chain task) MAY emit a `kind:template-field:<name>`
		// ledger row carrying the value the parent story's template will
		// require at close time. We roll those rows up into `story.fields`
		// last-write-wins BEFORE running EvaluateTransition so the gap-
		// check reads the freshly-populated map. Carrier rows for field
		// names the resolved template does NOT declare are silently
		// ignored — no error, no gate; the contract prose documents which
		// names are valid for each category.
		declared := make(map[string]struct{}, len(tmpl.Fields))
		for _, f := range tmpl.Fields {
			declared[f.Name] = struct{}{}
		}
		seen := make(map[string]struct{})
		for i := range rows {
			name := templateFieldNameFromTags(rows[i].Tags)
			if name == "" {
				continue
			}
			if _, ok := declared[name]; !ok {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			updated, err := c.deps.Stories.SetField(ctx, st.ID, name, rows[i].Content, caller.UserID, now, memberships)
			if err != nil {
				return StoryCloseOutput{}, err
			}
			st = updated
		}
		ev := story.EvaluationContext{
			LedgerEntriesForStory: func(ctx context.Context, storyID string) ([]ledger.LedgerEntry, error) {
				return rows, nil
			},
		}
		for _, failure := range tmpl.EvaluateTransition(ctx, story.StatusDone, st, ev) {
			gaps = append(gaps, StoryCloseGap{Code: "template:" + fieldFromTemplateFailure(failure) + ":missing", Detail: failure})
		}
	}

	// deploy:behind — pprod's reported commit must match the pushed
	// SHA recorded on the latest kind:release-evidence row. The
	// merge_to_main contract authored that row at release time; this
	// gap is defense-in-depth catching pprod regressions BETWEEN
	// release and close. Sentinel `release-evidence:absent` surfaces
	// when no release-evidence row exists so the orchestrator sees
	// *why* the converge check could not run, rather than silently
	// passing. sty_224774f0: per-project resolver — caller-supplied
	// pprod_commit takes precedence; otherwise the satellites-self
	// project (matched by SelfProjectID) falls through to
	// SatellitesInfo; otherwise the substrate emits
	// release-evidence:no-deploy-endpoint so the operator sees the
	// missing-configuration root cause instead of a misleading
	// deploy:behind. SkipDeployCheck suppresses the comparison
	// entirely (release-evidence:absent still fires below).
	releaseRow := latestReleaseEvidenceRow(rows)
	switch {
	case releaseRow == nil:
		gaps = append(gaps, StoryCloseGap{Code: "release-evidence:absent"})
	default:
		releaseSHA := pushedSHAFromTags(releaseRow.Tags)
		if releaseSHA == "" {
			gaps = append(gaps, StoryCloseGap{Code: "release-evidence:absent", Detail: "row " + releaseRow.ID + " missing pushed_sha tag"})
			break
		}
		if in.SkipDeployCheck {
			break
		}
		pprodCommit, err := c.resolvePprodCommit(ctx, caller, st.ProjectID, in)
		switch {
		case errors.Is(err, ErrNoDeployEndpoint):
			gaps = append(gaps, StoryCloseGap{
				Code:   "release-evidence:no-deploy-endpoint",
				Detail: "project_id=" + st.ProjectID + " has no deploy endpoint; supply pprod_commit or skip_deploy_check",
			})
		case err != nil:
			gaps = append(gaps, StoryCloseGap{Code: "deploy:behind", Detail: "satellites_info error: " + err.Error()})
		case pprodCommit == "" || len(pprodCommit) < 7:
			gaps = append(gaps, StoryCloseGap{Code: "deploy:behind", Detail: releaseSHA + " != " + pprodCommit})
		case strings.HasPrefix(releaseSHA, pprodCommit):
			// fast path — pprod has converged at the story's release commit
			// (sty_1e2f7ae7 prefix-match). No ancestry walk needed.
		default:
			// sty_1478a1df slow path — pprod's reported commit is not the
			// story's release commit, but it MAY be a descendant of it
			// (a later release in the same project superseded the story's
			// release, which is the common case under the trunk-based
			// ff-only flow). Walk project-scoped kind:release-evidence
			// rows newest-first to find pprodTipRow — the most recent
			// CONVERGED row whose pushed_sha prefix-matches pprodCommit.
			// release:pushed_unverified rows are excluded from tip
			// candidacy (they represent pushes that landed on origin but
			// never converged). Accept iff storyReleaseRow.CreatedAt <=
			// pprodTipRow.CreatedAt — main is ff-only, so an older row's
			// pushed_sha is necessarily in pprod's history.
			projectReleases, listErr := c.deps.Ledger.List(ctx, st.ProjectID, ledger.ListOptions{
				Tags:  []string{"kind:release-evidence"},
				Limit: ledger.MaxListLimit,
			}, memberships)
			if listErr != nil {
				return StoryCloseOutput{}, listErr
			}
			var pprodTipRow *ledger.LedgerEntry
			for i := range projectReleases {
				r := &projectReleases[i]
				if hasTag(r.Tags, "release:pushed_unverified") {
					continue
				}
				rowSHA := pushedSHAFromTags(r.Tags)
				if rowSHA == "" {
					continue
				}
				if !strings.HasPrefix(rowSHA, pprodCommit) {
					continue
				}
				pprodTipRow = r
				break // MemoryStore.List + SurrealStore.List both sort newest-first
			}
			switch {
			case pprodTipRow == nil:
				gaps = append(gaps, StoryCloseGap{
					Code:   "deploy:behind",
					Detail: releaseSHA + " not reachable from pprod " + pprodCommit + " (no converged kind:release-evidence row in project " + st.ProjectID + ")",
				})
			case releaseRow.CreatedAt.After(pprodTipRow.CreatedAt):
				gaps = append(gaps, StoryCloseGap{
					Code:   "deploy:behind",
					Detail: releaseSHA + " is newer than pprod tip " + pushedSHAFromTags(pprodTipRow.Tags) + " (pprod=" + pprodCommit + ")",
				})
			}
		}
	}

	if len(gaps) > 0 {
		return StoryCloseOutput{
			Status:      "fail",
			StoryID:     st.ID,
			StoryStatus: st.Status,
			Gaps:        gaps,
		}, nil
	}

	resolution := strings.TrimSpace(in.ResolutionCode)
	if resolution == "" {
		resolution = resolutionCodeDefault
	}
	contractID := c.resolveStoryCloseContract(ctx, st.WorkspaceID, st.ProjectID, memberships)
	payload := map[string]any{
		"resolution":     resolution,
		"review_task_id": reviewTask.ID,
		"verdict_row_id": verdictRow.ID,
		"chain_size":     len(tasks),
		"contract_id":    contractID,
	}
	body, _ := json.Marshal(payload)
	evidence, err := c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: st.WorkspaceID,
		ProjectID:   st.ProjectID,
		StoryID:     ledger.StringPtr(st.ID),
		Type:        ledger.TypeEvidence,
		Tags:        []string{"kind:close-evidence", "resolution:" + resolution},
		Content:     string(body),
		Durability:  ledger.DurabilityDurable,
		SourceType:  ledger.SourceSystem,
		Status:      ledger.StatusActive,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return StoryCloseOutput{}, err
	}
	updated, err := c.deps.Stories.UpdateStatusDerived(ctx, st.ID, story.StatusDone, now, memberships)
	if err != nil {
		return StoryCloseOutput{}, err
	}
	return StoryCloseOutput{
		Status:      "pass",
		StoryID:     updated.ID,
		StoryStatus: updated.Status,
		EvidenceID:  evidence.ID,
	}, nil
}

// latestStoryReviewTask returns the most recently created
// contract:story_review task on the chain regardless of Kind (sty_87148b8b).
//
// The canonical post-sty_b97dda00 workflow mints the story_review task as
// kind=work (workflow doc + story_reviewer.delivers both declare it). The
// historical tactical replay used during sty_8f99ef39 minted the same task
// as kind=review. Either shape is now valid for the close gate — the
// invariant the handler enforces is "the chain carries a closed
// contract:story_review task whose verdict row is verdict:pass", not the
// task's kind. The narrower kind=review-only filter that previously lived
// here is gone (no compat alias).
func latestStoryReviewTask(tasks []task.Task) *task.Task {
	var picked *task.Task
	for i := range tasks {
		t := &tasks[i]
		if t.Action != "contract:story_review" {
			continue
		}
		if picked == nil || t.CreatedAt.After(picked.CreatedAt) {
			picked = t
		}
	}
	return picked
}

// latestVerdictRow finds the latest kind:verdict ledger row tagged with
// task_id:<reviewTaskID>; nil when no such row exists.
func latestVerdictRow(rows []ledger.LedgerEntry, reviewTaskID string) *ledger.LedgerEntry {
	taskTag := "task_id:" + reviewTaskID
	var picked *ledger.LedgerEntry
	for i := range rows {
		r := &rows[i]
		if !hasTag(r.Tags, "kind:verdict") || !hasTag(r.Tags, taskTag) {
			continue
		}
		if picked == nil || r.CreatedAt.After(picked.CreatedAt) {
			picked = r
		}
	}
	return picked
}

// hasTag returns true when tags contains the literal tag value.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// latestReleaseEvidenceRow finds the most-recent kind:release-evidence
// ledger row from the supplied per-story rows; nil when none exist.
// The merge_to_main contract authors exactly one row per release; on a
// well-formed chain the latest row IS the row covering the chain's
// release.
func latestReleaseEvidenceRow(rows []ledger.LedgerEntry) *ledger.LedgerEntry {
	var picked *ledger.LedgerEntry
	for i := range rows {
		r := &rows[i]
		if !hasTag(r.Tags, "kind:release-evidence") {
			continue
		}
		if picked == nil || r.CreatedAt.After(picked.CreatedAt) {
			picked = r
		}
	}
	return picked
}

// templateFieldNameFromTags extracts the `<name>` from the first
// `kind:template-field:<name>` tag in tags. Empty string when no
// such tag is present. sty_4885dc89 — the close gate's roll-up uses
// this to project a ledger row onto the corresponding story.fields
// entry via story.Store.SetField.
func templateFieldNameFromTags(tags []string) string {
	const prefix = "kind:template-field:"
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(t, prefix))
		}
	}
	return ""
}

// pushedSHAFromTags extracts the value following the `pushed_sha:`
// prefix from a tag slice; empty when no such tag is present.
func pushedSHAFromTags(tags []string) string {
	const prefix = "pushed_sha:"
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(t, prefix))
		}
	}
	return ""
}

// resolvePprodCommit returns the pprod commit the deploy:behind
// gate compares against the release-evidence pushed_sha. The
// resolver branches in priority order (sty_224774f0):
//
//  1. Caller-supplied PprodCommitOverride wins for any project. The
//     consumer-project flow (vire, magentus-forge, resumere, …)
//     proves convergence by passing the deployed commit explicitly.
//  2. The satellites-self project (projectID == c.deps.SelfProjectID,
//     when SelfProjectID is non-empty) falls back to c.SatellitesInfo,
//     which reports this server's own running commit. This is the
//     pre-sty_224774f0 behaviour, scoped to the project the
//     deployment dogfoods against.
//  3. Otherwise the resolver returns ErrNoDeployEndpoint and the
//     gate emits release-evidence:no-deploy-endpoint so the operator
//     sees the missing-configuration root cause instead of a
//     misleading deploy:behind.
func (c *Client) resolvePprodCommit(ctx context.Context, caller Caller, projectID string, in StoryCloseInput) (string, error) {
	if in.PprodCommitOverride != "" {
		return in.PprodCommitOverride, nil
	}
	if c.deps.SelfProjectID != "" && c.deps.SelfProjectID == projectID {
		out, err := c.SatellitesInfo(ctx, caller, SatellitesInfoInput{})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out.Server.Commit), nil
	}
	return "", ErrNoDeployEndpoint
}

// resolveStoryCloseContract returns the document id of the
// `story_close` contract the gate cites on the kind:close-evidence
// row. Walks project → workspace → system via the shared
// Documents.ResolveByName tier resolver — the same precedence the
// rest of the substrate uses for contract / agent / workflow
// lookups. Returns "" when no contract row exists (the gate
// tolerates a missing contract; the structural invariants live in
// Go and the close still proceeds). Type-checked: only documents
// whose Type is `contract` are accepted, so a name collision with a
// non-contract row cannot poison the gate's evidence trail.
//
// sty_44fdf9f4 — the contract_id field on the close-evidence
// payload makes the project-scope override path observable from the
// ledger row alone (the operator sees which contract prose the gate
// cited, and a project-scope row's id differs from the system-scope
// row's id). The named gate steps remain Go invariants; the
// contract body is the authoritative prose surface, kept in
// lock-step per pr_substrate_model.
func (c *Client) resolveStoryCloseContract(ctx context.Context, workspaceID, projectID string, memberships []string) string {
	if c.deps.Documents == nil {
		return ""
	}
	doc, err := c.deps.Documents.ResolveByName(
		ctx,
		document.TypeContract,
		storyCloseContractName,
		workspaceID,
		projectID,
		memberships,
	)
	if err != nil {
		return ""
	}
	return doc.ID
}

// fieldFromTemplateFailure extracts the field name from a template's
// EvaluateTransition failure string. The hook's message format is
// `required field "scope" is empty — set it via story_update …`; we
// pick the first quoted token. Falls back to "unknown" so the gap
// code is still well-formed when the message changes.
func fieldFromTemplateFailure(msg string) string {
	open := strings.Index(msg, "\"")
	if open < 0 {
		return "unknown"
	}
	rest := msg[open+1:]
	close := strings.Index(rest, "\"")
	if close <= 0 {
		return "unknown"
	}
	return rest[:close]
}

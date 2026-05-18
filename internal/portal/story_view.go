// Story-view composite builder. Pulls the story-level panels described
// in docs/ui-design.md §2.2 — source docs / reviewer verdicts / repo
// provenance / ledger excerpts / activity / delivery strip — into one
// struct so the SSR template and the JSON composite endpoint render
// from the same shape.
//
// sty_a03449d1 wired TaskChain to the task store so the project-detail
// stories panel and the /stories/{id} composite both render the live
// task chain (work + review siblings, successor tasks minted with
// prior_task_id).
package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// excerptLimit caps the ledger-excerpts panel; older rows fall off the
// view (still queryable via the full ledger inspection page).
const excerptLimit = 50

// commitTagKind identifies the ledger rows the repo-provenance panel
// reads. The emitter that writes these rows is a follow-up story; the
// panel renders an empty-state until those rows exist.
const commitTagKind = "kind:commit"

// verdictTagKind identifies reviewer-verdict ledger rows written by
// the reviewer service.
const verdictTagKind = "kind:verdict"

// distinctLedgerKinds names the kind:* tag values the story-detail
// timeline styles distinctly.
var distinctLedgerKinds = map[string]string{
	"kind:plan-amend":              "plan-amend",
	"kind:agent-compose":           "agent-compose",
	"kind:agent-archive":           "agent-archive",
	"kind:session-default-install": "session-default-install",
}

// storyComposite is the view-model for the story view.
type storyComposite struct {
	Story         storyRow           `json:"story"`
	SourceDocs    []sourceDocLink    `json:"source_documents"`
	TaskChain     []taskChainCard    `json:"task_chain"`
	Verdicts      []verdictCard      `json:"verdicts"`
	Commits       []commitCard       `json:"commits"`
	Excerpts      []ledgerExcerpt    `json:"ledger_excerpts"`
	Activity      []storyActivityRow `json:"activity"`
	ActivityKinds []string           `json:"activity_kinds"`
	Delivery      deliveryStrip      `json:"delivery"`
	// ContextAudit aggregates kind:context-fetch ledger rows scoped to
	// this story into the three sub-section view-model the
	// _panel_context_audit.html template renders. sty_9f658001 slice 2.
	ContextAudit contextAuditPanel `json:"context_audit"`
}

// contextAuditPanel is the view-model for the Context audit panel —
// summary counters + violations table + top-sections table. Populated
// by buildContextAudit from kind:context-fetch ledger rows.
type contextAuditPanel struct {
	Summary    contextAuditSummary       `json:"summary"`
	Violations []contextAuditViolation   `json:"violations"`
	TopSections []contextAuditTopSection `json:"top_sections"`
}

// contextAuditSummary is the top header of the Context audit panel.
type contextAuditSummary struct {
	Fetches       int `json:"fetches"`
	UniqueVerbs   int `json:"unique_verbs"`
	TotalBytes    int `json:"total_bytes"`
	R1FailCount   int `json:"r1_fail_count"`
}

// contextAuditViolation is one R1-violating context-fetch row.
type contextAuditViolation struct {
	LedgerID  string   `json:"ledger_id"`
	Verb      string   `json:"verb"`
	CreatedAt string   `json:"created_at"`
	Sections  []string `json:"sections"`
	Refs      []string `json:"refs"`
}

// contextAuditTopSection aggregates fetches by section name.
type contextAuditTopSection struct {
	Name       string   `json:"name"`
	TotalBytes int      `json:"total_bytes"`
	Fetches    int      `json:"fetches"`
	Scopes     []string `json:"scopes"`
}

// sourceDocLink is the source-documents-panel view-model.
type sourceDocLink struct {
	Tag     string `json:"tag"`
	Path    string `json:"path"`
	Anchor  string `json:"anchor"`
	Display string `json:"display"`
}

// taskChainCard is one task on the story's chain rendered in
// /stories panels. Sequence is the 1-based row index in
// created_at order; Iteration is the lap among same-action peers
// (1 for the first attempt, >1 for successor tasks minted with
// prior_task_id). VerdictExcerpt carries a short preview of the
// verdict ledger row when one exists for the row's task_id.
type taskChainCard struct {
	ID             string   `json:"id"`
	Sequence       int      `json:"sequence"`
	Kind           string   `json:"kind"`
	Action         string   `json:"action,omitempty"`
	ContractName   string   `json:"contract_name,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Status         string   `json:"status"`
	Outcome        string   `json:"outcome,omitempty"`
	Iteration      int      `json:"iteration,omitempty"`
	ClaimedBy      string   `json:"claimed_by,omitempty"`
	ParentTaskID   string   `json:"parent_task_id,omitempty"`
	PriorTaskID    string   `json:"prior_task_id,omitempty"`
	Duration       string   `json:"duration,omitempty"`
	CreatedAt      string   `json:"created_at"`
	ClaimedAt      string   `json:"claimed_at,omitempty"`
	CompletedAt    string   `json:"completed_at,omitempty"`
	UpdatedAt      string   `json:"updated_at"`
	VerdictExcerpt string   `json:"verdict_excerpt,omitempty"`
}

// verdictCard is one reviewer-verdict row scoped to this story. The
// task_id tag (set by the reviewer service) carries the originating
// review task; ContractName comes from the `phase:<name>` tag.
type verdictCard struct {
	LedgerID     string `json:"ledger_id"`
	TaskID       string `json:"task_id,omitempty"`
	ContractName string `json:"contract_name,omitempty"`
	Verdict      string `json:"verdict,omitempty"`
	Score        int    `json:"score,omitempty"`
	Reasoning    string `json:"reasoning,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// commitCard is one commit linked to this story (via `kind:commit`
// ledger rows). Empty list → panel renders the empty-state copy.
type commitCard struct {
	LedgerID  string `json:"ledger_id"`
	SHA       string `json:"sha,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Author    string `json:"author,omitempty"`
	URL       string `json:"url,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ledgerExcerpt is one row in the bounded ledger-excerpts panel.
type ledgerExcerpt struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Tags      []string `json:"tags,omitempty"`
	Content   string   `json:"content,omitempty"`
	CreatedAt string   `json:"created_at"`
	KindClass string   `json:"kind_class,omitempty"`
}

// deliveryStrip is the banner at the top of the page. Resolution is
// drawn from the most recent kind:verdict row whose phase is
// `phase:story_close`, mirroring the lifecycle's terminal review.
type deliveryStrip struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution,omitempty"`
	Verdict    string `json:"verdict,omitempty"`
	Score      int    `json:"score,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// buildStoryComposite assembles the composite for storyID. Any nil
// store gracefully degrades (the corresponding panel renders empty).
// memberships scopes every read identically to the existing handler
// surface so cross-workspace requests stay 404-equivalent.
func buildStoryComposite(
	ctx context.Context,
	stories story.Store,
	docs document.Store,
	ledgerStore ledger.Store,
	tasks task.Store,
	storyID string,
	memberships []string,
) (storyComposite, error) {
	s, err := stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		return storyComposite{}, err
	}
	c := storyComposite{
		Story:      viewStoryRow(s),
		SourceDocs: sourceDocsForStory(s),
		TaskChain:  []taskChainCard{},
		Delivery:   deliveryStrip{Status: s.Status, UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339)},
	}
	_ = docs

	if ledgerStore != nil {
		c.Verdicts = verdictsForStory(ctx, ledgerStore, s.ProjectID, storyID, memberships)
		c.Commits = commitsForStory(ctx, ledgerStore, s.ProjectID, storyID, memberships)
		c.Excerpts = excerptsForStory(ctx, ledgerStore, s.ProjectID, storyID, memberships)
		c.ActivityKinds = resolveStoryActivityKinds(ctx, ledgerStore, s.WorkspaceID, s.ProjectID, memberships)
		c.Activity = buildStoryActivity(ctx, ledgerStore, s.ProjectID, storyID, c.ActivityKinds, memberships)
		c.Delivery = applyDeliveryVerdict(c.Delivery, c.Verdicts)
		c.ContextAudit = buildContextAudit(ctx, ledgerStore, s.ProjectID, storyID, memberships)
	}
	if tasks != nil {
		c.TaskChain = taskChainForStory(ctx, tasks, ledgerStore, s.ProjectID, storyID, memberships)
	}

	return c, nil
}

// taskChainForStory pulls every task scoped to storyID, orders by
// created_at, then projects to view-model with iteration + verdict
// excerpt populated. Verdict excerpts are best-effort; absence does
// not error out the view.
func taskChainForStory(ctx context.Context, tasks task.Store, ledgerStore ledger.Store, projectID, storyID string, memberships []string) []taskChainCard {
	rows, err := tasks.List(ctx, task.ListOptions{StoryID: storyID, Limit: 500}, memberships)
	if err != nil || len(rows) == 0 {
		return []taskChainCard{}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	verdicts := verdictExcerptsByTask(ctx, ledgerStore, projectID, memberships)
	now := time.Now().UTC()
	out := make([]taskChainCard, 0, len(rows))
	for i, t := range rows {
		card := taskChainCard{
			ID:             t.ID,
			Sequence:       i + 1,
			Kind:           t.Kind,
			Action:         t.Action,
			ContractName:   contractNameFromAction(t.Action),
			Status:         t.Status,
			Outcome:        t.Outcome,
			Iteration:      walkIterationForTask(t, rows),
			ClaimedBy:      t.ClaimedBy,
			ParentTaskID:   t.ParentTaskID,
			PriorTaskID:    t.PriorTaskID,
			CreatedAt:      t.CreatedAt.UTC().Format(time.RFC3339),
			VerdictExcerpt: verdicts[t.ID],
		}
		if t.ClaimedAt != nil {
			card.ClaimedAt = t.ClaimedAt.UTC().Format(time.RFC3339)
		}
		if t.CompletedAt != nil {
			card.CompletedAt = t.CompletedAt.UTC().Format(time.RFC3339)
		}
		card.Duration = humaniseTaskDuration(now, t)
		card.UpdatedAt = taskUpdatedAt(t).UTC().Format(time.RFC3339)
		card.Tags = buildTaskChainTags(card)
		out = append(out, card)
	}
	return out
}

// humaniseTaskDuration formats a coarse wall-clock string sized by the
// task's current lifecycle state: queue-wait for enqueued/published,
// run-time-so-far for claimed/in_flight, total run time for closed.
func humaniseTaskDuration(now time.Time, t task.Task) string {
	var d time.Duration
	switch t.Status {
	case task.StatusEnqueued, task.StatusPublished:
		d = now.Sub(t.CreatedAt)
	case task.StatusClaimed, task.StatusInFlight:
		if t.ClaimedAt == nil {
			return ""
		}
		d = now.Sub(*t.ClaimedAt)
	case task.StatusClosed:
		if t.CompletedAt == nil {
			return ""
		}
		// Prefer claim → complete (actual work time). Tasks that close
		// without being claimed (substrate-driven auto-close on review /
		// story_review contracts) fall back to created → complete so
		// every closed row reports a number.
		start := t.CreatedAt
		if t.ClaimedAt != nil {
			start = *t.ClaimedAt
		}
		d = t.CompletedAt.Sub(start)
	default:
		return ""
	}
	if d < 0 {
		return ""
	}
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// taskUpdatedAt returns the most recent state-change timestamp:
// CompletedAt > ClaimedAt > CreatedAt.
func taskUpdatedAt(t task.Task) time.Time {
	if t.CompletedAt != nil {
		return *t.CompletedAt
	}
	if t.ClaimedAt != nil {
		return *t.ClaimedAt
	}
	return t.CreatedAt
}

// buildTaskChainTags returns the key:value chip list rendered inside
// the task-row's title cell. Tasks have no native title field —
// the chip row IS the title.
func buildTaskChainTags(c taskChainCard) []string {
	tags := make([]string, 0, 4)
	if c.Kind != "" {
		tags = append(tags, "kind:"+c.Kind)
	}
	if c.ContractName != "" {
		tags = append(tags, "action:"+c.ContractName)
	} else if c.Action != "" {
		tags = append(tags, "action:"+c.Action)
	}
	if c.Iteration > 1 {
		tags = append(tags, fmt.Sprintf("iter:%d", c.Iteration))
	}
	if c.Outcome != "" {
		tags = append(tags, "outcome:"+c.Outcome)
	}
	return tags
}

// verdictExcerptsByTask scans kind:verdict ledger rows and indexes
// the short reasoning by the task_id tag they carry. Empty map when
// the store is nil or unreachable.
func verdictExcerptsByTask(ctx context.Context, ledgerStore ledger.Store, projectID string, memberships []string) map[string]string {
	out := map[string]string{}
	if ledgerStore == nil || projectID == "" {
		return out
	}
	rows, err := ledgerStore.List(ctx, projectID, ledger.ListOptions{
		Tags:  []string{verdictTagKind},
		Limit: ledger.MaxListLimit,
	}, memberships)
	if err != nil {
		return out
	}
	for _, r := range rows {
		var taskID string
		for _, t := range r.Tags {
			if id, ok := taskIDFromTag(t); ok {
				taskID = id
				break
			}
		}
		if taskID == "" {
			continue
		}
		if _, dup := out[taskID]; dup {
			continue
		}
		excerpt := r.Content
		if len(r.Structured) > 0 {
			var payload struct {
				Reasoning string `json:"reasoning"`
			}
			if json.Unmarshal(r.Structured, &payload) == nil && payload.Reasoning != "" {
				excerpt = payload.Reasoning
			}
		}
		out[taskID] = truncate(excerpt, 160)
	}
	return out
}

// sourceDocsForStory parses `source:` tags on the story into
// sourceDocLink rows. The tag convention is `source:<path>` with an
// optional `#anchor` fragment, e.g. `source:ui-design.md#story-view`.
func sourceDocsForStory(s story.Story) []sourceDocLink {
	out := make([]sourceDocLink, 0)
	for _, t := range s.Tags {
		if !strings.HasPrefix(t, "source:") {
			continue
		}
		raw := strings.TrimPrefix(t, "source:")
		if raw == "" {
			continue
		}
		path, anchor := raw, ""
		if i := strings.IndexByte(raw, '#'); i >= 0 {
			path = raw[:i]
			anchor = raw[i+1:]
		}
		display := path
		if anchor != "" {
			display = path + " §" + anchor
		}
		out = append(out, sourceDocLink{
			Tag:     t,
			Path:    path,
			Anchor:  anchor,
			Display: display,
		})
	}
	return out
}

// verdictsForStory pulls the kind:verdict ledger rows for the story,
// newest-first.
func verdictsForStory(ctx context.Context, store ledger.Store, projectID, storyID string, memberships []string) []verdictCard {
	rows, err := store.List(ctx, projectID, ledger.ListOptions{
		StoryID:       storyID,
		Tags:          []string{verdictTagKind},
		IncludeDerefd: true,
		Limit:         ledger.MaxListLimit,
	}, memberships)
	if err != nil {
		return nil
	}
	out := make([]verdictCard, 0, len(rows))
	for _, r := range rows {
		card := verdictCard{
			LedgerID:  r.ID,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		}
		for _, t := range r.Tags {
			switch {
			case strings.HasPrefix(t, "phase:"):
				card.ContractName = strings.TrimPrefix(t, "phase:")
			case strings.HasPrefix(t, "task_id:"):
				card.TaskID = strings.TrimPrefix(t, "task_id:")
			}
		}
		var payload struct {
			Verdict   string `json:"verdict"`
			Score     int    `json:"score"`
			Reasoning string `json:"reasoning"`
		}
		if len(r.Structured) > 0 {
			_ = json.Unmarshal(r.Structured, &payload)
			card.Verdict = payload.Verdict
			card.Score = payload.Score
			card.Reasoning = payload.Reasoning
		}
		if card.Reasoning == "" {
			card.Reasoning = r.Content
		}
		out = append(out, card)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// commitsForStory pulls kind:commit ledger rows scoped to the story.
func commitsForStory(ctx context.Context, store ledger.Store, projectID, storyID string, memberships []string) []commitCard {
	rows, err := store.List(ctx, projectID, ledger.ListOptions{
		StoryID: storyID,
		Tags:    []string{commitTagKind},
		Limit:   ledger.MaxListLimit,
	}, memberships)
	if err != nil {
		return nil
	}
	out := make([]commitCard, 0, len(rows))
	for _, r := range rows {
		card := commitCard{
			LedgerID:  r.ID,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		}
		var payload struct {
			SHA     string `json:"sha"`
			Subject string `json:"subject"`
			Author  string `json:"author"`
			URL     string `json:"url"`
		}
		if len(r.Structured) > 0 {
			_ = json.Unmarshal(r.Structured, &payload)
			card.SHA = payload.SHA
			card.Subject = payload.Subject
			card.Author = payload.Author
			card.URL = payload.URL
		}
		if card.Subject == "" {
			card.Subject = r.Content
		}
		out = append(out, card)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// excerptsForStory pulls a bounded window of all ledger rows scoped to
// the story (any tag).
func excerptsForStory(ctx context.Context, store ledger.Store, projectID, storyID string, memberships []string) []ledgerExcerpt {
	rows, err := store.List(ctx, projectID, ledger.ListOptions{
		StoryID: storyID,
		Limit:   excerptLimit,
	}, memberships)
	if err != nil {
		return nil
	}
	out := make([]ledgerExcerpt, 0, len(rows))
	for _, r := range rows {
		out = append(out, ledgerExcerpt{
			ID:        r.ID,
			Type:      r.Type,
			Tags:      r.Tags,
			Content:   truncate(r.Content, 240),
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
			KindClass: ledgerKindClass(r.Tags),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// ledgerKindClass returns the CSS suffix for the distinct lifecycle
// kinds.
func ledgerKindClass(tags []string) string {
	for _, t := range tags {
		if cls, ok := distinctLedgerKinds[t]; ok {
			return cls
		}
	}
	return ""
}

// applyDeliveryVerdict folds the most recent story_close verdict into
// the delivery strip.
func applyDeliveryVerdict(strip deliveryStrip, verdicts []verdictCard) deliveryStrip {
	for _, v := range verdicts {
		if v.ContractName != "story_close" {
			continue
		}
		strip.Verdict = v.Verdict
		strip.Score = v.Score
		strip.UpdatedAt = v.CreatedAt
		switch v.Verdict {
		case "approved":
			strip.Resolution = "delivered"
		case "rejected":
			strip.Resolution = "failed"
		case "needs_changes", "amended":
			strip.Resolution = "partially_delivered"
		}
		break
	}
	return strip
}

// truncate clips s to maxRunes, appending an ellipsis when clipped.
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// contextAuditPanelLimit caps the per-story window. Older rows are
// queryable via the full ledger inspection page.
const contextAuditPanelLimit = 200

// contextAuditViolationLimit caps the violations table rendered in the
// panel; older violations roll off the visible window.
const contextAuditViolationLimit = 25

// contextAuditTopSectionLimit caps the top-sections table.
const contextAuditTopSectionLimit = 10

// buildContextAudit fetches kind:context-fetch rows scoped to storyID,
// unmarshals each row's structured payload, and aggregates into the
// three sub-section view-models. Aggregation lives here, not on the
// substrate — the rows are the read primitive.
//
// sty_9f658001 slice 2: portal-side surface for the per-story drift
// surface. Substrate-emitted rows (kind:context-fetch) are
// substrate-emitted (cf development_reviewer.md §7a) — agent close
// evidence does not count these rows.
func buildContextAudit(ctx context.Context, ledgerStore ledger.Store, projectID, storyID string, memberships []string) contextAuditPanel {
	panel := contextAuditPanel{
		Violations:  []contextAuditViolation{},
		TopSections: []contextAuditTopSection{},
	}
	if ledgerStore == nil || projectID == "" || storyID == "" {
		return panel
	}
	rows, err := ledgerStore.List(ctx, projectID, ledger.ListOptions{
		StoryID: storyID,
		Tags:    []string{"kind:context-fetch"},
		Limit:   contextAuditPanelLimit,
	}, memberships)
	if err != nil {
		return panel
	}

	type aggSection struct {
		bytes   int
		fetches int
		scopes  map[string]struct{}
	}
	verbs := map[string]struct{}{}
	sections := map[string]*aggSection{}
	totalBytes := 0
	r1FailCount := 0

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})

	for _, r := range rows {
		var payload struct {
			Verb       string `json:"verb"`
			TotalBytes int    `json:"total_bytes"`
			Sections   []struct {
				Name        string `json:"name"`
				Bytes       int    `json:"bytes"`
				OriginScope string `json:"origin_scope"`
			} `json:"sections"`
			Rules struct {
				R1 struct {
					Violations []struct {
						Section string   `json:"section"`
						Scope   string   `json:"scope"`
						Refs    []string `json:"refs"`
					} `json:"violations"`
				} `json:"r1"`
			} `json:"rules"`
		}
		if len(r.Structured) == 0 {
			continue
		}
		if err := json.Unmarshal(r.Structured, &payload); err != nil {
			continue
		}
		verbs[payload.Verb] = struct{}{}
		totalBytes += payload.TotalBytes
		for _, sec := range payload.Sections {
			agg, ok := sections[sec.Name]
			if !ok {
				agg = &aggSection{scopes: map[string]struct{}{}}
				sections[sec.Name] = agg
			}
			agg.bytes += sec.Bytes
			agg.fetches++
			agg.scopes[sec.OriginScope] = struct{}{}
		}
		if len(payload.Rules.R1.Violations) == 0 {
			continue
		}
		r1FailCount++
		if len(panel.Violations) >= contextAuditViolationLimit {
			continue
		}
		viol := contextAuditViolation{
			LedgerID:  r.ID,
			Verb:      payload.Verb,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
			Sections:  make([]string, 0, len(payload.Rules.R1.Violations)),
			Refs:      []string{},
		}
		seenRefs := map[string]struct{}{}
		for _, v := range payload.Rules.R1.Violations {
			viol.Sections = append(viol.Sections, v.Section)
			for _, ref := range v.Refs {
				if _, dup := seenRefs[ref]; dup {
					continue
				}
				seenRefs[ref] = struct{}{}
				viol.Refs = append(viol.Refs, ref)
			}
		}
		sort.Strings(viol.Refs)
		panel.Violations = append(panel.Violations, viol)
	}

	panel.Summary = contextAuditSummary{
		Fetches:     len(rows),
		UniqueVerbs: len(verbs),
		TotalBytes:  totalBytes,
		R1FailCount: r1FailCount,
	}

	top := make([]contextAuditTopSection, 0, len(sections))
	for name, agg := range sections {
		scopes := make([]string, 0, len(agg.scopes))
		for s := range agg.scopes {
			scopes = append(scopes, s)
		}
		sort.Strings(scopes)
		top = append(top, contextAuditTopSection{
			Name:       name,
			TotalBytes: agg.bytes,
			Fetches:    agg.fetches,
			Scopes:     scopes,
		})
	}
	sort.Slice(top, func(i, j int) bool {
		return top[i].TotalBytes > top[j].TotalBytes
	})
	if len(top) > contextAuditTopSectionLimit {
		top = top[:contextAuditTopSectionLimit]
	}
	panel.TopSections = top
	return panel
}

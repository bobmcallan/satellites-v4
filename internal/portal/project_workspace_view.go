// Project workspace composite for story_6a0aca5a. The composite renders
// the panels on project_detail.html as previews; the dedicated
// per-section pages own full search and filtering. Document rows mirror
// the documents_view.go shape so the cards stay consistent.
//
// sty_70c0f7a3 extended this composite with repo, contracts, and recent
// ledger teasers so the consolidated project page can render every
// panel from a single handler call.
package portal

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

const (
	projectWorkspaceDefaultLimit = 25
	projectWorkspaceMaxLimit     = 200
	projectWorkspaceListLimit    = 200
	// projectWorkspacePageSize is the default page size for the stories
	// panel (sty_ca2e686b). The non-story sections continue to honour
	// projectWorkspaceDefaultLimit; the rename was deliberately not done
	// per pr_no_unrequested_compat.
	projectWorkspacePageSize = 50
)

// projectWorkspaceComposite is the view-model for the project_detail page.
// Each field maps to a panel in the panel registry.
type projectWorkspaceComposite struct {
	Stories         []storyCard
	Documents       []documentCard
	Contracts       []documentCard
	Repo            repoCard
	RepoEmpty       bool
	LedgerRecent    []ledgerRowView
	LedgerByStory   map[string][]ledgerRowView
	Changelogs      []changelogCard
	ChangelogsTotal int
	Filters         projectWorkspaceFilters
	StoryTotal      int
	DocTotal        int
	ContractsTotal  int
	LedgerTotal     int
	// Stories-panel pagination view-model (sty_ca2e686b). StoryPage is
	// the current 1-indexed page; StoryPageSize is the active window
	// size; StoryPageCount is ceil(StoryTotal/StoryPageSize). The Has*
	// booleans gate the prev/next anchors; the *Page ints supply the
	// link href without needing template arithmetic helpers.
	StoryPage      int
	StoryPageSize  int
	StoryPageCount int
	StoryHasPrev   bool
	StoryHasNext   bool
	StoryPrevPage  int
	StoryNextPage  int
}

// changelogCard is the per-row view-model for the Changelog panel.
// The first non-empty line of Content is exposed as Heading so the
// template can render a deterministic title without parsing markdown.
type changelogCard struct {
	ID            string
	Service       string
	VersionFrom   string
	VersionTo     string
	Heading       string
	Content       string
	EffectiveDate string
	UpdatedAt     string
}

// storyCard is the per-row view-model for the Stories section. The
// expand-row on the V3-style story panel reads Description,
// AcceptanceCriteria, and TaskChain. CreatedAt + Tags are exposed on
// the row's data-* attributes so the client-side `order:<field>` and
// tag-chip click handlers can reorder + filter without an extra round
// trip.
type storyCard struct {
	ID                 string
	ProjectID          string
	Title              string
	Status             string
	Priority           string
	Category           string
	Tags               []string
	CreatedAt          string
	UpdatedAt          string
	Description        string
	AcceptanceCriteria string
	TaskChain          []taskChainCard
}

// projectWorkspaceFilters carries the per-section row cap and the
// stories-panel page window. Limit governs the non-story sections
// (documents, contracts, changelog, ledger); Page + PageSize drive the
// stories paginator (sty_ca2e686b). The story window is intentionally
// independent of Limit so the existing `?limit=` semantics for the
// other panels stay untouched.
type projectWorkspaceFilters struct {
	Limit    int
	Page     int
	PageSize int
}

// parseProjectWorkspaceFilters reads the query string. `?limit=` is
// clamped to [1, projectWorkspaceMaxLimit] as before. The new
// `?stories_page=` and `?stories_page_size=` (sty_ca2e686b) drive the
// stories-panel pagination: Page defaults to 1 (coerced from <1);
// PageSize defaults to projectWorkspacePageSize and is clamped to
// [1, projectWorkspaceListLimit] so an out-of-band value never reaches
// the store.
func parseProjectWorkspaceFilters(r *http.Request) projectWorkspaceFilters {
	q := r.URL.Query()
	f := projectWorkspaceFilters{
		Limit:    projectWorkspaceDefaultLimit,
		Page:     1,
		PageSize: projectWorkspacePageSize,
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > projectWorkspaceMaxLimit {
				n = projectWorkspaceMaxLimit
			}
			f.Limit = n
		}
	}
	if raw := strings.TrimSpace(q.Get("stories_page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			f.Page = n
		}
	}
	if raw := strings.TrimSpace(q.Get("stories_page_size")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > projectWorkspaceListLimit {
				n = projectWorkspaceListLimit
			}
			f.PageSize = n
		}
	}
	return f
}

// buildProjectWorkspaceComposite assembles the composite from the story,
// document, repo, and ledger stores. Any store may be nil — the
// corresponding section is empty in that case so callers degrade
// gracefully when running without a backing store. Documents are loaded
// twice (project scope + system scope) and merged so global content
// (principles, reviewer notes) shows alongside the project's own.
func buildProjectWorkspaceComposite(ctx context.Context, stories story.Store, docs document.Store, repos repo.Store, led ledger.Store, changelogs changelog.Store, tasks task.Store, projectID string, f projectWorkspaceFilters, memberships []string, isAdmin bool) projectWorkspaceComposite {
	if f.Limit <= 0 {
		f.Limit = projectWorkspaceDefaultLimit
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = projectWorkspacePageSize
	}
	out := projectWorkspaceComposite{Filters: f}

	out.Stories, out.StoryTotal = collectStoryCards(ctx, stories, tasks, led, projectID, f, memberships)
	// sty_ca2e686b: paginator view-model. StoryTotal is the pre-window
	// count from collectStoryCards (capped at projectWorkspaceListLimit
	// = 200; a project with >200 stories will undercount until a future
	// story swaps the cardinality source for a stores.Count call —
	// option (b) in plan.md, deferred per pr_no_unrequested_compat).
	out.StoryPage = f.Page
	out.StoryPageSize = f.PageSize
	if out.StoryPageSize > 0 {
		out.StoryPageCount = (out.StoryTotal + out.StoryPageSize - 1) / out.StoryPageSize
	}
	if out.StoryPageCount < 1 {
		out.StoryPageCount = 1
	}
	out.StoryHasPrev = out.StoryPage > 1
	out.StoryHasNext = out.StoryPage < out.StoryPageCount
	if out.StoryHasPrev {
		out.StoryPrevPage = out.StoryPage - 1
	}
	if out.StoryHasNext {
		out.StoryNextPage = out.StoryPage + 1
	}
	// sty_a03449d1: each storyCard's TaskChain is populated from the
	// task store so the panel renders the live task chain. Empty
	// when no tasks exist (the chain is the conversation log; an
	// empty story has no plan yet).

	out.Documents = collectDocumentCards(ctx, docs, projectID, f, memberships)
	out.DocTotal = len(out.Documents)

	if docs != nil {
		out.Contracts = collectConfigCards(ctx, docs, projectID, document.TypeContract, memberships)
		out.ContractsTotal = len(out.Contracts)
		if len(out.Contracts) > f.Limit {
			out.Contracts = out.Contracts[:f.Limit]
		}
	}

	repoComp := buildRepoComposite(ctx, repos, projectID, memberships, isAdmin)
	out.Repo = repoComp.Repo
	out.RepoEmpty = repoComp.Empty

	out.LedgerRecent = collectRecentLedger(ctx, led, projectID, f, memberships)
	out.LedgerTotal = len(out.LedgerRecent)
	out.LedgerByStory = groupLedgerByStory(out.LedgerRecent, ledgerPerStoryCap)

	out.Changelogs = collectChangelogCards(ctx, changelogs, projectID, f, memberships)
	out.ChangelogsTotal = len(out.Changelogs)

	return out
}

// collectChangelogCards reads the most recent changelog rows for the
// project, capped at f.Limit. Returns an empty slice when the store is
// nil or errors so the page still renders. Sty_12af0bdc.
func collectChangelogCards(ctx context.Context, store changelog.Store, projectID string, f projectWorkspaceFilters, memberships []string) []changelogCard {
	if store == nil || projectID == "" {
		return []changelogCard{}
	}
	rows, err := store.List(ctx, changelog.ListOptions{ProjectID: projectID, Limit: f.Limit}, memberships)
	if err != nil {
		return []changelogCard{}
	}
	out := make([]changelogCard, 0, len(rows))
	for _, c := range rows {
		out = append(out, changelogCardFor(c))
	}
	return out
}

func changelogCardFor(c changelog.Changelog) changelogCard {
	heading := firstLine(c.Content)
	eff := ""
	if !c.EffectiveDate.IsZero() {
		eff = c.EffectiveDate.UTC().Format("2006-01-02")
	}
	return changelogCard{
		ID:            c.ID,
		Service:       c.Service,
		VersionFrom:   c.VersionFrom,
		VersionTo:     c.VersionTo,
		Heading:       heading,
		Content:       c.Content,
		EffectiveDate: eff,
		UpdatedAt:     c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// firstLine returns the first non-empty trimmed line of s, falling back
// to the trimmed whole string when there are no newlines. Used to pull
// a heading out of a markdown content body.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			return ln
		}
	}
	return strings.TrimSpace(s)
}

// ledgerPerStoryCap caps the per-story ledger preview shown in the
// story panel's expand-row. Sourced from the composite's LedgerRecent,
// so no extra queries.
const ledgerPerStoryCap = 3

// groupLedgerByStory groups recent ledger rows by their StoryID, keeping
// the original (UpdatedAt-desc) order from the source list. Each story's
// list is capped at perStory.
func groupLedgerByStory(rows []ledgerRowView, perStory int) map[string][]ledgerRowView {
	out := make(map[string][]ledgerRowView)
	for _, r := range rows {
		if r.StoryID == "" {
			continue
		}
		if len(out[r.StoryID]) >= perStory {
			continue
		}
		out[r.StoryID] = append(out[r.StoryID], r)
	}
	return out
}

// collectRecentLedger reads the most recent ledger rows for the project,
// capped at f.Limit. Returns an empty slice when the store is nil or
// errors so the page still renders.
func collectRecentLedger(ctx context.Context, led ledger.Store, projectID string, f projectWorkspaceFilters, memberships []string) []ledgerRowView {
	if led == nil || projectID == "" {
		return []ledgerRowView{}
	}
	rows, err := led.List(ctx, projectID, ledger.ListOptions{Limit: f.Limit}, memberships)
	if err != nil {
		return []ledgerRowView{}
	}
	out := make([]ledgerRowView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ledgerRowViewFor(r))
	}
	return out
}

// collectStoryCards lists project-scoped stories, sorts by UpdatedAt
// desc, and returns the page window plus the pre-window total. The
// total is measured before paging so the panel header can render the
// true substrate count (sty_ca2e686b) — not the page-size lie that
// shipped pre-fix.
//
// NOTE: total is bounded by projectWorkspaceListLimit (=200); projects
// with >200 stories undercount. Deferred per pr_no_unrequested_compat;
// see plan.md option (b) for the follow-up that adds a stores.Count.
//
// Each card's TaskChain is populated when the tasks store is non-nil
// so the SSR template can render the per-story chain inline.
func collectStoryCards(ctx context.Context, stories story.Store, tasks task.Store, led ledger.Store, projectID string, f projectWorkspaceFilters, memberships []string) ([]storyCard, int) {
	if stories == nil || projectID == "" {
		return []storyCard{}, 0
	}
	rows, err := stories.List(ctx, projectID, story.ListOptions{Limit: projectWorkspaceListLimit}, memberships)
	if err != nil {
		return []storyCard{}, 0
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	total := len(rows)
	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = projectWorkspacePageSize
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start >= total {
		rows = nil
	} else {
		end := start + pageSize
		if end > total {
			end = total
		}
		rows = rows[start:end]
	}
	verdicts := verdictExcerptsByTask(ctx, led, projectID, memberships)
	out := make([]storyCard, 0, len(rows))
	for _, s := range rows {
		card := storyCardFor(s)
		if tasks != nil {
			card.TaskChain = taskChainCardsForStory(ctx, tasks, verdicts, s.ID, memberships)
		}
		out = append(out, card)
	}
	return out, total
}

// taskChainCardsForStory pulls the story's tasks and projects them to
// taskChainCard view-models in created_at order. Iteration is computed
// per (action, kind) so retries advance the lap number. Verdict
// excerpts are looked up from the pre-built map keyed by task id.
func taskChainCardsForStory(ctx context.Context, tasks task.Store, verdicts map[string]string, storyID string, memberships []string) []taskChainCard {
	rows, err := tasks.List(ctx, task.ListOptions{StoryID: storyID, Limit: 500}, memberships)
	if err != nil || len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	out := make([]taskChainCard, 0, len(rows))
	for i, t := range rows {
		excerpt := ""
		if verdicts != nil {
			excerpt = verdicts[t.ID]
		}
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
			CreatedAt:      t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			VerdictExcerpt: excerpt,
		}
		if t.CompletedAt != nil {
			card.CompletedAt = t.CompletedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, card)
	}
	return out
}

// collectDocumentCards loads documents in project scope and system scope,
// drops contract + skill types (those live on the Configuration tab),
// dedupes by id, sorts by UpdatedAt desc, and caps at f.Limit.
func collectDocumentCards(ctx context.Context, docs document.Store, projectID string, f projectWorkspaceFilters, memberships []string) []documentCard {
	if docs == nil {
		return []documentCard{}
	}
	rows := loadProjectAndSystemDocs(ctx, docs, projectID, memberships)
	rows = excludeConfigDocs(rows)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
	}
	out := make([]documentCard, 0, len(rows))
	for _, d := range rows {
		out = append(out, documentCardFor(d))
	}
	return out
}

// loadProjectAndSystemDocs runs two List calls (project + system scope)
// and merges the results, deduping by id. A nil projectID skips the
// project scope.
func loadProjectAndSystemDocs(ctx context.Context, docs document.Store, projectID string, memberships []string) []document.Document {
	seen := make(map[string]struct{})
	out := make([]document.Document, 0)
	if projectID != "" {
		projectRows, err := docs.List(ctx, document.ListOptions{ProjectID: projectID, Limit: projectWorkspaceListLimit}, memberships)
		if err == nil {
			for _, d := range projectRows {
				if _, dup := seen[d.ID]; dup {
					continue
				}
				seen[d.ID] = struct{}{}
				out = append(out, d)
			}
		}
	}
	systemRows, err := docs.List(ctx, document.ListOptions{Scope: document.ScopeSystem, Limit: projectWorkspaceListLimit}, memberships)
	if err == nil {
		for _, d := range systemRows {
			if _, dup := seen[d.ID]; dup {
				continue
			}
			seen[d.ID] = struct{}{}
			out = append(out, d)
		}
	}
	return out
}

// excludeConfigDocs drops contract and skill types — those are lifecycle
// configuration and live on a separate Configuration tab (story_433d0661).
func excludeConfigDocs(rows []document.Document) []document.Document {
	out := make([]document.Document, 0, len(rows))
	for _, d := range rows {
		if d.Type == document.TypeContract || d.Type == document.TypeSkill {
			continue
		}
		out = append(out, d)
	}
	return out
}

// storyCardFor projects a story.Story into the card view-model.
func storyCardFor(s story.Story) storyCard {
	return storyCard{
		ID:                 s.ID,
		ProjectID:          s.ProjectID,
		Title:              s.Title,
		Status:             s.Status,
		Priority:           s.Priority,
		Category:           s.Category,
		Tags:               s.Tags,
		CreatedAt:          s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:          s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Description:        s.Description,
		AcceptanceCriteria: s.AcceptanceCriteria,
	}
}

package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// closedPaneDefaultWindow caps the Closed pane's default lookback —
// 24h per AC. "Load more" pagination through retention is sty_dc2998c5.
const closedPaneDefaultWindow = 24 * time.Hour

// projectTasksData is the SSR view-model for /projects/{id}/tasks
// (sty_953c4907).
type projectTasksData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Project         projectRow
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
	Composite       projectTasksComposite
	Filter          projectTasksFilter
}

// projectTasksComposite holds the three panes plus the active filter
// echoed back so the template can re-render the search bar.
type projectTasksComposite struct {
	Enqueued []projectTaskRow `json:"enqueued"`
	InFlight []projectTaskRow `json:"in_flight"`
	Closed   []projectTaskRow `json:"closed"`
}

// projectTaskRow is one row in any of the three panes. Layout
// mirrors the story-row (id / title / status / updated) plus a
// status-aware duration column. Outcome and iteration are surfaced
// as tag chips inside the title cell; full timestamps move to the
// click-to-expand detail row.
type projectTaskRow struct {
	ID            string   `json:"id"`
	Origin        string   `json:"origin"`
	Title         string   `json:"title"`
	Tags          []string `json:"tags,omitempty"`
	Status        string   `json:"status"`
	Priority      string   `json:"priority"`
	Kind          string   `json:"kind,omitempty"`
	Iteration     int      `json:"iteration"`
	StoryID       string   `json:"story_id,omitempty"`
	StoryTitle    string   `json:"story_title,omitempty"`
	StoryHref     string   `json:"story_href,omitempty"`
	WalkHref      string   `json:"walk_href,omitempty"`
	ClaimedByUser string   `json:"claimed_by_user,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	Duration      string   `json:"duration,omitempty"`
	CreatedAt     string   `json:"created_at"`
	ClaimedAt     string   `json:"claimed_at,omitempty"`
	ClosedAt      string   `json:"closed_at,omitempty"`
	UpdatedAt     string   `json:"updated_at"`
	PriorTaskID   string   `json:"prior_task_id,omitempty"`
}

// projectTasksFilter mirrors the filter tokens from the sty_953c4907 AC.
// Each non-empty field narrows the pane; tokens compose AND-style.
type projectTasksFilter struct {
	Raw            string
	Kind           string
	Status         string
	StoryID        string
	ContractName   string
	IterationOp    string // ">", "<", "="
	IterationVal   int
	OrderBy        string
	IncludeClosed  bool
	IncludeArchive bool
	FreeText       string
}

// handleProjectTasks renders the project-scoped task feed page
// (sty_953c4907). Cross-owner access returns 404 to avoid leaking
// project existence.
func (p *Portal) handleProjectTasks(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	if p.projects == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	active, chips, memberships := p.activeWorkspace(r, user)
	pr, err := p.projects.GetByID(r.Context(), id, memberships)
	if err != nil || pr.OwnerUserID != user.ID {
		http.NotFound(w, r)
		return
	}
	filter := parseProjectTasksFilter(r.URL.Query().Get("q"))
	composite := buildProjectTasksComposite(r.Context(), p.tasks, p.stories, p.documents, pr.ID, filter, time.Now().UTC(), memberships)
	data := projectTasksData{
		Title:           buildPageTitle(active, pr.Name, "tasks"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Project:         viewRow(pr),
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
		Composite:       composite,
		Filter:          filter,
	}
	if err := p.tmpl.ExecuteTemplate(w, "project_tasks.html", data); err != nil {
		p.logger.Error().Str("template", "project_tasks.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// parseProjectTasksFilter pulls structured tokens out of the search
// box. Tokens are space-separated key:value pairs; everything else
// flows into FreeText. Mirrors the story_list pattern from
// sty_6300fb27 so users get one mental model across the portal.
func parseProjectTasksFilter(raw string) projectTasksFilter {
	out := projectTasksFilter{Raw: raw}
	parts := strings.Fields(raw)
	freeText := []string{}
	for _, p := range parts {
		key, val, ok := strings.Cut(p, ":")
		if !ok {
			freeText = append(freeText, p)
			continue
		}
		switch strings.ToLower(key) {
		case "kind":
			out.Kind = val
		case "status":
			out.Status = val
		case "story":
			out.StoryID = val
		case "contract":
			out.ContractName = val
		case "iter":
			out.IterationOp, out.IterationVal = parseIterationToken(val)
		case "order":
			out.OrderBy = strings.ToLower(val)
		default:
			freeText = append(freeText, p)
		}
	}
	out.FreeText = strings.Join(freeText, " ")
	return out
}

func parseIterationToken(val string) (string, int) {
	if val == "" {
		return "", 0
	}
	op := "="
	cut := 0
	switch val[0] {
	case '>':
		op = ">"
		cut = 1
	case '<':
		op = "<"
		cut = 1
	}
	rest := val[cut:]
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			return "", 0
		}
		n = n*10 + int(r-'0')
	}
	return op, n
}

// buildProjectTasksComposite scans the task store under memberships,
// filters by projectID + filter tokens, and partitions into the three
// panes. The closed pane defaults to a 24h window when filter doesn't
// include status:archived. sty_953c4907.
func buildProjectTasksComposite(
	ctx context.Context,
	tasks task.Store,
	stories story.Store,
	docs document.Store,
	projectID string,
	filter projectTasksFilter,
	now time.Time,
	memberships []string,
) projectTasksComposite {
	out := projectTasksComposite{}
	if tasks == nil || projectID == "" {
		return out
	}
	rows, err := tasks.List(ctx, task.ListOptions{Limit: 500, Kind: filter.Kind}, memberships)
	if err != nil {
		return out
	}

	storyTitles := map[string]string{}
	if stories != nil {
		// Bounded pre-fetch: project stories. List returns most-recent;
		// 500 is the upper bound (story store caps), enough to cover
		// the active stories in any realistic project.
		ss, err := stories.List(ctx, projectID, story.ListOptions{Limit: 500}, memberships)
		if err == nil {
			for _, s := range ss {
				storyTitles[s.ID] = s.Title
			}
		}
	}
	contractDocCache := map[string]document.Document{}
	resolveContractDoc := func(name string) document.Document {
		if name == "" || docs == nil {
			return document.Document{}
		}
		if cached, ok := contractDocCache[name]; ok {
			return cached
		}
		ds, err := docs.List(ctx, document.ListOptions{Type: document.TypeContract, Scope: document.ScopeSystem}, nil)
		if err != nil {
			contractDocCache[name] = document.Document{}
			return document.Document{}
		}
		for _, d := range ds {
			if d.Status == document.StatusActive && d.Name == name {
				contractDocCache[name] = d
				return d
			}
		}
		contractDocCache[name] = document.Document{}
		return document.Document{}
	}

	closedWindowStart := now.Add(-closedPaneDefaultWindow)

	for _, t := range rows {
		if projectID != "" && t.ProjectID != projectID {
			continue
		}
		row := projectTaskRow{
			ID:            t.ID,
			Origin:        t.Origin,
			Status:        t.Status,
			Priority:      t.Priority,
			Kind:          t.Kind,
			Iteration:     t.Iteration,
			ClaimedByUser: t.ClaimedBy,
			Outcome:       t.Outcome,
			CreatedAt:     t.CreatedAt.UTC().Format(time.RFC3339),
			PriorTaskID:   t.PriorTaskID,
		}
		if t.ClaimedAt != nil {
			row.ClaimedAt = t.ClaimedAt.UTC().Format(time.RFC3339)
		}
		if t.CompletedAt != nil {
			row.ClosedAt = t.CompletedAt.UTC().Format(time.RFC3339)
		}
		// Title derives from contract category. Action is canonical
		// `contract:<name>`; the system contract doc may override the
		// category via Structured.
		if cn := contractNameFromAction(t.Action); cn != "" {
			row.Title = cn
			if cat := jsonStringField(resolveContractDoc(cn).Structured, "category"); cat != "" {
				row.Title = cat
			}
		}
		row.StoryID = t.StoryID
		row.StoryTitle = storyTitles[row.StoryID]
		if row.StoryID != "" {
			row.StoryHref = fmt.Sprintf("/projects/%s/stories/%s", projectID, row.StoryID)
			row.WalkHref = fmt.Sprintf("/stories/%s/walk", row.StoryID)
		}
		row.Duration = humaniseDuration(now, t)
		row.UpdatedAt = taskUpdatedAt(t).UTC().Format(time.RFC3339)
		row.Tags = buildTaskTags(row)

		if !filterMatches(row, t, filter, closedWindowStart, now) {
			continue
		}
		switch t.Status {
		case task.StatusEnqueued:
			out.Enqueued = append(out.Enqueued, row)
		case task.StatusClaimed, task.StatusInFlight:
			out.InFlight = append(out.InFlight, row)
		case task.StatusClosed:
			out.Closed = append(out.Closed, row)
		}
	}

	sortProjectTaskPane(out.Enqueued, filter.OrderBy)
	sortProjectTaskPane(out.InFlight, filter.OrderBy)
	sortProjectTaskPane(out.Closed, filter.OrderBy)
	return out
}

// filterMatches applies the filter tokens to one row + its source
// task. Closed rows older than the default window are hidden unless
// the filter explicitly opts in via status:closed.
func filterMatches(row projectTaskRow, t task.Task, filter projectTasksFilter, closedWindowStart, now time.Time) bool {
	if filter.Status != "" && filter.Status != row.Status {
		return false
	}
	if filter.StoryID != "" && filter.StoryID != row.StoryID {
		return false
	}
	if filter.ContractName != "" && filter.ContractName != row.Title {
		return false
	}
	if filter.IterationOp != "" {
		switch filter.IterationOp {
		case "=":
			if row.Iteration != filter.IterationVal {
				return false
			}
		case ">":
			if row.Iteration <= filter.IterationVal {
				return false
			}
		case "<":
			if row.Iteration >= filter.IterationVal {
				return false
			}
		}
	}
	if t.Status == task.StatusClosed && filter.Status == "" {
		// Default Closed pane window: only surface tasks closed within
		// the last 24h.
		if t.CompletedAt == nil || t.CompletedAt.Before(closedWindowStart) {
			return false
		}
	}
	if filter.FreeText != "" {
		needle := strings.ToLower(filter.FreeText)
		hay := strings.ToLower(row.ID + " " + row.StoryTitle + " " + row.Title + " " + row.ClaimedByUser)
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	return true
}

// sortProjectTaskPane orders rows within a pane per the active orderBy
// token. Default — Enqueued by age (oldest first), InFlight by claim
// age (oldest first), Closed by closed_at desc — applied when orderBy
// is empty.
func sortProjectTaskPane(rows []projectTaskRow, orderBy string) {
	switch orderBy {
	case "priority":
		sort.SliceStable(rows, func(i, j int) bool {
			return priorityRank(rows[i].Priority) < priorityRank(rows[j].Priority)
		})
	case "iteration":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Iteration > rows[j].Iteration })
	default: // "age" + fallback
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt < rows[j].CreatedAt })
	}
}

// priorityRank mirrors task.PriorityRank without re-importing — kept
// shallow so the portal's filter helpers don't grow a hard dep on the
// task package's internal ordering.
func priorityRank(p string) int {
	switch p {
	case task.PriorityCritical:
		return 0
	case task.PriorityHigh:
		return 1
	case task.PriorityMedium:
		return 2
	case task.PriorityLow:
		return 3
	}
	return 999
}

// humaniseDuration returns a short wall-clock string sized by the
// task's current lifecycle state: queue-wait for enqueued/published,
// run-time-so-far for claimed/in_flight, total run time for closed.
func humaniseDuration(now time.Time, t task.Task) string {
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
		if t.ClaimedAt == nil || t.CompletedAt == nil {
			return ""
		}
		d = t.CompletedAt.Sub(*t.ClaimedAt)
	default:
		return ""
	}
	if d < 0 {
		return ""
	}
	return humaniseShortDuration(d)
}

// humaniseShortDuration formats a Duration as a coarse "5s" / "12m" /
// "3h" / "2d" string suitable for a table cell.
func humaniseShortDuration(d time.Duration) string {
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

// buildTaskTags returns the key:value chip list rendered inside the
// title cell. Mirrors story-row tag chips — clickable to add to the
// filter.
func buildTaskTags(row projectTaskRow) []string {
	tags := make([]string, 0, 4)
	if row.Kind != "" {
		tags = append(tags, "kind:"+row.Kind)
	}
	if row.Iteration > 1 {
		tags = append(tags, fmt.Sprintf("iter:%d", row.Iteration))
	}
	if row.Outcome != "" {
		tags = append(tags, "outcome:"+row.Outcome)
	}
	if row.StoryTitle != "" {
		tags = append(tags, "story:"+row.StoryTitle)
	} else if row.StoryID != "" {
		tags = append(tags, "story:"+row.StoryID)
	}
	return tags
}

// jsonStringField reads a single string field from raw JSON. Defensive
// — non-object payloads return "".
func jsonStringField(raw []byte, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

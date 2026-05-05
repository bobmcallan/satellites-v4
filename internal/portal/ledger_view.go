// Ledger inspection composite for slice 11.3 (story_a9f8be3c). Builds
// the workspace-scoped ledger view per docs/ui-design.md §2.4 — search,
// filter sidebar, expand-row payloads, plus pagination metadata for
// the "N new rows" pill. SSR + JSON share this shape.
//
// sty_2b0ee8b3 added timeline mode: when a story_id filter is active
// the rows are reordered oldest-first and per-row kind metadata
// (KindClass, KindLabel, StatusChange) lets the template render
// event-shape chrome instead of a homogeneous list.
package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/bobmcallan/satellites/internal/ledger"
)

const ledgerDefaultLimit = 50

// ledgerComposite is the view-model for the ledger inspection page.
type ledgerComposite struct {
	Rows    []ledgerRowView `json:"rows"`
	Filters ledgerFilters   `json:"filters"`
	Total   int             `json:"total"`
}

// ledgerRowView is one rendered ledger row. Carries `Structured` as a
// raw string so the template can `<pre>`-print it (the JSON endpoint
// keeps the original bytes for client-side parsing).
//
// sty_2b0ee8b3 timeline metadata:
//   - KindClass — `plan` | `evidence` | `verdict` | `status-change` |
//     `process-notes` | `agent-compose` | `agent-archive` | `commit` |
//     `mcp-call` | `raw`. Extracted from the primary `kind:<x>` tag;
//     drives per-shape chrome on the timeline.
//   - KindLabel — human-readable label for the chip pill.
//   - VerdictOutcome / VerdictReasoning — populated for kind:verdict
//     rows from the structured payload.
//   - StatusChange — populated for kind:story.status_change rows
//     (parsed from Content's JSON or tags).
//   - TaskID — extracted from the task_id:<id> tag, when present.
//   - Phase — extracted from the phase:<name> tag, when present.
type ledgerRowView struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`
	Tags             []string `json:"tags,omitempty"`
	StoryID          string   `json:"story_id,omitempty"`
	Durability       string   `json:"durability"`
	SourceType       string   `json:"source_type"`
	Status           string   `json:"status"`
	CreatedAt        string   `json:"created_at"`
	CreatedBy        string   `json:"created_by"`
	Content          string   `json:"content"`
	Structured       string   `json:"structured,omitempty"`
	KindClass        string   `json:"kind_class,omitempty"`
	KindLabel        string   `json:"kind_label,omitempty"`
	VerdictOutcome   string   `json:"verdict_outcome,omitempty"`
	VerdictReasoning string   `json:"verdict_reasoning,omitempty"`
	StatusChangeFrom string   `json:"status_change_from,omitempty"`
	StatusChangeTo   string   `json:"status_change_to,omitempty"`
	TaskID           string   `json:"task_id,omitempty"`
	Phase            string   `json:"phase,omitempty"`
}

// ledgerFilters echoes the active filter state so the template can
// render the sidebar with the correct selections.
type ledgerFilters struct {
	Query      string   `json:"query,omitempty"`
	Type       string   `json:"type,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	StoryID    string   `json:"story_id,omitempty"`
	Durability string   `json:"durability,omitempty"`
	SourceType string   `json:"source_type,omitempty"`
	Status     string   `json:"status,omitempty"`
}

// parseLedgerFilters reads `?q=`, `?type=`, `?tag=`, `?story_id=`,
// `?durability=`, `?source_type=`, `?status=` from the request.
// `tag` may be repeated or comma-separated.
func parseLedgerFilters(r *http.Request) ledgerFilters {
	q := r.URL.Query()
	tags := make([]string, 0)
	for _, v := range q["tag"] {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}
	return ledgerFilters{
		Query:      strings.TrimSpace(q.Get("q")),
		Type:       strings.TrimSpace(q.Get("type")),
		Tags:       tags,
		StoryID:    strings.TrimSpace(q.Get("story_id")),
		Durability: strings.TrimSpace(q.Get("durability")),
		SourceType: strings.TrimSpace(q.Get("source_type")),
		Status:     strings.TrimSpace(q.Get("status")),
	}
}

// buildLedgerComposite reads a project's ledger rows under the active
// filters. Free-text query routes through ledger.Search; structured
// filters use ledger.List.
//
// When the story_id filter is active, the result is reordered
// oldest-first (timeline / narrative order). Otherwise the store's
// default newest-first feed shape stays.
func buildLedgerComposite(ctx context.Context, store ledger.Store, projectID string, f ledgerFilters, memberships []string) ledgerComposite {
	if store == nil {
		return ledgerComposite{Filters: f}
	}
	listOpts := ledger.ListOptions{
		Type:       f.Type,
		StoryID:    f.StoryID,
		Tags:       f.Tags,
		Durability: f.Durability,
		SourceType: f.SourceType,
		Status:     f.Status,
		Limit:      ledgerDefaultLimit,
	}
	var rows []ledger.LedgerEntry
	var err error
	if f.Query != "" {
		rows, err = store.Search(ctx, projectID, ledger.SearchOptions{
			ListOptions: listOpts,
			Query:       f.Query,
		}, memberships)
	} else {
		rows, err = store.List(ctx, projectID, listOpts, memberships)
	}
	if err != nil {
		return ledgerComposite{Filters: f}
	}
	out := make([]ledgerRowView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ledgerRowViewFor(r))
	}
	if f.StoryID != "" {
		// Story-scoped → render as timeline (ascending). Stable so
		// equal-timestamp rows keep store order.
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt < out[j].CreatedAt
		})
	}
	return ledgerComposite{Rows: out, Filters: f, Total: len(out)}
}

// ledgerRowViewFor projects a ledger.LedgerEntry into the row
// view-model. Timeline metadata (KindClass, VerdictOutcome,
// StatusChange*, TaskID, Phase) is derived once at projection
// time so the template stays free of parse logic. sty_2b0ee8b3.
func ledgerRowViewFor(r ledger.LedgerEntry) ledgerRowView {
	v := ledgerRowView{
		ID:         r.ID,
		Type:       r.Type,
		Tags:       r.Tags,
		Durability: r.Durability,
		SourceType: r.SourceType,
		Status:     r.Status,
		CreatedAt:  r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CreatedBy:  r.CreatedBy,
		Content:    r.Content,
	}
	if r.StoryID != nil {
		v.StoryID = *r.StoryID
	}
	if len(r.Structured) > 0 {
		v.Structured = string(r.Structured)
	}
	v.KindClass, v.KindLabel = ledgerKindFromTags(r.Tags, r.Type)
	v.TaskID = ledgerTagValue(r.Tags, "task_id:")
	v.Phase = ledgerTagValue(r.Tags, "phase:")
	switch v.KindClass {
	case "verdict":
		if len(r.Structured) > 0 {
			var payload struct {
				Verdict   string `json:"verdict"`
				Reasoning string `json:"reasoning"`
			}
			if json.Unmarshal(r.Structured, &payload) == nil {
				v.VerdictOutcome = payload.Verdict
				v.VerdictReasoning = payload.Reasoning
			}
		}
		if v.VerdictReasoning == "" {
			v.VerdictReasoning = r.Content
		}
	case "status-change":
		if len(r.Structured) > 0 {
			var payload struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			if json.Unmarshal(r.Structured, &payload) == nil {
				v.StatusChangeFrom = payload.From
				v.StatusChangeTo = payload.To
			}
		}
		if v.StatusChangeFrom == "" || v.StatusChangeTo == "" {
			// Fallback: try Content as JSON (the substrate's emitter
			// historically wrote the payload there before structured
			// migration).
			var payload struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			if json.Unmarshal([]byte(r.Content), &payload) == nil {
				if v.StatusChangeFrom == "" {
					v.StatusChangeFrom = payload.From
				}
				if v.StatusChangeTo == "" {
					v.StatusChangeTo = payload.To
				}
			}
		}
	}
	return v
}

// ledgerKindFromTags returns (cssClass, label) for the row's primary
// `kind:<x>` tag. Falls back to "raw" when no kind tag is present.
// Recognised kinds map to chrome the timeline template renders
// distinctly.
func ledgerKindFromTags(tags []string, rowType string) (string, string) {
	const prefix = "kind:"
	for _, t := range tags {
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		raw := strings.TrimPrefix(t, prefix)
		switch raw {
		case "plan":
			return "plan", "plan"
		case "evidence":
			return "evidence", "evidence"
		case "verdict":
			return "verdict", "verdict"
		case "story.status_change":
			return "status-change", "status change"
		case "operator-override":
			return "operator-override", "operator override"
		case "process-notes":
			return "process-notes", "process notes"
		case "agent-compose":
			return "agent-compose", "agent composed"
		case "agent-archive":
			return "agent-archive", "agent archived"
		case "commit":
			return "commit", "commit"
		case "mcp-call":
			return "mcp-call", "mcp call"
		case "decision":
			return "decision", "decision"
		case "artifact":
			return "artifact", "artifact"
		default:
			return "raw", raw
		}
	}
	// No kind tag — derive a label from the row type so the chip
	// still surfaces something readable.
	if rowType != "" {
		return "raw", rowType
	}
	return "raw", "raw"
}

// ledgerTagValue extracts the value of the first tag with the given
// `<prefix>` (including the trailing colon). Returns empty when no
// matching tag is present.
func ledgerTagValue(tags []string, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimPrefix(t, prefix)
		}
	}
	return ""
}

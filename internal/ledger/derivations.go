package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// KVRow is the projection-shape returned by KVProjection: the latest
// ledger row that carries a `key:<name>` tag plus the parsed value.
type KVRow struct {
	Key       string
	Value     string
	UpdatedAt time.Time
	UpdatedBy string
	EntryID   string
}

// CostSummary is the aggregate returned by CostRollup. Sums are taken
// over rows tagged kind:llm-usage; rows whose Structured payload doesn't
// parse contribute zero (caller can read SkippedRows to surface the
// signal).
type CostSummary struct {
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
	RowCount     int
	SkippedRows  int
}

// kvKeyTagPrefix is the convention for kv-row key encoding: the row
// carries a tag of the form `key:<name>` and Content holds the value.
const kvKeyTagPrefix = "key:"

// KVProjection returns the latest Type=kv row per key inside projectID.
// Multiple rows for the same key shadow older versions; the newest by
// CreatedAt wins. Workspace-scoped per memberships.
func KVProjection(ctx context.Context, store Store, projectID string, memberships []string) (map[string]KVRow, error) {
	rows, err := store.List(ctx, projectID, ListOptions{Type: TypeKV, Limit: MaxListLimit}, memberships)
	if err != nil {
		return nil, fmt.Errorf("ledger: kv projection list: %w", err)
	}
	out := make(map[string]KVRow, len(rows))
	for _, e := range rows {
		key := extractKey(e.Tags)
		if key == "" {
			continue
		}
		if existing, ok := out[key]; ok && existing.UpdatedAt.After(e.CreatedAt) {
			continue
		}
		out[key] = KVRow{
			Key:       key,
			Value:     e.Content,
			UpdatedAt: e.CreatedAt,
			UpdatedBy: e.CreatedBy,
			EntryID:   e.ID,
		}
	}
	return out, nil
}

func extractKey(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, kvKeyTagPrefix) {
			return strings.TrimPrefix(t, kvKeyTagPrefix)
		}
	}
	return ""
}

// StoryTimeline returns ledger rows scoped to storyID in CreatedAt ASC
// order — the natural shape for a portal story panel showing the audit
// trail. Workspace-scoped per memberships.
func StoryTimeline(ctx context.Context, store Store, storyID string, memberships []string) ([]LedgerEntry, error) {
	if storyID == "" {
		return nil, fmt.Errorf("ledger: timeline requires story id")
	}
	rows, err := store.List(ctx, "", ListOptions{StoryID: storyID, Limit: MaxListLimit, IncludeDerefd: true}, memberships)
	if err != nil {
		return nil, fmt.Errorf("ledger: story timeline list: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	return rows, nil
}

// CostRollup aggregates token + dollar usage across rows tagged
// `kind:llm-usage` inside projectID. Each row's Structured field is
// expected to be a JSON object with optional `cost_usd` (number),
// `input_tokens` (number), `output_tokens` (number); missing keys
// contribute zero. Rows whose Structured doesn't parse increment
// SkippedRows and otherwise contribute zero.
func CostRollup(ctx context.Context, store Store, projectID string, memberships []string) (CostSummary, error) {
	rows, err := store.List(ctx, projectID, ListOptions{Tags: []string{"kind:llm-usage"}, Limit: MaxListLimit, IncludeDerefd: true}, memberships)
	if err != nil {
		return CostSummary{}, fmt.Errorf("ledger: cost rollup list: %w", err)
	}
	summary := CostSummary{}
	for _, e := range rows {
		summary.RowCount++
		if len(e.Structured) == 0 {
			continue
		}
		var payload struct {
			CostUSD      float64 `json:"cost_usd"`
			InputTokens  int64   `json:"input_tokens"`
			OutputTokens int64   `json:"output_tokens"`
		}
		if err := json.Unmarshal(e.Structured, &payload); err != nil {
			summary.SkippedRows++
			continue
		}
		summary.CostUSD += payload.CostUSD
		summary.InputTokens += payload.InputTokens
		summary.OutputTokens += payload.OutputTokens
	}
	return summary, nil
}

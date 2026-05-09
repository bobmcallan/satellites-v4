// Translation — converts a surreallive.Event into the WireEvent(s)
// the wshandler emits to subscribed clients. Mirrors the kind/payload
// shapes the legacy *_emit.go files produced when the hub was the
// source of truth, ported to read from the post-mutation row instead
// of being called by the store.

package wshandler

import (
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bobmcallan/satellites/internal/surreallive"
)

// activityKindSet mirrors internal/ledger.DefaultStoryActivityKindTags
// (held inline to avoid an internal/ledger import from wshandler). The
// translator emits an extra story.activity.append event when a
// ledger.append's row carries a story_id AND any of these tag values.
var activityKindSet = map[string]struct{}{
	"kind:workflow-claim":  {},
	"kind:plan":            {},
	"kind:plan-approved":   {},
	"kind:plan-amend":      {},
	"kind:agent-compose":   {},
	"kind:action-claim":    {},
	"kind:close-request":   {},
	"kind:evidence":        {},
	"kind:artifact":        {},
	"kind:review-question": {},
	"kind:review-response": {},
	"kind:verdict":         {},
}

// monotonic id counter for synthesised WireEvents. Mirrors hub.Hub's
// idCounter — string-comparable, monotonic, formatted as 20-digit
// zero-padded so since_id semantics match what the legacy hub
// produced (clients that compared event ids lexicographically still
// see the same ordering invariant).
var wireIDCounter atomic.Uint64

func nextWireID() string {
	n := wireIDCounter.Add(1)
	s := strconv.FormatUint(n, 10)
	if len(s) >= 20 {
		return s
	}
	pad := make([]byte, 20-len(s))
	for i := range pad {
		pad[i] = '0'
	}
	return string(pad) + s
}

// translate returns 0+ WireEvents for one surreallive.Event. The
// source table determines the kind shape:
//
//   - tasks: kind = "task.<status>" — status pulled from row.status.
//   - stories: kind = "story.<status>".
//   - ledger: kind = "ledger.append" on CREATE; "ledger.dereference"
//     on UPDATE where row.status == "dereferenced". A second
//     "story.activity.append" WireEvent is appended when a CREATE row
//     carries a story_id and any tag in activityKindSet.
//
// DELETE events on tasks / stories / ledger don't currently produce a
// kind in the legacy hub flow, so they're dropped. Empty / missing
// workspace_id also drops the event — defence-in-depth against
// cross-workspace leakage.
func translate(ev surreallive.Event) []WireEvent {
	if ev.WorkspaceID == "" {
		return nil
	}
	row := ev.Row
	switch ev.Table {
	case "tasks":
		return translateTask(ev, row)
	case "stories":
		return translateStory(ev, row)
	case "ledger":
		return translateLedger(ev, row)
	}
	return nil
}

func translateTask(ev surreallive.Event, row map[string]any) []WireEvent {
	if ev.Action == surreallive.ActionDelete {
		return nil
	}
	status, _ := row["status"].(string)
	if status == "" {
		return nil
	}
	taskID := pickStringID(row, "id", "task_id")
	payload := map[string]any{
		"workspace_id": ev.WorkspaceID,
		"project_id":   ev.ProjectID,
		"task_id":      taskID,
		"priority":     pickString(row, "priority"),
	}
	if v, ok := row["story_id"].(string); ok && v != "" {
		payload["story_id"] = v
	}
	if v, ok := row["kind"].(string); ok && v != "" {
		payload["kind"] = v
	}
	if v, ok := row["action"].(string); ok && v != "" {
		payload["action"] = v
	}
	if v, ok := row["origin"].(string); ok && v != "" {
		payload["origin"] = v
	}
	if v, ok := row["outcome"].(string); ok && v != "" {
		payload["outcome"] = v
	}
	return []WireEvent{wireEvent("task."+status, ev.WorkspaceID, payload)}
}

func translateStory(ev surreallive.Event, row map[string]any) []WireEvent {
	if ev.Action == surreallive.ActionDelete {
		return nil
	}
	status, _ := row["status"].(string)
	if status == "" {
		return nil
	}
	storyID := pickStringID(row, "id", "story_id")
	payload := map[string]any{
		"workspace_id": ev.WorkspaceID,
		"project_id":   ev.ProjectID,
		"story_id":     storyID,
		"title":        pickString(row, "title"),
		"status":       status,
		"priority":     pickString(row, "priority"),
		"category":     pickString(row, "category"),
	}
	if tags, ok := pickStringSlice(row, "tags"); ok {
		payload["tags"] = tags
	} else {
		payload["tags"] = []string{}
	}
	if v, ok := row["updated_at"]; ok {
		payload["updated_at"] = v
	}
	return []WireEvent{wireEvent("story."+status, ev.WorkspaceID, payload)}
}

func translateLedger(ev surreallive.Event, row map[string]any) []WireEvent {
	if ev.Action == surreallive.ActionDelete {
		return nil
	}
	ledgerID := pickStringID(row, "id", "ledger_id")
	if ev.Action == surreallive.ActionUpdate {
		if status, _ := row["status"].(string); status == "dereferenced" {
			payload := map[string]any{
				"workspace_id": ev.WorkspaceID,
				"ledger_id":    ledgerID,
				"reason":       pickString(row, "reason"),
			}
			return []WireEvent{wireEvent("ledger.dereference", ev.WorkspaceID, payload)}
		}
		// Non-dereference UPDATEs on ledger don't surface in the
		// legacy hub flow — drop.
		return nil
	}
	// CREATE → ledger.append, optionally fanning out to story.activity.append.
	tags, _ := pickStringSlice(row, "tags")
	payload := map[string]any{
		"workspace_id": ev.WorkspaceID,
		"project_id":   ev.ProjectID,
		"ledger_id":    ledgerID,
		"type":         pickString(row, "type"),
		"tags":         tags,
	}
	storyID, hasStory := pickNonEmptyString(row, "story_id")
	if hasStory {
		payload["story_id"] = storyID
	}
	out := []WireEvent{wireEvent("ledger.append", ev.WorkspaceID, payload)}
	if !hasStory {
		return out
	}
	matchedKind, ok := activityKindForTags(tags)
	if !ok {
		return out
	}
	createdAt := ""
	if v, ok := row["created_at"].(string); ok {
		createdAt = v
	}
	activity := map[string]any{
		"workspace_id": ev.WorkspaceID,
		"project_id":   ev.ProjectID,
		"ledger_id":    ledgerID,
		"type":         payload["type"],
		"tags":         tags,
		"content":      pickString(row, "content"),
		"kind":         matchedKind,
		"story_id":     storyID,
		"created_at":   createdAt,
	}
	out = append(out, wireEvent("story.activity.append", ev.WorkspaceID, activity))
	return out
}

func wireEvent(kind, workspaceID string, payload map[string]any) WireEvent {
	return WireEvent{
		ID:          nextWireID(),
		Topic:       TopicPrefix + workspaceID,
		Kind:        kind,
		WorkspaceID: workspaceID,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Data:        payload,
	}
}

func pickString(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	v, _ := row[key].(string)
	return v
}

func pickNonEmptyString(row map[string]any, key string) (string, bool) {
	v := pickString(row, key)
	return v, v != ""
}

// pickStringID returns the first non-empty string under any of keys.
// Surreal returns ids as a structured RecordID for some tables and a
// plain string for others; we accept either by trying the listed
// keys in order.
func pickStringID(row map[string]any, keys ...string) string {
	if row == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := row[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func pickStringSlice(row map[string]any, key string) ([]string, bool) {
	if row == nil {
		return nil, false
	}
	raw, ok := row[key]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, false
}

func activityKindForTags(tags []string) (string, bool) {
	for _, t := range tags {
		if _, ok := activityKindSet[t]; ok {
			return t, true
		}
	}
	return "", false
}

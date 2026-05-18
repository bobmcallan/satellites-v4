// Translation — converts a surreallive.Event into the WireEvent(s)
// the wshandler emits to subscribed clients. Mirrors the kind/payload
// shapes the legacy *_emit.go files produced when the hub was the
// source of truth, ported to read from the post-mutation row instead
// of being called by the store.

package wshandler

import (
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bobmcallan/satellites/internal/surreallive"
	"github.com/bobmcallan/satellites/internal/wsbus"
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
	case "documents":
		return translateDocument(ev, row)
	case "repos":
		return translateRepo(ev, row)
	case "commits":
		return translateCommit(ev, row)
	case "projects":
		return translateProject(ev, row)
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
	out, ok := emitWireEvent("task."+status, ev.WorkspaceID, payload)
	if !ok {
		return nil
	}
	return []WireEvent{out}
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
	out, ok := emitWireEvent("story."+status, ev.WorkspaceID, payload)
	if !ok {
		return nil
	}
	return []WireEvent{out}
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
			out, ok := emitWireEvent("ledger.dereference", ev.WorkspaceID, payload)
			if !ok {
				return nil
			}
			return []WireEvent{out}
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
	appendEvt, ok := emitWireEvent("ledger.append", ev.WorkspaceID, payload)
	if !ok {
		return nil
	}
	out := []WireEvent{appendEvt}
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
	if activityEvt, ok := emitWireEvent("story.activity.append", ev.WorkspaceID, activity); ok {
		out = append(out, activityEvt)
	}
	return out
}

// translateDocument emits document.<status> on CREATE/UPDATE of the
// documents table. When the row's type is "contract", an additional
// contract.<status> WireEvent is emitted (the contract panel
// subscribes on that kind; the document panel subscribes on
// document.<status>). DELETE drops — mirrors task/story.
func translateDocument(ev surreallive.Event, row map[string]any) []WireEvent {
	if ev.Action == surreallive.ActionDelete {
		return nil
	}
	status, _ := row["status"].(string)
	if status == "" {
		return nil
	}
	documentID := pickStringID(row, "id", "document_id")
	docType := pickString(row, "type")
	payload := map[string]any{
		"workspace_id": ev.WorkspaceID,
		"project_id":   ev.ProjectID,
		"document_id":  documentID,
		"type":         docType,
		"name":         pickString(row, "name"),
		"scope":        pickString(row, "scope"),
		"status":       status,
	}
	if v, ok := row["updated_at"]; ok {
		payload["updated_at"] = v
	}
	docEvt, ok := emitWireEvent("document."+status, ev.WorkspaceID, payload)
	if !ok {
		return nil
	}
	out := []WireEvent{docEvt}
	if docType == "contract" {
		contractPayload := map[string]any{
			"workspace_id": ev.WorkspaceID,
			"project_id":   ev.ProjectID,
			"contract_id":  documentID,
			"name":         pickString(row, "name"),
			"scope":        pickString(row, "scope"),
			"status":       status,
		}
		if contractEvt, ok := emitWireEvent("contract."+status, ev.WorkspaceID, contractPayload); ok {
			out = append(out, contractEvt)
		}
	}
	return out
}

// translateRepo emits repo.<event> on CREATE/UPDATE of the repos
// table. CREATE → repo.created. UPDATE with status == "archived" →
// repo.archived; other UPDATEs → repo.updated. DELETE drops.
func translateRepo(ev surreallive.Event, row map[string]any) []WireEvent {
	if ev.Action == surreallive.ActionDelete {
		return nil
	}
	repoID := pickStringID(row, "id", "repo_id")
	status := pickString(row, "status")
	switch ev.Action {
	case surreallive.ActionCreate:
		payload := map[string]any{
			"workspace_id": ev.WorkspaceID,
			"project_id":   ev.ProjectID,
			"repo_id":      repoID,
			"git_remote":   pickString(row, "git_remote"),
			"status":       status,
		}
		out, ok := emitWireEvent("repo.created", ev.WorkspaceID, payload)
		if !ok {
			return nil
		}
		return []WireEvent{out}
	case surreallive.ActionUpdate:
		payload := map[string]any{
			"workspace_id": ev.WorkspaceID,
			"project_id":   ev.ProjectID,
			"repo_id":      repoID,
			"status":       status,
			"head_sha":     pickString(row, "head_sha"),
			"symbol_count": row["symbol_count"],
			"file_count":   row["file_count"],
		}
		if v, ok := row["last_indexed_at"]; ok {
			payload["last_indexed_at"] = v
		}
		kind := "repo.updated"
		if status == "archived" {
			kind = "repo.archived"
		}
		out, ok := emitWireEvent(kind, ev.WorkspaceID, payload)
		if !ok {
			return nil
		}
		return []WireEvent{out}
	}
	return nil
}

// translateCommit emits repo.commit_appended on CREATE of a row in
// the commits table. UPDATE and DELETE drop — commits are
// append-only; later edits surface via the parent repos row's
// last_indexed_at advance (translateRepo's repo.updated path).
func translateCommit(ev surreallive.Event, row map[string]any) []WireEvent {
	if ev.Action != surreallive.ActionCreate {
		return nil
	}
	payload := map[string]any{
		"workspace_id": ev.WorkspaceID,
		"project_id":   ev.ProjectID,
		"repo_id":      pickString(row, "repo_id"),
		"sha":          pickString(row, "sha"),
		"author":       pickString(row, "author"),
	}
	if v, ok := row["committed_at"]; ok {
		payload["committed_at"] = v
	}
	out, ok := emitWireEvent("repo.commit_appended", ev.WorkspaceID, payload)
	if !ok {
		return nil
	}
	return []WireEvent{out}
}

// translateProject emits project.updated on UPDATE and
// project.deleted on DELETE. CREATE drops — the project panel listens
// for mutation on existing rows; first-time creation is observed via
// the project_add MCP verb's response, not the WS surface. DELETE
// delivery depends on the surreallive notification carrying enough
// row payload to populate workspace_id; if the DELETE row is empty
// (no workspace_id) the event is dropped at the top-level guard in
// translate().
func translateProject(ev surreallive.Event, row map[string]any) []WireEvent {
	projectID := pickStringID(row, "id", "project_id")
	switch ev.Action {
	case surreallive.ActionUpdate:
		payload := map[string]any{
			"workspace_id":  ev.WorkspaceID,
			"project_id":    projectID,
			"name":          pickString(row, "name"),
			"owner_user_id": pickString(row, "owner_user_id"),
			"status":        pickString(row, "status"),
		}
		if v, ok := row["updated_at"]; ok {
			payload["updated_at"] = v
		}
		out, ok := emitWireEvent("project.updated", ev.WorkspaceID, payload)
		if !ok {
			return nil
		}
		return []WireEvent{out}
	case surreallive.ActionDelete:
		payload := map[string]any{
			"workspace_id": ev.WorkspaceID,
			"project_id":   projectID,
		}
		out, ok := emitWireEvent("project.deleted", ev.WorkspaceID, payload)
		if !ok {
			return nil
		}
		return []WireEvent{out}
	}
	return nil
}

// emitWireEvent guards every emit through the wsbus registry
// (sty_08fc8d20). An unregistered Kind drops with a log line — the
// configseed boot lint catches the offending literal source on next
// boot so the registry edit lands alongside the new emit. The plain
// wireEvent constructor remains so test helpers can still synthesise
// arbitrary frames without going through the registry.
func emitWireEvent(kind, workspaceID string, payload map[string]any) (WireEvent, bool) {
	if _, err := wsbus.LookupOrError(kind); err != nil {
		log.Printf("wshandler: drop emit for unregistered kind %q (%v)", kind, err)
		return WireEvent{}, false
	}
	return wireEvent(kind, workspaceID, payload), true
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

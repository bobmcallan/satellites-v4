package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/hubemit"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/task"
)

// Outcome enumerates the terminal outcomes of a reindex worker run.
// The dispatcher uses this to close the underlying task with a matching
// outcome enum.
type Outcome string

// Outcome values.
const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeSkipped Outcome = "skipped"
)

// ReindexHandlerName is the value the worker keys off in
// task.Payload.handler. repo_add and repo_scan emit tasks with this
// handler tag (see internal/mcpserver/repo_handlers.go).
const ReindexHandlerName = "reindex_repo"

// reindex ledger row tags. Each lifecycle phase is a distinct tag so
// audit consumers can filter without inspecting the structured payload.
const (
	tagReindexStart    = "kind:repo-reindex-start"
	tagReindexComplete = "kind:repo-reindex-complete"
	tagReindexFailed   = "kind:repo-reindex-failed"
	tagReindexSkipped  = "kind:repo-reindex-skipped"
)

// ReindexPayload is the JSON envelope expected on task.Payload. Public
// because the MCP layer needs the same shape when enqueuing.
type ReindexPayload struct {
	Handler   string `json:"handler"`
	RepoID    string `json:"repo_id"`
	GitRemote string `json:"git_remote"`
	Trigger   string `json:"trigger"`
}

// Deps bundles the dependencies the reindex worker needs. Each field
// must be non-nil except Publisher (ws emit hooks are advisory).
type Deps struct {
	Repos     Store
	Tasks     task.Store
	Ledger    ledger.Store
	Indexer   codeindex.Indexer
	Publisher hubemit.Publisher
}

// HandleReindex is the per-task worker for `origin=event,
// handler=reindex_repo`. The dispatcher invokes this once per claimed
// task; the function does NOT loop on the queue itself per
// pr_f81f60ca ("workers run tasks; workers do not choose their next task").
//
// Lifecycle:
//  1. Decode payload + load repo row.
//  2. Concurrency guard — skip if a sibling reindex is already in
//     flight for the same repo_id.
//  3. Write kind:repo-reindex-start.
//  4. Call codeindex.IndexRepo.
//  5. On success: UpdateIndexState + kind:repo-reindex-complete.
//     On failure: kind:repo-reindex-failed (repo row left untouched).
//
// Returns the dispatcher-friendly outcome plus the underlying error
// when one is fatal (decoding, transport). errors are wrapped with
// context so callers can match with errors.Is.
func HandleReindex(ctx context.Context, deps Deps, t task.Task) (Outcome, error) {
	if deps.Repos == nil || deps.Tasks == nil || deps.Ledger == nil || deps.Indexer == nil {
		return OutcomeFailure, errors.New("repo: HandleReindex requires non-nil Repos, Tasks, Ledger, Indexer")
	}

	payload, err := decodeReindexPayload(t.Payload)
	if err != nil {
		// We can't append a structured ledger row without knowing the repo,
		// but we still record the failure for audit.
		_, _ = deps.Ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: t.WorkspaceID,
			ProjectID:   t.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{tagReindexFailed, "task_id:" + t.ID, "phase:decode"},
			Content:     fmt.Sprintf("reindex payload decode failed: %v", err),
		}, time.Now().UTC())
		return OutcomeFailure, fmt.Errorf("repo: decode payload: %w", err)
	}

	r, err := deps.Repos.GetByID(ctx, payload.RepoID, nil)
	if err != nil {
		_, _ = deps.Ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: t.WorkspaceID,
			ProjectID:   t.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{tagReindexFailed, "task_id:" + t.ID, "repo_id:" + payload.RepoID, "phase:lookup"},
			Content:     fmt.Sprintf("reindex: repo %s not found: %v", payload.RepoID, err),
		}, time.Now().UTC())
		return OutcomeFailure, fmt.Errorf("repo: lookup %s: %w", payload.RepoID, err)
	}

	if other, found := findOtherInFlight(ctx, deps.Tasks, r, t.ID); found {
		appendReindexRow(ctx, deps.Ledger, tagReindexSkipped, r, t.ID, map[string]any{
			"reason":        "another reindex in flight",
			"other_task_id": other,
			"trigger":       payload.Trigger,
		})
		emitReindex(ctx, deps.Publisher, "skipped", r, map[string]any{
			"task_id":       t.ID,
			"other_task_id": other,
		})
		return OutcomeSkipped, nil
	}

	prevHead := r.HeadSHA
	startedAt := time.Now().UTC()
	appendReindexRow(ctx, deps.Ledger, tagReindexStart, r, t.ID, map[string]any{
		"trigger":       payload.Trigger,
		"prev_head_sha": prevHead,
	})
	emitReindex(ctx, deps.Publisher, "start", r, map[string]any{
		"task_id":       t.ID,
		"trigger":       payload.Trigger,
		"prev_head_sha": prevHead,
	})

	result, err := deps.Indexer.IndexRepo(ctx, r.GitRemote, r.DefaultBranch)
	if err != nil {
		appendReindexRow(ctx, deps.Ledger, tagReindexFailed, r, t.ID, map[string]any{
			"prev_head_sha": prevHead,
			"trigger":       payload.Trigger,
			"error":         err.Error(),
			"duration_ms":   time.Since(startedAt).Milliseconds(),
		})
		emitReindex(ctx, deps.Publisher, "failed", r, map[string]any{
			"task_id":     t.ID,
			"error":       err.Error(),
			"duration_ms": time.Since(startedAt).Milliseconds(),
		})
		return OutcomeFailure, fmt.Errorf("repo: code index: %w", err)
	}

	completedAt := time.Now().UTC()
	updated, err := deps.Repos.UpdateIndexState(ctx, r.ID, result.HeadSHA, completedAt, result.SymbolCount, result.FileCount)
	if err != nil {
		appendReindexRow(ctx, deps.Ledger, tagReindexFailed, r, t.ID, map[string]any{
			"prev_head_sha": prevHead,
			"new_head_sha":  result.HeadSHA,
			"trigger":       payload.Trigger,
			"error":         err.Error(),
			"phase":         "update_index_state",
			"duration_ms":   completedAt.Sub(startedAt).Milliseconds(),
		})
		emitReindex(ctx, deps.Publisher, "failed", r, map[string]any{
			"task_id":     t.ID,
			"error":       err.Error(),
			"phase":       "update_index_state",
			"duration_ms": completedAt.Sub(startedAt).Milliseconds(),
		})
		return OutcomeFailure, fmt.Errorf("repo: update index state %s: %w", r.ID, err)
	}

	appendReindexRow(ctx, deps.Ledger, tagReindexComplete, updated, t.ID, map[string]any{
		"prev_head_sha": prevHead,
		"new_head_sha":  updated.HeadSHA,
		"symbol_count":  updated.SymbolCount,
		"file_count":    updated.FileCount,
		"index_version": updated.IndexVersion,
		"trigger":       payload.Trigger,
		"duration_ms":   completedAt.Sub(startedAt).Milliseconds(),
	})
	emitReindex(ctx, deps.Publisher, "complete", updated, map[string]any{
		"task_id":      t.ID,
		"head_sha":     updated.HeadSHA,
		"symbol_count": updated.SymbolCount,
		"file_count":   updated.FileCount,
		"duration_ms":  completedAt.Sub(startedAt).Milliseconds(),
	})
	return OutcomeSuccess, nil
}

// decodeReindexPayload validates the envelope on task.Payload. Empty
// payloads, non-JSON, missing repo_id, or wrong handler all fail here
// before any side-effects run.
func decodeReindexPayload(raw []byte) (ReindexPayload, error) {
	if len(raw) == 0 {
		return ReindexPayload{}, errors.New("payload empty")
	}
	var p ReindexPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ReindexPayload{}, fmt.Errorf("unmarshal: %w", err)
	}
	if p.Handler != ReindexHandlerName {
		return ReindexPayload{}, fmt.Errorf("handler %q is not %q", p.Handler, ReindexHandlerName)
	}
	if p.RepoID == "" {
		return ReindexPayload{}, errors.New("repo_id missing")
	}
	return p, nil
}

// findOtherInFlight returns the id of an in-flight reindex task for r
// that is NOT t.ID itself. Workspace-scoped to r.WorkspaceID.
func findOtherInFlight(ctx context.Context, tasks task.Store, r Repo, selfTaskID string) (string, bool) {
	for _, status := range []string{task.StatusInFlight, task.StatusClaimed} {
		rows, err := tasks.List(ctx, task.ListOptions{
			Origin: task.OriginEvent,
			Status: status,
		}, []string{r.WorkspaceID})
		if err != nil {
			continue
		}
		for _, row := range rows {
			if row.ID == selfTaskID {
				continue
			}
			if !payloadTargetsRepo(row.Payload, r.ID) {
				continue
			}
			return row.ID, true
		}
	}
	return "", false
}

// payloadTargetsRepo decodes a peer task's payload and reports whether
// it targets repoID. Tolerant of malformed payloads — those simply
// don't match.
func payloadTargetsRepo(raw []byte, repoID string) bool {
	if len(raw) == 0 {
		return false
	}
	var p ReindexPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	return p.Handler == ReindexHandlerName && p.RepoID == repoID
}

// appendReindexRow writes a kind:repo-reindex-* ledger row scoped to
// the repo's workspace + project.
func appendReindexRow(ctx context.Context, led ledger.Store, kind string, r Repo, taskID string, structured map[string]any) {
	if led == nil {
		return
	}
	body, _ := json.Marshal(structured)
	_, _ = led.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		Type:        ledger.TypeDecision,
		Tags:        []string{kind, "repo_id:" + r.ID, "task_id:" + taskID},
		Content:     fmt.Sprintf("%s repo=%s task=%s", kind, r.ID, taskID),
		Structured:  body,
	}, time.Now().UTC())
}

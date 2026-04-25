package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/task"
)

// stale-check ledger row tags.
const (
	tagStaleCheckComplete = "kind:repo-stale-check-complete"
	tagStaleCheckError    = "kind:repo-stale-check-error"
)

// RemoteHeadResolver fetches the current HEAD SHA of a remote branch
// without cloning. Production wires GitLsRemoteResolver; tests inject a
// stub.
type RemoteHeadResolver interface {
	HeadSHA(ctx context.Context, gitRemote, branch string) (string, error)
}

// StaleCheckDeps bundles the resources the sweep needs. Resolver +
// Repos + Tasks + Ledger are required; Now is optional (defaults to
// time.Now().UTC()).
type StaleCheckDeps struct {
	Repos    Store
	Tasks    task.Store
	Ledger   ledger.Store
	Resolver RemoteHeadResolver
	Now      func() time.Time
}

// StaleCheckResult is the structured outcome captured on the
// `kind:repo-stale-check-complete` ledger row. Returned to the caller
// so a cron tier (or test) can log + alert.
type StaleCheckResult struct {
	Scanned         int `json:"scanned"`
	EnqueuedReindex int `json:"enqueued_reindex_count"`
	Errors          int `json:"errors"`
}

// RunStaleCheck sweeps every active repo, fetches its remote HEAD via
// the resolver, and enqueues a reindex task whenever the head differs
// from the stored HeadSHA. A single resolver failure does not abort the
// sweep — Errors is incremented and the loop continues. Writes one
// kind:repo-stale-check-complete row at workspace=""/system scope at
// the end. The cron tier is responsible for invoking this on schedule.
func RunStaleCheck(ctx context.Context, deps StaleCheckDeps) (StaleCheckResult, error) {
	if deps.Repos == nil || deps.Tasks == nil || deps.Ledger == nil || deps.Resolver == nil {
		return StaleCheckResult{}, fmt.Errorf("repo: RunStaleCheck requires non-nil Repos, Tasks, Ledger, Resolver")
	}
	now := deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	rows, err := deps.Repos.ListActive(ctx)
	if err != nil {
		return StaleCheckResult{}, fmt.Errorf("repo: stale check list: %w", err)
	}
	result := StaleCheckResult{}
	for _, r := range rows {
		result.Scanned++
		head, herr := deps.Resolver.HeadSHA(ctx, r.GitRemote, r.DefaultBranch)
		if herr != nil {
			result.Errors++
			appendStaleErrorRow(ctx, deps.Ledger, r, herr, now())
			continue
		}
		if head == r.HeadSHA {
			continue
		}
		if id := enqueueReindexFromStale(ctx, deps.Tasks, deps.Ledger, r, "stale_check", head, now()); id != "" {
			result.EnqueuedReindex++
		} else {
			result.Errors++
		}
	}

	body, _ := json.Marshal(result)
	_, _ = deps.Ledger.Append(ctx, ledger.LedgerEntry{
		Type:       ledger.TypeDecision,
		Tags:       []string{tagStaleCheckComplete},
		Content:    fmt.Sprintf("stale check complete: scanned=%d enqueued=%d errors=%d", result.Scanned, result.EnqueuedReindex, result.Errors),
		Structured: body,
	}, now())
	return result, nil
}

// EnqueueReindex writes the reindex task + the kind:task-enqueued
// audit row scoped to the repo's workspace. Returns the task id, or
// empty when the enqueue rejected. Trigger names the originator
// ("stale_check", "webhook:<delivery>", "portal").
func EnqueueReindex(ctx context.Context, tasks task.Store, led ledger.Store, r Repo, trigger, observedHead string, now time.Time) string {
	return enqueueReindexFromStale(ctx, tasks, led, r, trigger, observedHead, now)
}

// enqueueReindexFromStale writes the reindex task + the
// kind:task-enqueued audit row scoped to the repo's workspace. Returns
// the task id, or empty when the enqueue rejected.
func enqueueReindexFromStale(ctx context.Context, tasks task.Store, led ledger.Store, r Repo, trigger, observedHead string, now time.Time) string {
	payload := ReindexPayload{
		Handler:   ReindexHandlerName,
		RepoID:    r.ID,
		GitRemote: r.GitRemote,
		Trigger:   trigger,
	}
	body, _ := json.Marshal(payload)
	t, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID:      r.WorkspaceID,
		ProjectID:        r.ProjectID,
		Origin:           task.OriginEvent,
		Payload:          body,
		Priority:         task.PriorityMedium,
		ExpectedDuration: 5 * time.Minute,
	}, now)
	if err != nil {
		return ""
	}
	if led != nil {
		auditBody, _ := json.Marshal(map[string]any{
			"task_id":       t.ID,
			"trigger":       trigger,
			"observed_head": observedHead,
			"prev_head":     r.HeadSHA,
		})
		_, _ = led.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: r.WorkspaceID,
			ProjectID:   r.ProjectID,
			Type:        ledger.TypeDecision,
			Tags: []string{
				"kind:task-enqueued",
				"task_id:" + t.ID,
				"origin:" + t.Origin,
				"handler:reindex_repo",
				"repo_id:" + r.ID,
				"trigger:" + trigger,
			},
			Content:    fmt.Sprintf("stale-check enqueued reindex for repo %s (head drift)", r.ID),
			Structured: auditBody,
		}, now)
	}
	return t.ID
}

func appendStaleErrorRow(ctx context.Context, led ledger.Store, r Repo, err error, now time.Time) {
	if led == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"repo_id":    r.ID,
		"git_remote": r.GitRemote,
		"error":      err.Error(),
	})
	_, _ = led.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		Type:        ledger.TypeDecision,
		Tags:        []string{tagStaleCheckError, "repo_id:" + r.ID},
		Content:     fmt.Sprintf("stale check resolver error: %s", err.Error()),
		Structured:  body,
	}, now)
}

// GitLsRemoteResolver is the production RemoteHeadResolver. It shells
// out to `git ls-remote` to fetch the head sha of branch on remote
// without cloning.
type GitLsRemoteResolver struct{}

// HeadSHA implements RemoteHeadResolver for GitLsRemoteResolver.
func (GitLsRemoteResolver) HeadSHA(ctx context.Context, gitRemote, branch string) (string, error) {
	if branch == "" {
		branch = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", gitRemote, "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w", gitRemote, branch, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("git ls-remote %s %s: empty response", gitRemote, branch)
	}
	parts := strings.Fields(line)
	if len(parts) < 1 {
		return "", fmt.Errorf("git ls-remote %s %s: malformed response %q", gitRemote, branch, line)
	}
	return parts[0], nil
}

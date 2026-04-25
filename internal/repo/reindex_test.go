package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/task"
)

// recordingIndexer lets tests assert IndexRepo arguments and inject
// canned IndexResult / errors.
type recordingIndexer struct {
	codeindex.Stub
	indexCalls  int
	lastRemote  string
	lastBranch  string
	indexResult codeindex.IndexResult
	indexErr    error
}

func (r *recordingIndexer) IndexRepo(ctx context.Context, gitRemote, defaultBranch string) (codeindex.IndexResult, error) {
	r.indexCalls++
	r.lastRemote = gitRemote
	r.lastBranch = defaultBranch
	return r.indexResult, r.indexErr
}

// recordingPublisher captures Publish calls for ws-emit assertions.
type recordingPublisher struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	Topic     string
	Kind      string
	Workspace string
}

func (p *recordingPublisher) Publish(ctx context.Context, topic, kind, workspaceID string, payload any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, recordedEvent{Topic: topic, Kind: kind, Workspace: workspaceID})
}

func (p *recordingPublisher) hasKind(kind string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func newReindexFixture(t *testing.T) (Deps, Repo, task.Task) {
	t.Helper()
	now := time.Date(2026, 4, 25, 1, 0, 0, 0, time.UTC)
	ctx := context.Background()
	repos := NewMemoryStore()
	tasks := task.NewMemoryStore()
	led := ledger.NewMemoryStore()

	r, err := repos.Create(ctx, Repo{
		WorkspaceID:   "ws_1",
		ProjectID:     "proj_a",
		GitRemote:     "git@github.com:example/r.git",
		DefaultBranch: "main",
	}, now)
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	payload := ReindexPayload{
		Handler:   ReindexHandlerName,
		RepoID:    r.ID,
		GitRemote: r.GitRemote,
		Trigger:   "repo_add",
	}
	body, _ := json.Marshal(payload)
	tk, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		Origin:      task.OriginEvent,
		Priority:    task.PriorityMedium,
		Payload:     body,
	}, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	return Deps{
		Repos:  repos,
		Tasks:  tasks,
		Ledger: led,
	}, r, tk
}

func TestHandleReindex_HappyPath(t *testing.T) {
	t.Parallel()
	deps, r, tk := newReindexFixture(t)
	rec := &recordingIndexer{
		indexResult: codeindex.IndexResult{
			HeadSHA:     "deadbeef",
			SymbolCount: 1234,
			FileCount:   56,
		},
	}
	pub := &recordingPublisher{}
	deps.Indexer = rec
	deps.Publisher = pub

	outcome, err := HandleReindex(context.Background(), deps, tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != OutcomeSuccess {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeSuccess)
	}
	if rec.lastRemote != r.GitRemote {
		t.Fatalf("IndexRepo gitRemote = %q, want %q", rec.lastRemote, r.GitRemote)
	}
	if rec.lastBranch != r.DefaultBranch {
		t.Fatalf("IndexRepo defaultBranch = %q, want %q", rec.lastBranch, r.DefaultBranch)
	}

	got, _ := deps.Repos.GetByID(context.Background(), r.ID, nil)
	if got.HeadSHA != "deadbeef" {
		t.Errorf("repo.HeadSHA = %q, want deadbeef", got.HeadSHA)
	}
	if got.IndexVersion != 1 {
		t.Errorf("repo.IndexVersion = %d, want 1", got.IndexVersion)
	}
	if got.SymbolCount != 1234 || got.FileCount != 56 {
		t.Errorf("counts = (%d, %d), want (1234, 56)", got.SymbolCount, got.FileCount)
	}

	rows, _ := deps.Ledger.List(context.Background(), "", ledger.ListOptions{}, nil)
	if !hasReindexTag(rows, tagReindexStart) {
		t.Errorf("missing %s row; tags=%v", tagReindexStart, flattenReindexTags(rows))
	}
	if !hasReindexTag(rows, tagReindexComplete) {
		t.Errorf("missing %s row; tags=%v", tagReindexComplete, flattenReindexTags(rows))
	}
	if hasReindexTag(rows, tagReindexFailed) {
		t.Errorf("unexpected %s row on happy path", tagReindexFailed)
	}

	if !pub.hasKind("repo.reindex.start") {
		t.Errorf("missing ws event repo.reindex.start; events=%+v", pub.events)
	}
	if !pub.hasKind("repo.reindex.complete") {
		t.Errorf("missing ws event repo.reindex.complete; events=%+v", pub.events)
	}
}

func TestHandleReindex_JcodemunchUnavailable(t *testing.T) {
	t.Parallel()
	deps, r, tk := newReindexFixture(t)
	rec := &recordingIndexer{
		indexErr: &codeindex.UnavailableError{Op: "index_repo"},
	}
	pub := &recordingPublisher{}
	deps.Indexer = rec
	deps.Publisher = pub

	outcome, err := HandleReindex(context.Background(), deps, tk)
	if err == nil {
		t.Fatalf("expected error from indexer failure; got nil")
	}
	if !errors.Is(err, codeindex.ErrUnavailable) {
		t.Fatalf("error chain missing ErrUnavailable: %v", err)
	}
	if outcome != OutcomeFailure {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeFailure)
	}

	got, _ := deps.Repos.GetByID(context.Background(), r.ID, nil)
	if got.IndexVersion != 0 {
		t.Errorf("repo.IndexVersion = %d after failure, want 0 (untouched)", got.IndexVersion)
	}
	if got.HeadSHA != "" {
		t.Errorf("repo.HeadSHA = %q after failure, want empty (untouched)", got.HeadSHA)
	}

	rows, _ := deps.Ledger.List(context.Background(), "", ledger.ListOptions{}, nil)
	if !hasReindexTag(rows, tagReindexStart) {
		t.Errorf("missing %s row", tagReindexStart)
	}
	if !hasReindexTag(rows, tagReindexFailed) {
		t.Errorf("missing %s row", tagReindexFailed)
	}
	if hasReindexTag(rows, tagReindexComplete) {
		t.Errorf("unexpected %s row on failure", tagReindexComplete)
	}
	if !pub.hasKind("repo.reindex.failed") {
		t.Errorf("missing ws event repo.reindex.failed; events=%+v", pub.events)
	}
}

func TestHandleReindex_RepoNotFound(t *testing.T) {
	t.Parallel()
	deps, _, _ := newReindexFixture(t)
	rec := &recordingIndexer{}
	deps.Indexer = rec

	payload := ReindexPayload{
		Handler:   ReindexHandlerName,
		RepoID:    "repo_missing",
		GitRemote: "git@host:missing.git",
		Trigger:   "repo_add",
	}
	body, _ := json.Marshal(payload)
	bogus := task.Task{
		ID:          "task_bogus",
		WorkspaceID: "ws_1",
		ProjectID:   "proj_a",
		Origin:      task.OriginEvent,
		Priority:    task.PriorityMedium,
		Payload:     body,
	}

	outcome, err := HandleReindex(context.Background(), deps, bogus)
	if err == nil {
		t.Fatalf("expected error for missing repo; got nil")
	}
	if outcome != OutcomeFailure {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeFailure)
	}
	if rec.indexCalls != 0 {
		t.Errorf("IndexRepo called %d times for missing repo, want 0", rec.indexCalls)
	}
	rows, _ := deps.Ledger.List(context.Background(), "", ledger.ListOptions{}, nil)
	if !hasReindexTag(rows, tagReindexFailed) {
		t.Errorf("missing %s row for missing-repo failure", tagReindexFailed)
	}
}

func TestHandleReindex_ConcurrencyGuard(t *testing.T) {
	t.Parallel()
	deps, r, tk := newReindexFixture(t)
	rec := &recordingIndexer{}
	pub := &recordingPublisher{}
	deps.Indexer = rec
	deps.Publisher = pub

	// Pre-seed another reindex task targeting the same repo, and flip
	// it to in_flight via a deliberate sequence: enqueue, claim, mark
	// in_flight via Update.
	otherPayload, _ := json.Marshal(ReindexPayload{
		Handler:   ReindexHandlerName,
		RepoID:    r.ID,
		GitRemote: r.GitRemote,
		Trigger:   "repo_scan",
	})
	now := time.Now().UTC()
	other, err := deps.Tasks.Enqueue(context.Background(), task.Task{
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		Origin:      task.OriginEvent,
		Priority:    task.PriorityMedium,
		Payload:     otherPayload,
	}, now)
	if err != nil {
		t.Fatalf("seed other task: %v", err)
	}
	// Two Claim calls: first picks tk (older), second picks other.
	// Both end up in status=claimed; the worker's concurrency guard is
	// satisfied by ANY peer in claimed or in_flight that targets the
	// same repo_id, so this is sufficient.
	if _, err := deps.Tasks.Claim(context.Background(), "worker_1", []string{r.WorkspaceID}, now); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, err := deps.Tasks.Claim(context.Background(), "worker_2", []string{r.WorkspaceID}, now); err != nil {
		t.Fatalf("claim 2: %v", err)
	}

	outcome, err := HandleReindex(context.Background(), deps, tk)
	if err != nil {
		t.Fatalf("concurrency skip should not error: %v", err)
	}
	if outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want %q (other task %s in flight)", outcome, OutcomeSkipped, other.ID)
	}
	if rec.indexCalls != 0 {
		t.Errorf("IndexRepo called %d times during skip, want 0", rec.indexCalls)
	}
	rows, _ := deps.Ledger.List(context.Background(), "", ledger.ListOptions{}, nil)
	if !hasReindexTag(rows, tagReindexSkipped) {
		t.Errorf("missing %s row; tags=%v", tagReindexSkipped, flattenReindexTags(rows))
	}
	if hasReindexTag(rows, tagReindexStart) {
		t.Errorf("unexpected %s row on skip", tagReindexStart)
	}
}

func TestHandleReindex_MalformedPayload(t *testing.T) {
	t.Parallel()
	deps, _, _ := newReindexFixture(t)
	rec := &recordingIndexer{}
	deps.Indexer = rec

	bogus := task.Task{
		ID:          "task_bogus",
		WorkspaceID: "ws_1",
		ProjectID:   "proj_a",
		Origin:      task.OriginEvent,
		Payload:     []byte("not-json"),
	}
	outcome, err := HandleReindex(context.Background(), deps, bogus)
	if err == nil {
		t.Fatalf("expected error for malformed payload; got nil")
	}
	if outcome != OutcomeFailure {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeFailure)
	}
	if !strings.Contains(err.Error(), "decode payload") {
		t.Errorf("error should mention decode: %v", err)
	}
	if rec.indexCalls != 0 {
		t.Errorf("IndexRepo called %d times for malformed payload, want 0", rec.indexCalls)
	}
}

// Helpers ---------------------------------------------------------------

func hasReindexTag(rows []ledger.LedgerEntry, tag string) bool {
	for _, r := range rows {
		for _, tg := range r.Tags {
			if tg == tag {
				return true
			}
		}
	}
	return false
}

func flattenReindexTags(rows []ledger.LedgerEntry) []string {
	out := make([]string, 0)
	for _, r := range rows {
		out = append(out, r.Tags...)
	}
	return out
}

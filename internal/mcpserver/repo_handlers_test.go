package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/jcodemunch"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

type repoFixture struct {
	t          *testing.T
	server     *Server
	ledger     ledger.Store
	repos      repo.Store
	tasks      task.Store
	workspaces workspace.Store
	projects   project.Store
	wsID       string
	projectID  string
	caller     CallerIdentity
	ctx        context.Context
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cfg := &config.Config{Env: "dev"}

	wsStore := workspace.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	projStore := project.NewMemoryStore()
	repoStore := repo.NewMemoryStore()
	taskStore := task.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "user_alice", "alpha", now)
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	proj, err := projStore.Create(ctx, "user_alice", ws.ID, "p1", now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	server := New(cfg, satarbor.New("info"), now, Deps{
		LedgerStore:    ledStore,
		ProjectStore:   projStore,
		WorkspaceStore: wsStore,
		RepoStore:      repoStore,
		TaskStore:      taskStore,
	})

	return &repoFixture{
		t:          t,
		server:     server,
		ledger:     ledStore,
		repos:      repoStore,
		tasks:      taskStore,
		workspaces: wsStore,
		projects:   projStore,
		wsID:       ws.ID,
		projectID:  proj.ID,
		caller:     CallerIdentity{UserID: "user_alice", Source: "session"},
		ctx:        ctx,
	}
}

func (f *repoFixture) callerCtx() context.Context {
	return withCaller(f.ctx, f.caller)
}

func TestRepoAdd_CreatesEnqueuesAndAudits(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	res, err := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote":     "git@github.com:example/a.git",
		"default_branch": "main",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	body := decodeMap(t, res)
	if body["repo_id"] == "" {
		t.Fatalf("repo_id empty: %+v", body)
	}
	if body["task_id"] == "" {
		t.Fatalf("task_id empty: %+v", body)
	}
	if body["deduplicated"] != false {
		t.Fatalf("deduplicated = %v, want false", body["deduplicated"])
	}

	rows, err := f.ledger.List(f.ctx, "", ledger.ListOptions{}, nil)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if !hasTag(rows, "kind:repo-added") {
		t.Errorf("missing kind:repo-added ledger row; tags=%v", flattenTags(rows))
	}
	if !hasTag(rows, "kind:task-enqueued") {
		t.Errorf("missing kind:task-enqueued ledger row; tags=%v", flattenTags(rows))
	}
	tasks, _ := f.tasks.List(f.ctx, task.ListOptions{}, nil)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task enqueued, got %d", len(tasks))
	}
	if len(tasks) > 0 && tasks[0].Origin != task.OriginEvent {
		t.Errorf("task origin = %q, want %q", tasks[0].Origin, task.OriginEvent)
	}
}

func TestRepoAdd_DedupsOnSameRemote(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	first, err := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/dup.git",
	}))
	if err != nil || first.IsError {
		t.Fatalf("first add: %v / %s", err, firstText(first))
	}
	firstBody := decodeMap(t, first)
	firstRepoID := firstBody["repo_id"].(string)

	second, err := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/dup.git",
	}))
	if err != nil || second.IsError {
		t.Fatalf("second add: %v / %s", err, firstText(second))
	}
	secondBody := decodeMap(t, second)
	if secondBody["repo_id"].(string) != firstRepoID {
		t.Fatalf("dedup: second repo_id = %q, want %q", secondBody["repo_id"], firstRepoID)
	}
	if secondBody["deduplicated"] != true {
		t.Fatalf("dedup: deduplicated = %v, want true", secondBody["deduplicated"])
	}
}

func TestRepoGet_NotFoundCrossWorkspace(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	add, _ := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/scoped.git",
	}))
	if add.IsError {
		t.Fatalf("add failed: %s", firstText(add))
	}
	repoID := decodeMap(t, add)["repo_id"].(string)

	// Same caller hits the row.
	got, _ := f.server.handleRepoGet(f.callerCtx(), newCallToolReq("repo_get", map[string]any{"repo_id": repoID}))
	if got.IsError {
		t.Fatalf("in-workspace get: %s", firstText(got))
	}

	// A caller in another user's workspace must miss.
	if _, err := f.workspaces.Create(f.ctx, "user_bob", "beta", time.Now().UTC()); err != nil {
		t.Fatalf("ws bob: %v", err)
	}
	bobCtx := withCaller(f.ctx, CallerIdentity{UserID: "user_bob", Source: "session"})
	cross, _ := f.server.handleRepoGet(bobCtx, newCallToolReq("repo_get", map[string]any{"repo_id": repoID}))
	if !cross.IsError {
		t.Errorf("cross-workspace get should isError; got %s", firstText(cross))
	}
}

func TestRepoList_FiltersByStatus(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	now := time.Now().UTC()
	active, err := f.repos.Create(f.ctx, repo.Repo{
		WorkspaceID: f.wsID,
		ProjectID:   f.projectID,
		GitRemote:   "git@github.com:example/active.git",
		Status:      repo.StatusActive,
	}, now)
	if err != nil {
		t.Fatalf("seed active: %v", err)
	}
	// We can only have one repo per project in this slice's invariant —
	// archive the row and verify status filtering.
	if _, err := f.repos.Archive(f.ctx, active.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	defaultRes, _ := f.server.handleRepoList(f.callerCtx(), newCallToolReq("repo_list", map[string]any{}))
	if defaultRes.IsError {
		t.Fatalf("default list: %s", firstText(defaultRes))
	}
	defaultBody := decodeMap(t, defaultRes)
	if defaultBody["status"] != "active" {
		t.Errorf("default status filter = %v, want active", defaultBody["status"])
	}
	rows, _ := defaultBody["repos"].([]any)
	if len(rows) != 0 {
		t.Errorf("default list returned %d rows, want 0 (only archived row exists)", len(rows))
	}

	archivedRes, _ := f.server.handleRepoList(f.callerCtx(), newCallToolReq("repo_list", map[string]any{
		"status": "archived",
	}))
	archivedBody := decodeMap(t, archivedRes)
	rows, _ = archivedBody["repos"].([]any)
	if len(rows) != 1 {
		t.Errorf("archived list returned %d rows, want 1", len(rows))
	}
}

func TestRepoScan_IdempotentInFlight(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	add, _ := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/scan.git",
	}))
	if add.IsError {
		t.Fatalf("add: %s", firstText(add))
	}
	repoID := decodeMap(t, add)["repo_id"].(string)
	taskIDFromAdd := decodeMap(t, add)["task_id"].(string)

	scan, _ := f.server.handleRepoScan(f.callerCtx(), newCallToolReq("repo_scan", map[string]any{
		"repo_id": repoID,
	}))
	if scan.IsError {
		t.Fatalf("scan: %s", firstText(scan))
	}
	scanBody := decodeMap(t, scan)
	if scanBody["deduplicated"] != true {
		t.Fatalf("scan: deduplicated = %v, want true (add task is in-flight)", scanBody["deduplicated"])
	}
	if scanBody["task_id"] != taskIDFromAdd {
		t.Fatalf("scan: task_id = %q, want %q", scanBody["task_id"], taskIDFromAdd)
	}
}

func TestRepoSearch_AuditRow(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	add, _ := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/search.git",
	}))
	repoID := decodeMap(t, add)["repo_id"].(string)

	res, _ := f.server.handleRepoSearch(f.callerCtx(), newCallToolReq("repo_search", map[string]any{
		"repo_id": repoID,
		"query":   "ParseToken",
	}))
	// Stub returns ErrUnavailable; handler returns isError envelope.
	if !res.IsError {
		t.Fatalf("expected isError from stub; got %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "jcodemunch_unavailable") {
		t.Errorf("error result missing 'jcodemunch_unavailable': %s", firstText(res))
	}

	rows, _ := f.ledger.List(f.ctx, "", ledger.ListOptions{}, nil)
	found := false
	for _, r := range rows {
		hasKindRepoQuery := false
		hasActionSearch := false
		for _, tag := range r.Tags {
			if tag == "kind:repo-query" {
				hasKindRepoQuery = true
			}
			if tag == "action:search" {
				hasActionSearch = true
			}
		}
		if hasKindRepoQuery && hasActionSearch {
			if !strings.Contains(string(r.Structured), "ParseToken") {
				t.Errorf("audit row found but query missing from Structured: %s", string(r.Structured))
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit row with kind:repo-query + action:search missing; tags=%v", flattenTags(rows))
	}
}

func TestRepoGetFile_ForwardsAndUnavailable(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	add, _ := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/file.git",
	}))
	repoID := decodeMap(t, add)["repo_id"].(string)

	res, _ := f.server.handleRepoGetFile(f.callerCtx(), newCallToolReq("repo_get_file", map[string]any{
		"repo_id": repoID,
		"path":    "README.md",
	}))
	if !res.IsError {
		t.Fatalf("expected isError from stub; got %s", firstText(res))
	}
	if !strings.Contains(firstText(res), "jcodemunch_unavailable") {
		t.Errorf("error result missing 'jcodemunch_unavailable': %s", firstText(res))
	}
}

func TestRepoSearch_ProxyKeyIsGitRemote(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	add, _ := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/key.git",
	}))
	repoID := decodeMap(t, add)["repo_id"].(string)

	rec := &recordingClient{}
	f.server.jcodemunch = rec
	_, _ = f.server.handleRepoSearch(f.callerCtx(), newCallToolReq("repo_search", map[string]any{
		"repo_id": repoID,
		"query":   "x",
	}))
	if rec.lastSearchKey != "git@github.com:example/key.git" {
		t.Errorf("proxy key passed to jcodemunch = %q, want git remote (NOT repo_id)", rec.lastSearchKey)
	}
	if strings.HasPrefix(rec.lastSearchKey, "repo_") {
		t.Errorf("proxy key leaks satellites repo_id prefix: %q", rec.lastSearchKey)
	}
}

func TestRepoSearch_NotFoundCrossWorkspace(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	add, _ := f.server.handleRepoAdd(f.callerCtx(), newCallToolReq("repo_add", map[string]any{
		"git_remote": "git@github.com:example/scoped-search.git",
	}))
	repoID := decodeMap(t, add)["repo_id"].(string)

	if _, err := f.workspaces.Create(f.ctx, "user_bob", "beta", time.Now().UTC()); err != nil {
		t.Fatalf("ws bob: %v", err)
	}
	bobCtx := withCaller(f.ctx, CallerIdentity{UserID: "user_bob", Source: "session"})
	res, _ := f.server.handleRepoSearch(bobCtx, newCallToolReq("repo_search", map[string]any{
		"repo_id": repoID,
		"query":   "x",
	}))
	if !res.IsError {
		t.Errorf("cross-workspace repo_search should isError; got %s", firstText(res))
	}
}

// recordingClient intercepts the jcodemunch surface so tests can assert
// the proxy key passed in.
type recordingClient struct {
	jcodemunch.Stub
	lastSearchKey string
}

func (r *recordingClient) SearchSymbols(ctx context.Context, repoKey, query, kind, language string) (json.RawMessage, error) {
	r.lastSearchKey = repoKey
	return nil, &jcodemunch.UnavailableError{Op: "search_symbols"}
}

// Helpers ---------------------------------------------------------------

func decodeMap(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	text := firstText(res)
	out := map[string]any{}
	if text == "" {
		t.Fatalf("empty result text")
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode: %v / %s", err, text)
	}
	return out
}

func hasTag(rows []ledger.LedgerEntry, tag string) bool {
	for _, r := range rows {
		for _, tg := range r.Tags {
			if tg == tag {
				return true
			}
		}
	}
	return false
}

func flattenTags(rows []ledger.LedgerEntry) []string {
	all := make([]string, 0)
	for _, r := range rows {
		all = append(all, r.Tags...)
	}
	return all
}

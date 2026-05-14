package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/task"
)

// auditTestEnv bundles the wiring shared by audit middleware tests.
type auditTestEnv struct {
	led       *ledger.MemoryStore
	projects  *project.MemoryStore
	tasks     *task.MemoryStore
	audit     *auditLogger
	projectID string
	wsID      string
	now       time.Time
}

func newAuditTestEnv(t *testing.T) *auditTestEnv {
	t.Helper()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	led := ledger.NewMemoryStore()
	projs := project.NewMemoryStore()
	tasks := task.NewMemoryStore()
	proj, err := projs.Create(ctx, "u_alice", "wksp_a", "alpha", now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}
	build := func() *client.Client {
		return client.New(client.Deps{Ledger: led, Projects: projs, Tasks: tasks})
	}
	a := newAuditLogger(build, satarbor.New("info"), 720*time.Hour, proj.ID, func() time.Time { return now })
	return &auditTestEnv{
		led:       led,
		projects:  projs,
		tasks:     tasks,
		audit:     a,
		projectID: proj.ID,
		wsID:      proj.WorkspaceID,
		now:       now,
	}
}

func auditCallRequest(verb string, args map[string]any) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = verb
	if args == nil {
		args = map[string]any{}
	}
	req.Params.Arguments = args
	return req
}

// TestAuditMiddleware_WritesOneRowPerCall asserts the wrapper emits a
// single kind:mcp-call ledger row carrying verb, duration, and the
// caller's args. Sty_1493c077.
func TestAuditMiddleware_WritesOneRowPerCall(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := env.audit.middleware(stub)
	if _, err := wrapped(context.Background(), auditCallRequest("task_walk", map[string]any{"project_id": env.projectID})); err != nil {
		t.Fatalf("wrapped: %v", err)
	}

	rows, _ := env.led.List(context.Background(), env.projectID, ledger.ListOptions{Tags: []string{AuditTagKind}}, []string{env.wsID})
	if len(rows) != 1 {
		t.Fatalf("audit row count = %d, want 1", len(rows))
	}
	row := rows[0]
	if !strings.Contains(strings.Join(row.Tags, ","), "verb:task_walk") {
		t.Errorf("missing verb tag: %v", row.Tags)
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Structured, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["verb"] != "task_walk" {
		t.Errorf("payload verb = %v, want task_walk", payload["verb"])
	}
}

// TestAuditMiddleware_ReadsLandEphemeral asserts read-classified verbs
// land with durability=ephemeral and an expires_at, while mutations
// land durable.
func TestAuditMiddleware_ReadsLandEphemeral(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := env.audit.middleware(stub)
	ctx := context.Background()
	args := map[string]any{"project_id": env.projectID}

	// Read.
	_, _ = wrapped(ctx, auditCallRequest("story_get", args))
	// Mutation.
	_, _ = wrapped(ctx, auditCallRequest("story_add", args))

	rows, _ := env.led.List(ctx, env.projectID, ledger.ListOptions{Tags: []string{AuditTagKind}}, []string{env.wsID})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byVerb := map[string]ledger.LedgerEntry{}
	for _, r := range rows {
		for _, tag := range r.Tags {
			if strings.HasPrefix(tag, "verb:") {
				byVerb[strings.TrimPrefix(tag, "verb:")] = r
			}
		}
	}
	getRow, ok := byVerb["story_get"]
	if !ok {
		t.Fatalf("story_get row missing")
	}
	if getRow.Durability != ledger.DurabilityEphemeral {
		t.Errorf("story_get durability = %q, want ephemeral", getRow.Durability)
	}
	if getRow.ExpiresAt == nil {
		t.Errorf("story_get must carry expires_at")
	}
	addRow, ok := byVerb["story_add"]
	if !ok {
		t.Fatalf("story_add row missing")
	}
	if addRow.Durability != ledger.DurabilityDurable {
		t.Errorf("story_add durability = %q, want durable", addRow.Durability)
	}
	if addRow.ExpiresAt != nil {
		t.Errorf("durable rows must not carry expires_at")
	}
}

// TestAuditMiddleware_SanitisesSecrets asserts password / api_key /
// token args are redacted from the recorded payload.
func TestAuditMiddleware_SanitisesSecrets(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := env.audit.middleware(stub)
	args := map[string]any{
		"project_id": env.projectID,
		"password":   "swordfish",
		"api_key":    "sk-secret",
		"plain":      "ok",
	}
	_, _ = wrapped(context.Background(), auditCallRequest("task_walk", args))

	rows, _ := env.led.List(context.Background(), env.projectID, ledger.ListOptions{Tags: []string{AuditTagKind}}, []string{env.wsID})
	if len(rows) == 0 {
		t.Fatal("no audit row")
	}
	var payload struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rows[0].Structured, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Arguments["password"] != "<redacted>" {
		t.Errorf("password not redacted: %v", payload.Arguments["password"])
	}
	if payload.Arguments["api_key"] != "<redacted>" {
		t.Errorf("api_key not redacted: %v", payload.Arguments["api_key"])
	}
	if payload.Arguments["plain"] != "ok" {
		t.Errorf("plain value lost: %v", payload.Arguments["plain"])
	}
}

// TestAuditMiddleware_ResolvesStoryIDFromArg asserts the story_id arg
// is stamped on the audit row's tags so the timeline view can filter.
func TestAuditMiddleware_ResolvesStoryIDFromArg(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := env.audit.middleware(stub)
	args := map[string]any{"project_id": env.projectID, "story_id": "sty_target"}
	_, _ = wrapped(context.Background(), auditCallRequest("task_walk", args))

	rows, _ := env.led.List(context.Background(), env.projectID, ledger.ListOptions{Tags: []string{AuditTagKind}}, []string{env.wsID})
	if len(rows) == 0 {
		t.Fatal("no audit row")
	}
	if rows[0].StoryID == nil || *rows[0].StoryID != "sty_target" {
		t.Errorf("StoryID = %v, want sty_target", rows[0].StoryID)
	}
	found := false
	for _, tag := range rows[0].Tags {
		if tag == "story_id:sty_target" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("story_id:sty_target tag missing: %v", rows[0].Tags)
	}
}

// TestAuditMiddleware_ResolvesStoryIDFromTask asserts the resolver
// follows a task_id arg through the task store to its parent story.
func TestAuditMiddleware_ResolvesStoryIDFromTask(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	tk, err := env.tasks.Enqueue(context.Background(), task.Task{
		WorkspaceID: env.wsID,
		ProjectID:   env.projectID,
		StoryID:     "sty_via_task",
		Origin:      task.OriginEvent,
		Status:      task.StatusPublished,
		Kind:        task.KindWork,
		Action:      "contract:plan",
	}, env.now)
	if err != nil {
		t.Fatalf("task enqueue: %v", err)
	}
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := env.audit.middleware(stub)
	args := map[string]any{"project_id": env.projectID, "task_id": tk.ID}
	_, _ = wrapped(context.Background(), auditCallRequest("task_get", args))

	rows, _ := env.led.List(context.Background(), env.projectID, ledger.ListOptions{Tags: []string{AuditTagKind}}, []string{env.wsID})
	if len(rows) == 0 {
		t.Fatal("no audit row")
	}
	if rows[0].StoryID == nil || *rows[0].StoryID != "sty_via_task" {
		t.Errorf("StoryID = %v, want sty_via_task", rows[0].StoryID)
	}
}

// TestAuditMiddleware_NoProjectSkipped asserts a call with no project
// arg AND no default project resolves to no audit row (rather than a
// workspace-less row that no operator can see).
func TestAuditMiddleware_NoProjectSkipped(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	env.audit.defaultProjectID = ""
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := env.audit.middleware(stub)
	_, _ = wrapped(context.Background(), auditCallRequest("satellites_info", nil))

	rows, _ := env.led.List(context.Background(), env.projectID, ledger.ListOptions{}, []string{env.wsID})
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// TestAuditMiddleware_HandlerErrorStillRecorded asserts a handler that
// returns an error still produces an audit row carrying the error
// string in the structured payload.
func TestAuditMiddleware_HandlerErrorStillRecorded(t *testing.T) {
	t.Parallel()
	env := newAuditTestEnv(t)
	sentinel := errors.New("handler boom")
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return nil, sentinel
	}
	wrapped := env.audit.middleware(stub)
	_, err := wrapped(context.Background(), auditCallRequest("story_add", map[string]any{"project_id": env.projectID}))
	if !errors.Is(err, sentinel) {
		t.Errorf("handler error must propagate: %v", err)
	}
	rows, _ := env.led.List(context.Background(), env.projectID, ledger.ListOptions{Tags: []string{AuditTagKind}}, []string{env.wsID})
	if len(rows) != 1 {
		t.Fatalf("audit row count = %d, want 1", len(rows))
	}
	var payload map[string]any
	_ = json.Unmarshal(rows[0].Structured, &payload)
	if payload["error"] != "handler boom" {
		t.Errorf("error not captured: %v", payload["error"])
	}
}

// TestAuditMiddleware_AuditWriteFailureNonBlocking asserts a failing
// ledger.Append does not propagate to the caller. The handler's
// success result must reach the caller verbatim.
func TestAuditMiddleware_AuditWriteFailureNonBlocking(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	led := &auditFailingLedger{}
	build := func() *client.Client {
		return client.New(client.Deps{Ledger: led})
	}
	a := newAuditLogger(build, satarbor.New("info"), 720*time.Hour, "proj_x", func() time.Time { return now })
	stub := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("inner ok"), nil
	}
	wrapped := a.middleware(stub)
	res, err := wrapped(context.Background(), auditCallRequest("task_walk", map[string]any{"project_id": "proj_x"}))
	if err != nil {
		t.Fatalf("audit failure leaked to caller: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	if !led.called {
		t.Error("audit Append was not attempted")
	}
}

// TestIsAuditReadVerb spot-checks the read/mutation classifier.
func TestIsAuditReadVerb(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"story_get":          true,
		"task_walk":          true,
		"document_search":    true,
		"ledger_recall":      true,
		"satellites_info":    true,
		"story_template_get": true,
		"task_add":           false,
		"task_update":        false,
		"story_update":       false, // sty_4db0e025 D1 folded update_status + field_set into story_update
		"story_close":        false, // sty_b97dda00 slice 1 — mechanical close mutates story status + appends close-evidence
		"ledger_append":      false,
	}
	for verb, want := range cases {
		if got := isAuditReadVerb(verb); got != want {
			t.Errorf("isAuditReadVerb(%q) = %v, want %v", verb, got, want)
		}
	}
}

// auditFailingLedger is a ledger.Store stub whose Append always errors.
// Used to prove audit-write failures don't propagate to handler callers.
type auditFailingLedger struct {
	called bool
}

func (f *auditFailingLedger) Append(ctx context.Context, e ledger.LedgerEntry, now time.Time) (ledger.LedgerEntry, error) {
	f.called = true
	return ledger.LedgerEntry{}, errors.New("stub: ledger down")
}

func (f *auditFailingLedger) List(ctx context.Context, projectID string, opts ledger.ListOptions, memberships []string) ([]ledger.LedgerEntry, error) {
	return nil, nil
}

func (f *auditFailingLedger) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error) {
	return 0, nil
}

func (f *auditFailingLedger) GetByID(ctx context.Context, id string, memberships []string) (ledger.LedgerEntry, error) {
	return ledger.LedgerEntry{}, ledger.ErrNotFound
}

func (f *auditFailingLedger) Search(ctx context.Context, projectID string, opts ledger.SearchOptions, memberships []string) ([]ledger.LedgerEntry, error) {
	return nil, nil
}

func (f *auditFailingLedger) SearchSemantic(ctx context.Context, projectID, query string, opts ledger.SearchOptions, memberships []string) ([]ledger.LedgerEntry, error) {
	return nil, ledger.ErrSemanticUnavailable
}

func (f *auditFailingLedger) Recall(ctx context.Context, rootID string, memberships []string) ([]ledger.LedgerEntry, error) {
	return nil, nil
}

func (f *auditFailingLedger) Dereference(ctx context.Context, id, reason, actor string, now time.Time, memberships []string) (ledger.LedgerEntry, error) {
	return ledger.LedgerEntry{}, ledger.ErrNotFound
}

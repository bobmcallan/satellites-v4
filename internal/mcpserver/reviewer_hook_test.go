package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/reviewer"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// stubReviewer lets a test seed a fixed Verdict + UsageCost. Implements
// reviewer.Reviewer.
type stubReviewer struct {
	verdict reviewer.Verdict
	usage   reviewer.UsageCost
	calls   int
}

func (s *stubReviewer) Review(ctx context.Context, req reviewer.Request) (reviewer.Verdict, reviewer.UsageCost, error) {
	s.calls++
	return s.verdict, s.usage, nil
}

// newReviewerFixture wires a harness like claimFixture, but with a
// caller-provided stub reviewer and contract docs carrying the
// requested validation_mode.
type reviewerFixture struct {
	ctx       context.Context
	server    *Server
	caller    CallerIdentity
	storyID   string
	wsID      string
	projectID string
	sessionID string
	cis       []contract.ContractInstance
	now       time.Time
}

func newReviewerFixture(t *testing.T, mode string, checks []reviewer.Check, stub *stubReviewer) *reviewerFixture {
	t.Helper()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	projStore := project.NewMemoryStore()
	contractStore := contract.NewMemoryStore(docStore, storyStore)
	sessionStore := session.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "user_alice", "alpha", now)
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	proj, err := projStore.Create(ctx, "user_alice", ws.ID, "p1", now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	structured, _ := json.Marshal(map[string]any{
		"validation_mode": mode,
		"checks":          checks,
	})

	contractDocs := map[string]document.Document{}
	for _, name := range []string{"preplan", "plan", "develop", "story_close"} {
		d, err := docStore.Create(ctx, document.Document{
			Type:       document.TypeContract,
			Scope:      document.ScopeSystem,
			Name:       name,
			Status:     document.StatusActive,
			Body:       "body-" + name,
			Structured: structured,
		}, now)
		if err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
		contractDocs[name] = d
	}

	parent, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Title:       "parent",
	}, now)
	if err != nil {
		t.Fatalf("story: %v", err)
	}

	cis := make([]contract.ContractInstance, 0, 4)
	for i, name := range []string{"preplan", "plan", "develop", "story_close"} {
		ci, err := contractStore.Create(ctx, contract.ContractInstance{
			StoryID:          parent.ID,
			ContractID:       contractDocs[name].ID,
			ContractName:     name,
			Sequence:         i,
			RequiredForClose: name != "story_close",
			Status:           contract.StatusReady,
		}, now)
		if err != nil {
			t.Fatalf("ci seed %d: %v", i, err)
		}
		cis = append(cis, ci)
	}

	var rev reviewer.Reviewer = stub
	if stub == nil {
		rev = reviewer.AcceptAll{}
	}

	server := New(&config.Config{Env: "dev"}, satarbor.New("info"), now, Deps{
		DocStore:         docStore,
		ProjectStore:     projStore,
		DefaultProjectID: proj.ID,
		LedgerStore:      ledStore,
		StoryStore:       storyStore,
		WorkspaceStore:   wsStore,
		ContractStore:    contractStore,
		SessionStore:     sessionStore,
		Reviewer:         rev,
		NowFunc:          func() time.Time { return now },
	})
	sessionID := "session-reviewer-test"
	if _, err := sessionStore.Register(ctx, "user_alice", sessionID, session.SourceSessionStart, now); err != nil {
		t.Fatalf("register session: %v", err)
	}
	return &reviewerFixture{
		ctx:       ctx,
		server:    server,
		caller:    CallerIdentity{UserID: "user_alice", Source: "session"},
		storyID:   parent.ID,
		wsID:      ws.ID,
		projectID: proj.ID,
		sessionID: sessionID,
		cis:       cis,
		now:       now,
	}
}

func (f *reviewerFixture) callerCtx() context.Context {
	return withCaller(f.ctx, f.caller)
}

// claim drives a CI into claimed state via the MCP handler.
func (f *reviewerFixture) claim(t *testing.T, idx int) {
	t.Helper()
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[idx].ID,
		"session_id":           f.sessionID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("claim[%d]: err=%v text=%s", idx, err, firstText(res))
	}
}

func TestClose_LLMAccepted(t *testing.T) {
	t.Parallel()
	stub := &stubReviewer{
		verdict: reviewer.Verdict{Outcome: reviewer.VerdictAccepted, Rationale: "looks good", PrinciplesCited: []string{"pr_quality"}},
		usage:   reviewer.UsageCost{InputTokens: 100, OutputTokens: 50, CostUSD: 0.002, Model: "sonnet"},
	}
	f := newReviewerFixture(t, reviewer.ModeLLM, nil, stub)
	f.claim(t, 0)
	res, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "done",
		"evidence_markdown":    "all tests pass",
	}))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	var body struct {
		Status           string `json:"status"`
		Verdict          string `json:"verdict"`
		VerdictLedgerID  string `json:"verdict_ledger_id"`
		LLMUsageLedgerID string `json:"llm_usage_ledger_id"`
	}
	_ = json.Unmarshal([]byte(firstText(res)), &body)
	if body.Status != contract.StatusPassed {
		t.Fatalf("status: %q", body.Status)
	}
	if body.Verdict != reviewer.VerdictAccepted {
		t.Fatalf("verdict: %q", body.Verdict)
	}
	if body.VerdictLedgerID == "" {
		t.Fatalf("missing verdict_ledger_id")
	}
	if body.LLMUsageLedgerID == "" {
		t.Fatalf("missing llm_usage_ledger_id")
	}
	// Verify verdict row shape.
	v, _ := f.server.ledger.GetByID(f.ctx, body.VerdictLedgerID, nil)
	if v.Type != ledger.TypeVerdict {
		t.Fatalf("verdict row type: %q", v.Type)
	}
	// Verify llm-usage tag.
	u, _ := f.server.ledger.GetByID(f.ctx, body.LLMUsageLedgerID, nil)
	hasUsageTag := false
	for _, tag := range u.Tags {
		if tag == "kind:llm-usage" {
			hasUsageTag = true
		}
	}
	if !hasUsageTag {
		t.Fatalf("llm-usage row missing tag: %v", u.Tags)
	}
	if stub.calls != 1 {
		t.Fatalf("reviewer calls: got %d want 1", stub.calls)
	}
}

func TestClose_LLMRejected(t *testing.T) {
	t.Parallel()
	stub := &stubReviewer{
		verdict: reviewer.Verdict{Outcome: reviewer.VerdictRejected, Rationale: "evidence thin"},
	}
	f := newReviewerFixture(t, reviewer.ModeLLM, nil, stub)
	f.claim(t, 0)
	res, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "done",
		"evidence_markdown":    "thin",
	}))
	if err != nil || res.IsError {
		t.Fatalf("close: err=%v text=%s", err, firstText(res))
	}
	var body struct {
		Status  string `json:"status"`
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal([]byte(firstText(res)), &body)
	if body.Status != contract.StatusFailed {
		t.Fatalf("status: %q want failed", body.Status)
	}
	if body.Verdict != reviewer.VerdictRejected {
		t.Fatalf("verdict: %q", body.Verdict)
	}
	ci, _ := f.server.contracts.GetByID(f.ctx, f.cis[0].ID, nil)
	if ci.Status != contract.StatusFailed {
		t.Fatalf("CI.Status: %q want failed", ci.Status)
	}
}

func TestClose_LLMNeedsMore(t *testing.T) {
	t.Parallel()
	stub := &stubReviewer{
		verdict: reviewer.Verdict{
			Outcome:         reviewer.VerdictNeedsMore,
			Rationale:       "two questions",
			ReviewQuestions: []string{"what's the migration plan?", "is feature-flag wired?"},
		},
	}
	f := newReviewerFixture(t, reviewer.ModeLLM, nil, stub)
	f.claim(t, 0)
	res, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "done",
		"evidence_markdown":    "partial",
	}))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError on needs_more, got %s", firstText(res))
	}
	text := firstText(res)
	if !strings.Contains(text, `"error":"needs_more"`) {
		t.Fatalf("expected needs_more error, got %s", text)
	}

	// CI should still be claimed, not passed.
	ci, _ := f.server.contracts.GetByID(f.ctx, f.cis[0].ID, nil)
	if ci.Status != contract.StatusClaimed {
		t.Fatalf("CI.Status after needs_more: %q want claimed", ci.Status)
	}

	// Two review-question rows should exist.
	rows, _ := f.server.ledger.List(f.ctx, f.projectID, ledger.ListOptions{
		Type: ledger.TypeDecision,
		Tags: []string{"kind:review-question"},
	}, nil)
	if len(rows) != 2 {
		t.Fatalf("review-question rows: got %d want 2", len(rows))
	}

	// After respond + reviewer flips to accepted, next close passes.
	stub.verdict = reviewer.Verdict{Outcome: reviewer.VerdictAccepted, Rationale: "ok"}
	if _, err := f.server.handleStoryContractRespond(f.callerCtx(), newCallToolReq("story_contract_respond", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"response_markdown":    "here are the answers",
	})); err != nil {
		t.Fatalf("respond: %v", err)
	}
	res2, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "retry",
		"evidence_markdown":    "now complete",
	}))
	if err != nil || res2.IsError {
		t.Fatalf("re-close: err=%v text=%s", err, firstText(res2))
	}
	ci, _ = f.server.contracts.GetByID(f.ctx, f.cis[0].ID, nil)
	if ci.Status != contract.StatusPassed {
		t.Fatalf("CI.Status after re-close: %q", ci.Status)
	}
}

func TestClose_CheckBasedArtifactExistsPass(t *testing.T) {
	t.Parallel()
	checks := []reviewer.Check{
		{Name: "plan_present", Type: "artifact_exists", Config: map[string]string{"artifact": "plan.md"}},
	}
	f := newReviewerFixture(t, reviewer.ModeCheckBased, checks, nil)
	f.claim(t, 0)
	// Seed an artifact row tagged artifact:plan.md on CI[0].
	_, err := f.server.ledger.Append(f.ctx, ledger.LedgerEntry{
		WorkspaceID: f.wsID,
		ProjectID:   f.projectID,
		StoryID:     ledger.StringPtr(f.storyID),
		ContractID:  ledger.StringPtr(f.cis[0].ID),
		Type:        ledger.TypeArtifact,
		Tags:        []string{"artifact:plan.md", "phase:plan"},
		Content:     "plan body",
		CreatedBy:   "user_alice",
	}, f.now)
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	res, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "checks-based pass",
	}))
	if err != nil || res.IsError {
		t.Fatalf("close: err=%v text=%s", err, firstText(res))
	}
	var body struct {
		Verdict string `json:"verdict"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal([]byte(firstText(res)), &body)
	if body.Verdict != reviewer.VerdictAccepted {
		t.Fatalf("verdict: %q", body.Verdict)
	}
	if body.Status != contract.StatusPassed {
		t.Fatalf("status: %q", body.Status)
	}
}

func TestClose_CheckBasedArtifactExistsFail(t *testing.T) {
	t.Parallel()
	checks := []reviewer.Check{
		{Name: "plan_present", Type: "artifact_exists", Config: map[string]string{"artifact": "plan.md"}},
	}
	f := newReviewerFixture(t, reviewer.ModeCheckBased, checks, nil)
	f.claim(t, 0)

	res, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "no artifact",
	}))
	if err != nil || res.IsError {
		t.Fatalf("close: err=%v text=%s", err, firstText(res))
	}
	var body struct {
		Verdict string `json:"verdict"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal([]byte(firstText(res)), &body)
	if body.Verdict != reviewer.VerdictRejected {
		t.Fatalf("verdict: %q", body.Verdict)
	}
	if body.Status != contract.StatusFailed {
		t.Fatalf("status: %q", body.Status)
	}
}

func TestClose_AgentModeSkipsReviewer(t *testing.T) {
	t.Parallel()
	stub := &stubReviewer{
		verdict: reviewer.Verdict{Outcome: reviewer.VerdictRejected, Rationale: "would reject but mode=agent"},
	}
	f := newReviewerFixture(t, reviewer.ModeAgent, nil, stub)
	f.claim(t, 0)
	res, err := f.server.handleStoryContractClose(f.callerCtx(), newCallToolReq("story_contract_close", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"close_markdown":       "agent mode",
	}))
	if err != nil || res.IsError {
		t.Fatalf("close: err=%v text=%s", err, firstText(res))
	}
	if stub.calls != 0 {
		t.Fatalf("reviewer should not be called in agent mode, got %d calls", stub.calls)
	}
	// No kind:verdict row should exist on this CI.
	rows, _ := f.server.ledger.List(f.ctx, f.projectID, ledger.ListOptions{
		Type: ledger.TypeVerdict,
	}, nil)
	for _, r := range rows {
		if r.ContractID != nil && *r.ContractID == f.cis[0].ID {
			t.Fatalf("agent mode should not write verdict row, got %+v", r)
		}
	}
	ci, _ := f.server.contracts.GetByID(f.ctx, f.cis[0].ID, nil)
	if ci.Status != contract.StatusPassed {
		t.Fatalf("agent mode close should pass CI: %q", ci.Status)
	}
}

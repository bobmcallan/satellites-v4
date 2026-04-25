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
	"github.com/bobmcallan/satellites/internal/rolegrant"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// claimFixture wires the harness for story_contract_claim tests: a
// workspace + project + 4 active system-scope contract docs + a
// parent story with a fully-laid-out 4-CI workflow + a registered
// session for the caller with an orchestrator grant stamped so the
// grant-based claim path has observable values.
type claimFixture struct {
	t            *testing.T
	ctx          context.Context
	server       *Server
	caller       CallerIdentity
	storyID      string
	wsID         string
	projectID    string
	sessionID    string
	grantID      string
	cis          []contract.ContractInstance
	contractDocs map[string]string
	now          time.Time
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cfg := &config.Config{Env: "dev"}

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	projStore := project.NewMemoryStore()
	contractStore := contract.NewMemoryStore(docStore, storyStore)
	sessionStore := session.NewMemoryStore()
	grantStore := rolegrant.NewMemoryStore(docStore)

	ws, err := wsStore.Create(ctx, "user_alice", "alpha", now)
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	proj, err := projStore.Create(ctx, "user_alice", ws.ID, "p1", now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// Seed the orchestrator role + agent so resolveRequiredRoleGrant
	// can resolve a real grant id for the claim path.
	role, err := docStore.Create(ctx, document.Document{
		WorkspaceID: ws.ID,
		Type:        document.TypeRole,
		Name:        SeedRoleOrchestratorName,
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"allowed_mcp_verbs":["*"]}`),
	}, now)
	if err != nil {
		t.Fatalf("seed role: %v", err)
	}
	agent, err := docStore.Create(ctx, document.Document{
		WorkspaceID: ws.ID,
		Type:        document.TypeAgent,
		Name:        SeedAgentOrchestratorName,
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"permitted_roles":["` + role.ID + `"],"tool_ceiling":["*"]}`),
	}, now)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	contractDocs := make(map[string]string)
	for _, name := range []string{"preplan", "plan", "develop", "story_close"} {
		d, err := docStore.Create(ctx, document.Document{
			Type:       document.TypeContract,
			Scope:      document.ScopeSystem,
			Name:       name,
			Status:     document.StatusActive,
			Body:       "body-" + name,
			Structured: []byte(`{"required_role":"` + role.ID + `"}`),
		}, now)
		if err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
		contractDocs[name] = d.ID
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
			ContractID:       contractDocs[name],
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

	server := New(cfg, satarbor.New("info"), now, Deps{
		DocStore:         docStore,
		ProjectStore:     projStore,
		DefaultProjectID: proj.ID,
		LedgerStore:      ledStore,
		StoryStore:       storyStore,
		WorkspaceStore:   wsStore,
		ContractStore:    contractStore,
		SessionStore:     sessionStore,
		RoleGrantStore:   grantStore,
		NowFunc:          func() time.Time { return now },
	})

	sessionID := "7d4e28d5-ded3-4bd4-a3ea-b4ed899ab0dc"
	if _, err := sessionStore.Register(ctx, "user_alice", sessionID, session.SourceSessionStart, now); err != nil {
		t.Fatalf("session register: %v", err)
	}
	grant, err := grantStore.Create(ctx, rolegrant.RoleGrant{
		WorkspaceID: ws.ID,
		RoleID:      role.ID,
		AgentID:     agent.ID,
		GranteeKind: rolegrant.GranteeSession,
		GranteeID:   sessionID,
	}, now)
	if err != nil {
		t.Fatalf("grant create: %v", err)
	}
	if _, err := sessionStore.SetOrchestratorGrant(ctx, "user_alice", sessionID, grant.ID, now); err != nil {
		t.Fatalf("stamp grant: %v", err)
	}

	return &claimFixture{
		t:            t,
		ctx:          ctx,
		server:       server,
		caller:       CallerIdentity{UserID: "user_alice", Source: "session"},
		storyID:      parent.ID,
		wsID:         ws.ID,
		projectID:    proj.ID,
		sessionID:    sessionID,
		grantID:      grant.ID,
		cis:          cis,
		contractDocs: contractDocs,
		now:          now,
	}
}

func (f *claimFixture) callerCtx() context.Context {
	return withCaller(f.ctx, f.caller)
}

// mintSessionGrant registers a fresh session under the fixture's user +
// mints an orchestrator grant keyed to it, returning the new grant id.
// Used by resume/rebind tests where the CI must be re-claimed under a
// distinct grant.
func (f *claimFixture) mintSessionGrant(t *testing.T, sessionID string) string {
	t.Helper()
	if _, err := f.server.sessions.Register(f.ctx, "user_alice", sessionID, session.SourceSessionStart, f.now); err != nil {
		t.Fatalf("register %q: %v", sessionID, err)
	}
	role, err := f.server.docs.GetByName(f.ctx, "", SeedRoleOrchestratorName, nil)
	if err != nil {
		t.Fatalf("lookup role: %v", err)
	}
	agent, err := f.server.docs.GetByName(f.ctx, "", SeedAgentOrchestratorName, nil)
	if err != nil {
		t.Fatalf("lookup agent: %v", err)
	}
	grant, err := f.server.grants.Create(f.ctx, rolegrant.RoleGrant{
		WorkspaceID: f.wsID,
		RoleID:      role.ID,
		AgentID:     agent.ID,
		GranteeKind: rolegrant.GranteeSession,
		GranteeID:   sessionID,
	}, f.now)
	if err != nil {
		t.Fatalf("grant create for %q: %v", sessionID, err)
	}
	if _, err := f.server.sessions.SetOrchestratorGrant(f.ctx, "user_alice", sessionID, grant.ID, f.now); err != nil {
		t.Fatalf("stamp grant for %q: %v", sessionID, err)
	}
	return grant.ID
}

func TestClaim_HappyPath(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
		"permissions_claim":    []string{"Bash:go_test"},
		"plan_markdown":        "first plan",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	var body struct {
		Status              string `json:"status"`
		ActionClaimLedgerID string `json:"action_claim_ledger_id"`
		PlanLedgerID        string `json:"plan_ledger_id"`
		Amended             bool   `json:"amended"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Status != "claimed" {
		t.Fatalf("status: %q", body.Status)
	}
	if body.ActionClaimLedgerID == "" {
		t.Fatalf("missing action_claim_ledger_id")
	}
	if body.PlanLedgerID == "" {
		t.Fatalf("missing plan_ledger_id")
	}
	if body.Amended {
		t.Fatalf("should not be amended on first claim")
	}
}

func TestClaim_PredecessorNotTerminal(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	// Try to claim CI[2] (develop) while CI[0] + CI[1] are still ready.
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[2].ID,
		"session_id":           f.sessionID,
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError")
	}
	text := firstText(res)
	if !anySubstring(text, `"error":"predecessor_not_terminal"`, `"blocking":"`+f.cis[0].ID+`"`) {
		t.Fatalf("expected predecessor_not_terminal blocking CI[0], got %s", text)
	}
}

func TestClaim_SessionNotRegistered(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           "ghost-session",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := firstText(res)
	if !strings.Contains(text, `"error":"session_not_registered"`) {
		t.Fatalf("expected session_not_registered, got %s", text)
	}
}

func TestClaim_SessionStale(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	// Override the session's last_seen_at to a time comfortably older
	// than the staleness window relative to the fixture clock. Story
	// 3ae6621b made the handler clock injectable; tests now advance the
	// session backwards from the frozen "now" rather than relying on
	// real wall-clock drift.
	mem := f.server.sessions.(*session.MemoryStore)
	ancient := f.now.Add(-2 * session.StalenessDefault)
	if _, err := mem.Touch(f.ctx, "user_alice", f.sessionID, ancient); err != nil {
		t.Fatalf("touch: %v", err)
	}

	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := firstText(res)
	if !strings.Contains(text, `"error":"session_stale"`) {
		t.Fatalf("expected session_stale, got %s", text)
	}
}

func TestClaim_CINotReady(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	// Claim CI[0] first.
	if _, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
	})); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Transition it to passed via the store so the next claim attempt
	// sees ci_not_ready.
	if _, err := f.server.contracts.UpdateStatus(f.ctx, f.cis[0].ID, contract.StatusPassed, "user_alice", f.now, nil); err != nil {
		t.Fatalf("force passed: %v", err)
	}
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := firstText(res)
	if !anySubstring(text, `"error":"ci_not_ready"`, `"current":"passed"`) {
		t.Fatalf("expected ci_not_ready current=passed, got %s", text)
	}
}

func TestClaim_GrantMismatch(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	// Session B has its own orchestrator grant — so it passes the
	// required_role gate but the CI is already bound to session A's
	// grant, triggering grant_mismatch.
	otherSession := "other-session-uuid"
	f.mintSessionGrant(t, otherSession)

	// Session A claims.
	if _, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
	})); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Session B attempts.
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           otherSession,
	}))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	text := firstText(res)
	if !strings.Contains(text, `"error":"grant_mismatch"`) {
		t.Fatalf("expected grant_mismatch, got %s", text)
	}
}

func TestClaim_Amend(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	first, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
		"plan_markdown":        "initial plan",
	}))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	var firstBody struct {
		PlanLedgerID string `json:"plan_ledger_id"`
	}
	_ = json.Unmarshal([]byte(firstText(first)), &firstBody)
	if firstBody.PlanLedgerID == "" {
		t.Fatalf("first missing plan")
	}

	second, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
		"plan_markdown":        "amended plan",
	}))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	var secondBody struct {
		Amended      bool   `json:"amended"`
		PlanLedgerID string `json:"plan_ledger_id"`
	}
	_ = json.Unmarshal([]byte(firstText(second)), &secondBody)
	if !secondBody.Amended {
		t.Fatalf("expected amended=true on second claim, got %s", firstText(second))
	}
	if secondBody.PlanLedgerID == "" || secondBody.PlanLedgerID == firstBody.PlanLedgerID {
		t.Fatalf("expected fresh plan ledger id, got %q (prior %q)", secondBody.PlanLedgerID, firstBody.PlanLedgerID)
	}

	// Assert the first plan row is dereferenced.
	firstPlan, err := f.server.ledger.GetByID(f.ctx, firstBody.PlanLedgerID, nil)
	if err != nil {
		t.Fatalf("load first plan: %v", err)
	}
	if firstPlan.Status != ledger.StatusDereferenced {
		t.Fatalf("expected first plan status=dereferenced, got %q", firstPlan.Status)
	}
}

func TestClaim_LedgerShapes(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
		"permissions_claim":    []string{"Bash:go_test", "Write:**"},
		"skills_used":          []string{"golang-testing"},
		"plan_markdown":        "the plan body",
	}))
	if err != nil || res.IsError {
		t.Fatalf("claim: err=%v text=%s", err, firstText(res))
	}
	var body struct {
		ActionClaimLedgerID string `json:"action_claim_ledger_id"`
		PlanLedgerID        string `json:"plan_ledger_id"`
	}
	_ = json.Unmarshal([]byte(firstText(res)), &body)

	ac, err := f.server.ledger.GetByID(f.ctx, body.ActionClaimLedgerID, nil)
	if err != nil {
		t.Fatalf("load action_claim: %v", err)
	}
	if ac.Type != ledger.TypeActionClaim {
		t.Fatalf("ac type: %q", ac.Type)
	}
	hasKindTag := false
	for _, tag := range ac.Tags {
		if tag == "kind:action-claim" {
			hasKindTag = true
		}
	}
	if !hasKindTag {
		t.Fatalf("action_claim missing kind:action-claim tag: %v", ac.Tags)
	}
	var structured map[string]any
	if err := json.Unmarshal(ac.Structured, &structured); err != nil {
		t.Fatalf("structured parse: %v", err)
	}
	if structured["permissions_claim"] == nil {
		t.Fatalf("action_claim missing permissions_claim payload")
	}

	plan, err := f.server.ledger.GetByID(f.ctx, body.PlanLedgerID, nil)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	if plan.Type != ledger.TypePlan {
		t.Fatalf("plan type: %q", plan.Type)
	}
	if plan.Content != "the plan body" {
		t.Fatalf("plan content: %q", plan.Content)
	}
}

func TestClaim_CINotFound(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": "ci_ghost",
		"session_id":           f.sessionID,
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(firstText(res), `"error":"ci_not_found"`) {
		t.Fatalf("expected ci_not_found, got %s", firstText(res))
	}
}

func TestClaim_HappyChain(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	// Claim → pass → claim → pass chain of 4.
	for i, ci := range f.cis {
		res, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
			"contract_instance_id": ci.ID,
			"session_id":           f.sessionID,
		}))
		if err != nil {
			t.Fatalf("claim[%d]: %v", i, err)
		}
		if res.IsError {
			t.Fatalf("claim[%d] isError: %s", i, firstText(res))
		}
		// Force transition to passed via store for the chain to continue.
		if _, err := f.server.contracts.UpdateStatus(f.ctx, ci.ID, contract.StatusPassed, "user_alice", f.now, nil); err != nil {
			t.Fatalf("pass[%d]: %v", i, err)
		}
	}
}

func TestResume_HappyPath(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	// Claim first.
	if _, err := f.server.handleStoryContractClaim(f.callerCtx(), newCallToolReq("story_contract_claim", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           f.sessionID,
	})); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Register a fresh session with its own orchestrator grant for the
	// rebind target.
	newSess := "fresh-session"
	newGrant := f.mintSessionGrant(t, newSess)
	res, err := f.server.handleStoryContractResume(f.callerCtx(), newCallToolReq("story_contract_resume", map[string]any{
		"contract_instance_id": f.cis[0].ID,
		"session_id":           newSess,
		"reason":               "harness restart",
	}))
	if err != nil || res.IsError {
		t.Fatalf("resume: err=%v text=%s", err, firstText(res))
	}
	// CI should now be bound to the new session's grant.
	got, _ := f.server.contracts.GetByID(f.ctx, f.cis[0].ID, nil)
	if got.ClaimedViaGrantID != newGrant {
		t.Fatalf("grant not rebound: got %q want %q", got.ClaimedViaGrantID, newGrant)
	}
}

func TestSessionWhoami(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	res, err := f.server.handleSessionWhoami(f.callerCtx(), newCallToolReq("session_whoami", map[string]any{
		"session_id": f.sessionID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("whoami: err=%v text=%s", err, firstText(res))
	}
	if !strings.Contains(firstText(res), `"session_id":"`+f.sessionID+`"`) {
		t.Fatalf("whoami body mismatch: %s", firstText(res))
	}

	res, err = f.server.handleSessionWhoami(f.callerCtx(), newCallToolReq("session_whoami", map[string]any{
		"session_id": "ghost",
	}))
	if err != nil {
		t.Fatalf("ghost: %v", err)
	}
	if !strings.Contains(firstText(res), `"error":"session_not_registered"`) {
		t.Fatalf("ghost should be not_registered, got %s", firstText(res))
	}
}

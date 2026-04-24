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
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// contractFixture holds the test harness — wired memory stores + one
// workspace + one project + four active system-scope contract docs +
// one parent story.
type contractFixture struct {
	t         *testing.T
	ctx       context.Context
	server    *Server
	caller    CallerIdentity
	projectID string
	wsID      string
	storyID   string
	now       time.Time
}

func newContractFixture(t *testing.T) *contractFixture {
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

	ws, err := wsStore.Create(ctx, "user_alice", "alpha", now)
	if err != nil {
		t.Fatalf("ws create: %v", err)
	}
	if err := wsStore.AddMember(ctx, ws.ID, "user_alice", workspace.RoleAdmin, "system", now); err != nil {
		// Creator is already a member; ignore the duplicate-insert error
		// by swallowing. Membership is required for the handler's
		// workspace scope resolution.
		_ = err
	}

	proj, err := projStore.Create(ctx, "user_alice", ws.ID, "p1", now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}

	// Seed contract docs: preplan/plan/develop/story_close, scope=system.
	for _, name := range []string{"preplan", "plan", "develop", "story_close"} {
		if _, err := docStore.Create(ctx, document.Document{
			Type:   document.TypeContract,
			Scope:  document.ScopeSystem,
			Name:   name,
			Body:   "body-" + name,
			Status: document.StatusActive,
		}, now); err != nil {
			t.Fatalf("seed contract %q: %v", name, err)
		}
	}

	parent, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Title:       "parent",
	}, now)
	if err != nil {
		t.Fatalf("parent story: %v", err)
	}

	server := New(cfg, satarbor.New("info"), now, Deps{
		DocStore:         docStore,
		ProjectStore:     projStore,
		DefaultProjectID: proj.ID,
		LedgerStore:      ledStore,
		StoryStore:       storyStore,
		WorkspaceStore:   wsStore,
		ContractStore:    contractStore,
	})

	return &contractFixture{
		t:         t,
		ctx:       ctx,
		server:    server,
		caller:    CallerIdentity{UserID: "user_alice", Source: "session"},
		projectID: proj.ID,
		wsID:      ws.ID,
		storyID:   parent.ID,
		now:       now,
	}
}

func (f *contractFixture) callerCtx() context.Context {
	return withCaller(f.ctx, f.caller)
}

func TestProjectWorkflowSpec_Default(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleProjectWorkflowSpecGet(f.callerCtx(), newCallToolReq("project_workflow_spec_get", map[string]any{
		"project_id": f.projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", firstText(res))
	}
	var body struct {
		Spec contract.WorkflowSpec `json:"spec"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Spec.Slots) != 4 {
		t.Fatalf("default slot count: got %d want 4", len(body.Spec.Slots))
	}
}

func TestProjectWorkflowSpec_Roundtrip(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	slots := []contract.Slot{
		{ContractName: "preplan", Required: true, MinCount: 1, MaxCount: 1, Source: "project"},
		{ContractName: "develop", Required: true, MinCount: 1, MaxCount: 2, Source: "project"},
		{ContractName: "story_close", Required: true, MinCount: 1, MaxCount: 1, Source: "project"},
	}
	raw, _ := json.Marshal(slots)
	res, err := f.server.handleProjectWorkflowSpecSet(f.callerCtx(), newCallToolReq("project_workflow_spec_set", map[string]any{
		"project_id": f.projectID,
		"slots":      string(raw),
	}))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.IsError {
		t.Fatalf("set isError: %s", firstText(res))
	}

	getRes, err := f.server.handleProjectWorkflowSpecGet(f.callerCtx(), newCallToolReq("project_workflow_spec_get", map[string]any{
		"project_id": f.projectID,
	}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var body struct {
		Spec contract.WorkflowSpec `json:"spec"`
	}
	if err := json.Unmarshal([]byte(firstText(getRes)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Spec.Slots) != 3 {
		t.Fatalf("slot count: got %d want 3", len(body.Spec.Slots))
	}
	if body.Spec.Slots[1].MaxCount != 2 {
		t.Fatalf("develop max: got %d want 2", body.Spec.Slots[1].MaxCount)
	}
}

func TestWorkflowClaim_HappyPath(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": []string{"preplan", "plan", "develop", "story_close"},
		"claim_markdown":     "shape-approved",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	var body struct {
		ClaimLedgerID     string                      `json:"claim_ledger_id"`
		ContractInstances []contract.ContractInstance `json:"contract_instances"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.ClaimLedgerID == "" {
		t.Fatalf("expected claim_ledger_id")
	}
	if len(body.ContractInstances) != 4 {
		t.Fatalf("CI count: got %d want 4", len(body.ContractInstances))
	}
	for i, ci := range body.ContractInstances {
		if ci.Sequence != i {
			t.Fatalf("CI[%d] sequence: got %d want %d", i, ci.Sequence, i)
		}
		if ci.Status != contract.StatusReady {
			t.Fatalf("CI[%d] status: got %q want %q", i, ci.Status, contract.StatusReady)
		}
	}
}

func TestWorkflowClaim_MissingRequiredSlot(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": []string{"plan", "develop", "story_close"},
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := firstText(res)
	if !anySubstring(text, `"error":"missing_required_slot"`, `"contract_name":"preplan"`) {
		t.Fatalf("expected structured missing_required_slot, got %s", text)
	}
}

func TestWorkflowClaim_CountOutOfRange(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	props := []string{"preplan", "plan"}
	for i := 0; i < 11; i++ {
		props = append(props, "develop")
	}
	props = append(props, "story_close")
	res, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": props,
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := firstText(res)
	if !anySubstring(text, `"error":"count_out_of_range"`, `"contract_name":"develop"`, `"count":11`) {
		t.Fatalf("expected structured count_out_of_range, got %s", text)
	}
}

func TestWorkflowClaim_UnknownContract(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": []string{"preplan", "plan", "bogus", "develop", "story_close"},
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := firstText(res)
	if !anySubstring(text, `"error":"unknown_contract"`, `"contract_name":"bogus"`) {
		t.Fatalf("expected structured unknown_contract, got %s", text)
	}
}

func TestWorkflowClaim_Idempotent(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	proposed := []string{"preplan", "plan", "develop", "story_close"}
	first, _ := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": proposed,
	}))
	if first.IsError {
		t.Fatalf("first claim error: %s", firstText(first))
	}
	var firstBody struct {
		ContractInstances []contract.ContractInstance `json:"contract_instances"`
	}
	_ = json.Unmarshal([]byte(firstText(first)), &firstBody)

	second, _ := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": proposed,
	}))
	if second.IsError {
		t.Fatalf("second claim error: %s", firstText(second))
	}
	var secondBody struct {
		ContractInstances []contract.ContractInstance `json:"contract_instances"`
		Idempotent        bool                        `json:"idempotent"`
	}
	_ = json.Unmarshal([]byte(firstText(second)), &secondBody)
	if !secondBody.Idempotent {
		t.Fatalf("second claim not flagged idempotent: %s", firstText(second))
	}
	if len(firstBody.ContractInstances) != len(secondBody.ContractInstances) {
		t.Fatalf("CI count drift: first=%d second=%d", len(firstBody.ContractInstances), len(secondBody.ContractInstances))
	}
	for i := range firstBody.ContractInstances {
		if firstBody.ContractInstances[i].ID != secondBody.ContractInstances[i].ID {
			t.Fatalf("CI[%d] id drift: %q vs %q", i, firstBody.ContractInstances[i].ID, secondBody.ContractInstances[i].ID)
		}
	}
	// Assert only one kind:workflow-claim row exists.
	rows, err := f.server.ledger.List(f.ctx, f.projectID, ledger.ListOptions{
		Type: ledger.TypeWorkflowClaim,
	}, nil)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("workflow-claim rows: got %d want 1", len(rows))
	}
}

func TestContractNext_OrderedBySequence(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	_, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": []string{"preplan", "plan", "develop", "story_close"},
	}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	res, err := f.server.handleStoryContractNext(f.callerCtx(), newCallToolReq("story_contract_next", map[string]any{
		"story_id": f.storyID,
	}))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	var body struct {
		CI *contract.ContractInstance `json:"contract_instance"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.CI == nil {
		t.Fatalf("expected a CI")
	}
	if body.CI.Sequence != 0 {
		t.Fatalf("sequence: got %d want 0", body.CI.Sequence)
	}
	if body.CI.ContractName != "preplan" {
		t.Fatalf("name: got %q want %q", body.CI.ContractName, "preplan")
	}
}

func TestContractNext_NoReadyReturnsNil(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryContractNext(f.callerCtx(), newCallToolReq("story_contract_next", map[string]any{
		"story_id": f.storyID,
	}))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	if !strings.Contains(firstText(res), `"contract_instance":null`) {
		t.Fatalf("expected null CI, got %s", firstText(res))
	}
}

func TestContractNext_ReturnsSkills(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	claim, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           f.storyID,
		"proposed_contracts": []string{"preplan", "plan", "develop", "story_close"},
	}))
	if err != nil || claim.IsError {
		t.Fatalf("claim: err=%v text=%s", err, firstText(claim))
	}
	var claimBody struct {
		ContractInstances []contract.ContractInstance `json:"contract_instances"`
	}
	_ = json.Unmarshal([]byte(firstText(claim)), &claimBody)
	preplanContractID := claimBody.ContractInstances[0].ContractID

	// Seed a skill doc bound to the preplan contract. WorkspaceID is
	// set so the membership-scoped read path returns it.
	if _, err := f.server.docs.Create(f.ctx, document.Document{
		WorkspaceID:     f.wsID,
		Type:            document.TypeSkill,
		Scope:           document.ScopeSystem,
		Name:            "test-skill",
		Body:            "skill body",
		Status:          document.StatusActive,
		ContractBinding: document.StringPtr(preplanContractID),
	}, f.now); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	res, err := f.server.handleStoryContractNext(f.callerCtx(), newCallToolReq("story_contract_next", map[string]any{
		"story_id": f.storyID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("next: err=%v text=%s", err, firstText(res))
	}
	text := firstText(res)
	if !strings.Contains(text, `"name":"test-skill"`) {
		t.Fatalf("expected skill in result, got %s", text)
	}
}

func TestWorkflowClaim_StoryNotFound(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":           "sty_ghost",
		"proposed_contracts": []string{"preplan", "plan", "develop", "story_close"},
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for ghost story")
	}
	if !strings.Contains(firstText(res), "not found") {
		t.Fatalf("expected 'not found' message, got %s", firstText(res))
	}
}

func TestContractNext_StoryNotFound(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryContractNext(f.callerCtx(), newCallToolReq("story_contract_next", map[string]any{
		"story_id": "sty_ghost",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError")
	}
	if !strings.Contains(firstText(res), "not found") {
		t.Fatalf("expected 'not found', got %s", firstText(res))
	}
}

func TestWorkflowClaim_DefaultsFromSpec(t *testing.T) {
	t.Parallel()
	f := newContractFixture(t)
	res, err := f.server.handleStoryWorkflowClaim(f.callerCtx(), newCallToolReq("story_workflow_claim", map[string]any{
		"story_id":       f.storyID,
		"claim_markdown": "no-proposed — spec defaults",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	var body struct {
		ContractInstances []contract.ContractInstance `json:"contract_instances"`
	}
	_ = json.Unmarshal([]byte(firstText(res)), &body)
	if len(body.ContractInstances) != 4 {
		t.Fatalf("default expansion count: got %d want 4", len(body.ContractInstances))
	}
}

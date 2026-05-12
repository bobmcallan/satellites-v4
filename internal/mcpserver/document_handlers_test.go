package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// newDocumentTestServer builds a Server with MemoryStore-backed
// dependencies for handler-level unit tests.
func newDocumentTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	return New(cfg, satarbor.New("info"), time.Now(), Deps{
		Client: client.Deps{
			Documents:       docStore,
			Workspaces: wsStore,
		},
	})
}

// withCaller wraps ctx with the supplied identity so handler calls see
// the caller as if AuthMiddleware had run.
func withCaller(ctx context.Context, id auth.CallerIdentity) context.Context {
	return auth.WithCaller(ctx, id)
}

func newCallToolReq(name string, args map[string]any) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return req
}

// TestHandleDocumentUpdate_RejectsImmutable enumerates every immutable
// field and confirms the handler returns isError naming the offending
// field.
func TestHandleDocumentUpdate_RejectsImmutable(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	// Seed a row to update.
	doc, err := s.deps.Documents.Create(ctx, document.Document{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Name:  "p",
		Tags:  []string{"v4"},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []string{"workspace_id", "project_id", "type", "scope", "name"}
	for _, field := range cases {
		t.Run(field, func(t *testing.T) {
			res, err := s.handleDocumentUpdate(ctx, newCallToolReq("document_update", map[string]any{
				"id":  doc.ID,
				field: "tampered",
			}))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			text := firstText(res)
			if !strings.Contains(text, "immutable field rejected: "+field) {
				t.Errorf("rejection text = %q, want to mention %q", text, field)
			}
			if !res.IsError {
				t.Errorf("IsError = false; want true for immutable %q", field)
			}
		})
	}
}

// TestHandleDocumentList_WorkspaceIsolation builds two workspaces with
// distinct callers, each owning a tenant-scoped row; each caller's
// document_list must see only their own row. Uses scope=workspace
// (type=role) — scope=system is workspace-blind by design per
// sty_6ee30308 and would not exercise tenant isolation here.
func TestHandleDocumentList_WorkspaceIsolation(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	wsA, err := s.deps.Workspaces.Create(ctx, "user_alice", "alpha", time.Now().UTC())
	if err != nil {
		t.Fatalf("ws A: %v", err)
	}
	wsB, err := s.deps.Workspaces.Create(ctx, "user_bob", "beta", time.Now().UTC())
	if err != nil {
		t.Fatalf("ws B: %v", err)
	}
	if _, err := s.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: wsA.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "alice-only",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("alice role: %v", err)
	}
	if _, err := s.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: wsB.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "bob-only",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("bob role: %v", err)
	}

	aliceCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_alice", Source: "session"})
	bobCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_bob", Source: "session"})

	resA, _ := s.handleDocumentList(aliceCtx, newCallToolReq("document_list", map[string]any{"type": "role"}))
	resB, _ := s.handleDocumentList(bobCtx, newCallToolReq("document_list", map[string]any{"type": "role"}))
	rowsA := decodeArray(t, resA)
	rowsB := decodeArray(t, resB)

	if len(rowsA) != 1 || nameOf(rowsA[0]) != "alice-only" {
		t.Errorf("alice list = %+v, want only alice-only", rowsA)
	}
	if len(rowsB) != 1 || nameOf(rowsB[0]) != "bob-only" {
		t.Errorf("bob list = %+v, want only bob-only", rowsB)
	}
}

// TestHandleDocumentAdd_ScopeSystemRejectsProjectID confirms the
// scope-vs-project-id invariant is enforced at the handler layer (not
// only in document.Validate at the store layer).
func TestHandleDocumentAdd_ScopeSystemRejectsProjectID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	res, err := s.handleDocumentAdd(ctx, newCallToolReq("document_add", map[string]any{
		"type":       "principle",
		"scope":      "system",
		"name":       "bad",
		"project_id": "proj_x",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Errorf("scope=system + project_id should isError; got %s", firstText(res))
	}
}

// TestHandleDocumentAdd_ScopeSystemDropsWorkspaceID covers
// sty_e2512dbd: when the caller creates a scope=system row, the
// handler MUST NOT stamp the caller's workspace on it. The system
// tier is non-tenant; a stamped workspace would violate Validate()
// and pull downstream readers into the wrong tenancy.
func TestHandleDocumentAdd_ScopeSystemDropsWorkspaceID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "user_creator", Source: "session"})

	res, err := s.handleDocumentAdd(ctx, newCallToolReq("document_add", map[string]any{
		"type":  "principle",
		"scope": "system",
		"name":  "sample-principle",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("create scope=system rejected: %s", firstText(res))
	}
	got := decodeOne(t, res)
	if ws, _ := got["workspace_id"].(string); ws != "" {
		t.Errorf("scope=system row stamped with workspace_id=%q, want empty", ws)
	}
}

// TestTaskAddSystemAgent_PlacesTaskInCallerProject covers sty_e2512dbd:
// task_add against a scope=system agent must ignore the agent's
// stamped tenancy (which should be empty post-migration; ignored
// defensively) and use the caller's project. Mirrors the live single-
// task-flow gap that surfaced sty_e2512dbd.
func TestTaskAddSystemAgent_PlacesTaskInCallerProject(t *testing.T) {
	t.Parallel()
	// Use the orchestrator fixture which already seeds developer_agent
	// at scope=system. The caller's project is f.projectID.
	f := newOrchestratorFixture(t)
	devID := agentDocID(t, f.server, "developer_agent")

	doc, err := f.server.deps.Documents.GetByID(context.Background(), devID, nil)
	if err != nil {
		t.Fatalf("agent get: %v", err)
	}
	if doc.WorkspaceID != "" {
		t.Fatalf("seed fixture created system agent with workspace_id=%q (sty_e2512dbd violation)", doc.WorkspaceID)
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"agent_id": devID,
		"prompt":   "place me in the caller's project",
	}
	res, err := f.server.handleTaskAdd(f.callerCtx(), req)
	if err != nil {
		t.Fatalf("task_add: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_add rejected: %s", firstText(res))
	}
	out := decodeOne(t, res)
	taskID, _ := out["task_id"].(string)
	if taskID == "" {
		t.Fatalf("task_add returned empty task_id: %+v", out)
	}
	row, err := f.taskStore.GetByID(context.Background(), taskID, nil)
	if err != nil {
		t.Fatalf("task GetByID: %v", err)
	}
	if row.ProjectID != f.projectID {
		t.Errorf("task placed in project %q, want caller's project %q", row.ProjectID, f.projectID)
	}
	if row.WorkspaceID != f.wsID {
		t.Errorf("task placed in workspace %q, want caller's workspace %q", row.WorkspaceID, f.wsID)
	}
}

// TestHandleDocumentList_SystemScopeWorkspaceBlind covers sty_6ee30308:
// scope=system reads must be visible to every authenticated caller,
// even when the row was created in a workspace the caller has no
// membership in. Ensures the substrate's public configuration tier
// (agents, contracts, principles seeded under config/seed/system/) is
// reachable by dispatched agents per pr_substrate_provides_context.
func TestHandleDocumentList_SystemScopeWorkspaceBlind(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	if _, err := s.deps.Workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}

	// sty_e2512dbd: scope=system rows are non-tenant — no workspace_id.
	if _, err := s.deps.Documents.Create(ctx, document.Document{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Name:  "global-principle",
		Tags:  []string{"system"},
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed principle: %v", err)
	}

	otherCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_other", Source: "session"})

	res, _ := s.handleDocumentList(otherCtx, newCallToolReq("document_list", map[string]any{
		"type":  "principle",
		"scope": "system",
	}))
	rows := decodeArray(t, res)
	if len(rows) != 1 || nameOf(rows[0]) != "global-principle" {
		t.Fatalf("user_other list(scope=system) = %+v, want one row global-principle", rows)
	}
}

// TestHandleDocumentList_MixedScopeUnion covers the AC: a list call
// without a scope filter returns the union of scope=system rows
// (workspace-blind) plus rows in the caller's tier (workspace tier
// keyed by the caller's default workspace), with cross-tenant rows
// hidden. Sty_08196787 promoted this union to the per-tier ladder
// ResolveByName already used; the test exercises the workspace-tier
// rung that was previously unreachable for typed `_list` wrappers.
func TestHandleDocumentList_MixedScopeUnion(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	wsAlice, err := s.deps.Workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}
	wsBob, err := s.deps.Workspaces.Create(ctx, "user_bob", "bob-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("bob ws: %v", err)
	}

	// sty_e2512dbd: scope=system rows have no workspace_id; tenant rows do.
	mk := func(wsID, scope, name string) {
		t.Helper()
		doc := document.Document{
			Type:  document.TypeRole,
			Scope: scope,
			Name:  name,
		}
		if scope != document.ScopeSystem {
			doc.WorkspaceID = wsID
		}
		if _, err := s.deps.Documents.Create(ctx, doc, time.Now().UTC()); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	mk("", document.ScopeSystem, "system-row")
	mk(wsAlice.ID, document.ScopeWorkspace, "alice-row")
	mk(wsBob.ID, document.ScopeWorkspace, "bob-row")

	aliceCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_alice", Source: "session"})
	res, _ := s.handleDocumentList(aliceCtx, newCallToolReq("document_list", map[string]any{
		"type": "role",
	}))
	rows := decodeArray(t, res)

	names := map[string]bool{}
	for _, r := range rows {
		names[nameOf(r)] = true
	}
	if !names["system-row"] {
		t.Errorf("system-row missing from union list: %+v", rows)
	}
	if !names["alice-row"] {
		t.Errorf("alice-row missing from union list: %+v", rows)
	}
	if names["bob-row"] {
		t.Errorf("bob-row leaked across tenant boundary: %+v", rows)
	}
}

// TestHandleDocumentList_ProjectScopeTenantIsolation covers the
// negative branch: scope=project reads remain membership-scoped, so a
// caller with no membership in the target workspace gets an empty
// result.
func TestHandleDocumentList_ProjectScopeTenantIsolation(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	wsAlice, err := s.deps.Workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}
	if _, err := s.deps.Workspaces.Create(ctx, "user_bob", "bob-tier", time.Now().UTC()); err != nil {
		t.Fatalf("bob ws: %v", err)
	}

	aliceProj := "proj_alice"
	if _, err := s.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: wsAlice.ID,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeProject,
		ProjectID:   &aliceProj,
		Name:        "alice-only",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("alice principle: %v", err)
	}

	bobCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_bob", Source: "session"})
	res, _ := s.handleDocumentList(bobCtx, newCallToolReq("document_list", map[string]any{
		"type":       "principle",
		"scope":      "project",
		"project_id": aliceProj,
	}))
	rows := decodeArray(t, res)
	if len(rows) != 0 {
		t.Errorf("bob saw alice's project row: %+v", rows)
	}
}

// TestHandleDocumentGet_SystemScopeByID covers the AC: a caller in any
// workspace can resolve a scope=system row by id.
func TestHandleDocumentGet_SystemScopeByID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	if _, err := s.deps.Workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}

	doc, err := s.deps.Documents.Create(ctx, document.Document{
		Type:  document.TypeAgent,
		Scope: document.ScopeSystem,
		Name:  "developer_agent",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	otherCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_other", Source: "session"})
	res, _ := s.handleDocumentGet(otherCtx, newCallToolReq("document_get", map[string]any{
		"id": doc.ID,
	}))
	if res.IsError {
		t.Fatalf("scope=system get-by-id was rejected: %s", firstText(res))
	}
	got := decodeOne(t, res)
	if got["name"] != "developer_agent" {
		t.Errorf("got name=%v, want developer_agent", got["name"])
	}
}

// TestHandleDocumentGet_TenantIsolatedByID covers the negative branch:
// a workspace-scope row stays invisible to callers without the
// matching workspace membership.
func TestHandleDocumentGet_TenantIsolatedByID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	wsAlice, err := s.deps.Workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}
	if _, err := s.deps.Workspaces.Create(ctx, "user_bob", "bob-tier", time.Now().UTC()); err != nil {
		t.Fatalf("bob ws: %v", err)
	}

	doc, err := s.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: wsAlice.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "alice-only",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("alice role: %v", err)
	}

	bobCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_bob", Source: "session"})
	res, _ := s.handleDocumentGet(bobCtx, newCallToolReq("document_get", map[string]any{
		"id": doc.ID,
	}))
	if !res.IsError {
		t.Fatalf("bob resolved alice's workspace-scope row: %s", firstText(res))
	}
}

// TestHandleDocumentGet_SystemScopeByName covers the system-tier name
// resolution path: a caller looking up by name (no project_id) gets
// the scope=system row even though the handler resolves a project
// context for the fallback path.
func TestHandleDocumentGet_SystemScopeByName(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	if _, err := s.deps.Workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}
	if _, err := s.deps.Documents.Create(ctx, document.Document{
		Type:  document.TypeAgent,
		Scope: document.ScopeSystem,
		Name:  "developer_agent",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	otherCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_other", Source: "session"})
	res, _ := s.handleDocumentGet(otherCtx, newCallToolReq("document_get", map[string]any{
		"name": "developer_agent",
	}))
	if res.IsError {
		t.Fatalf("get-by-name on system agent failed: %s", firstText(res))
	}
	got := decodeOne(t, res)
	if got["name"] != "developer_agent" {
		t.Errorf("got name=%v, want developer_agent", got["name"])
	}
}

// TestHandleDocumentGet_WorkspaceTierByName confirms the hierarchical
// name resolver reaches the workspace tier — the rung that was
// unreachable pre-sty_e2bfeffa. A workspace-tier role is seeded for
// the caller's workspace; document_get(name=…) must return it.
func TestHandleDocumentGet_WorkspaceTierByName(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	ws, err := s.deps.Workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}

	wsRow, err := s.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: ws.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "wksp_role",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed workspace role: %v", err)
	}

	aliceCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_alice", Source: "session"})
	res, _ := s.handleDocumentGet(aliceCtx, newCallToolReq("document_get", map[string]any{
		"name": "wksp_role",
	}))
	if res.IsError {
		t.Fatalf("workspace-tier name resolve failed: %s", firstText(res))
	}
	got := decodeOne(t, res)
	if got["id"] != wsRow.ID {
		t.Errorf("got id=%v, want %q", got["id"], wsRow.ID)
	}
	if got["scope"] != document.ScopeWorkspace {
		t.Errorf("got scope=%v, want %q", got["scope"], document.ScopeWorkspace)
	}
}

// TestAgentGetWrapper_TypeFilter seeds a contract and an agent that
// share the same name. agent_get(name=…) must return the agent row,
// not the contract — the tightened typed wrapper pins the type filter
// before delegating to handleDocumentGet.
func TestAgentGetWrapper_TypeFilter(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	s.registerDocumentWrappers()
	ctx := context.Background()

	contractRow, err := s.deps.Documents.Create(ctx, document.Document{
		Type:       document.TypeContract,
		Scope:      document.ScopeSystem,
		Name:       "develop",
		Structured: []byte(`{"category":"develop","required_for_close":false,"validation_mode":"llm"}`),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	agentRow, err := s.deps.Documents.Create(ctx, document.Document{
		Type:  document.TypeAgent,
		Scope: document.ScopeSystem,
		Name:  "develop",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := s.deps.Workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}
	otherCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_other", Source: "session"})
	handler := s.wrapperGet(document.TypeAgent)
	res, _ := handler(otherCtx, newCallToolReq("agent_get", map[string]any{
		"name": "develop",
	}))
	if res.IsError {
		t.Fatalf("agent_get(name=develop) failed: %s", firstText(res))
	}
	got := decodeOne(t, res)
	if got["id"] != agentRow.ID {
		t.Errorf("agent_get returned id=%v, want %q (contract was %q)", got["id"], agentRow.ID, contractRow.ID)
	}
	if got["type"] != document.TypeAgent {
		t.Errorf("agent_get returned type=%v, want %q", got["type"], document.TypeAgent)
	}
}

// TestAgentGetWrapper_TypeFilterByID seeds a contract and an agent
// with distinct names. agent_get(id=<contract_id>) must reject with a
// type-mismatch error so a typed wrapper does not return a row of the
// wrong kind. Mirrors the name-based filter test for the id branch
// (sty_7cfe5e29).
func TestAgentGetWrapper_TypeFilterByID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	s.registerDocumentWrappers()
	ctx := context.Background()

	contractRow, err := s.deps.Documents.Create(ctx, document.Document{
		Type:       document.TypeContract,
		Scope:      document.ScopeSystem,
		Name:       "contract_only",
		Structured: []byte(`{"category":"develop","required_for_close":false,"validation_mode":"llm"}`),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	agentRow, err := s.deps.Documents.Create(ctx, document.Document{
		Type:  document.TypeAgent,
		Scope: document.ScopeSystem,
		Name:  "agent_only",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := s.deps.Workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}
	otherCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_other", Source: "session"})
	handler := s.wrapperGet(document.TypeAgent)

	resMatch, _ := handler(otherCtx, newCallToolReq("agent_get", map[string]any{
		"id": agentRow.ID,
	}))
	if resMatch.IsError {
		t.Fatalf("agent_get(id=<agent>) failed: %s", firstText(resMatch))
	}
	got := decodeOne(t, resMatch)
	if got["id"] != agentRow.ID {
		t.Errorf("agent_get returned id=%v, want %q", got["id"], agentRow.ID)
	}

	resMismatch, _ := handler(otherCtx, newCallToolReq("agent_get", map[string]any{
		"id": contractRow.ID,
	}))
	if !resMismatch.IsError {
		t.Fatalf("agent_get(id=<contract>) should error on type mismatch; got body=%s", firstText(resMismatch))
	}
	msg := firstText(resMismatch)
	if !strings.Contains(msg, "not \"agent\"") {
		t.Errorf("agent_get(id=<contract>) error = %q, want type-mismatch text mentioning agent", msg)
	}
}

// TestAgentListWrapper_SystemScopeWorkspaceBlind covers the wrapper
// layer (agent_list → handleDocumentList) end-to-end so a regression
// in wrapperList that bypassed the helper would surface here.
func TestAgentListWrapper_SystemScopeWorkspaceBlind(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	s.registerDocumentWrappers()
	ctx := context.Background()

	if _, err := s.deps.Workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}
	for _, name := range []string{"developer_agent", "releaser_agent", "story_close_agent"} {
		if _, err := s.deps.Documents.Create(ctx, document.Document{
			Type:  document.TypeAgent,
			Scope: document.ScopeSystem,
			Name:  name,
		}, time.Now().UTC()); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	otherCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_other", Source: "session"})
	handler := s.wrapperList(document.TypeAgent)
	res, _ := handler(otherCtx, newCallToolReq("agent_list", map[string]any{
		"scope": "system",
	}))
	rows := decodeArray(t, res)
	if len(rows) != 3 {
		t.Errorf("agent_list(scope=system) = %d rows, want 3 (developer_agent, releaser_agent, story_close_agent)", len(rows))
	}
}

// TestContractListWrapper_HierarchicalTiers covers the wrapper-layer
// routing for sty_08196787: contract_list reaches the workspace-tier
// rung that was unreachable for typed `_list` wrappers pre-fix. The
// fixture shape mirrors the live workspace-tier contracts (sty_690b1653
// `review`-style: scope=workspace with WorkspaceID set and ProjectID
// nil), so the test proves the workspace tier is reached via the
// workspace_id cascade — not via the row's project_id field.
func TestContractListWrapper_HierarchicalTiers(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	s.registerDocumentWrappers()
	ctx := context.Background()

	wsRow, err := s.deps.Workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}

	wkspContract, err := s.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: wsRow.ID,
		Type:        document.TypeContract,
		Scope:       document.ScopeWorkspace,
		Name:        "develop_wksp",
		Structured:  []byte(`{"category":"develop","required_for_close":false,"validation_mode":"llm"}`),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed workspace contract: %v", err)
	}

	aliceCtx := withCaller(ctx, auth.CallerIdentity{UserID: "user_alice", Source: "session"})
	handler := s.wrapperList(document.TypeContract)
	res, _ := handler(aliceCtx, newCallToolReq("contract_list", map[string]any{}))
	rows := decodeArray(t, res)

	found := false
	for _, r := range rows {
		if r["id"] == wkspContract.ID {
			if r["scope"] != document.ScopeWorkspace {
				t.Errorf("workspace-tier contract returned with scope=%v, want %q", r["scope"], document.ScopeWorkspace)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("contract_list missing workspace-tier row %q (got %+v)", wkspContract.ID, rows)
	}
}

func decodeOne(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || res.IsError {
		t.Fatalf("isError or nil: %+v", res)
	}
	text := firstText(res)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode object: %v; raw=%s", err, text)
	}
	return out
}

func firstText(res *mcpgo.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if t, ok := res.Content[0].(mcpgo.TextContent); ok {
		return t.Text
	}
	return ""
}

func decodeArray(t *testing.T, res *mcpgo.CallToolResult) []map[string]any {
	t.Helper()
	if res == nil || res.IsError {
		t.Fatalf("isError or nil: %+v", res)
	}
	text := firstText(res)
	if text == "" || text == "null" {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode array: %v; raw=%s", err, text)
	}
	return out
}

func nameOf(row map[string]any) string {
	v, _ := row["name"].(string)
	return v
}

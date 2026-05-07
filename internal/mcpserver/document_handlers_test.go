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
		DocStore:       docStore,
		WorkspaceStore: wsStore,
	})
}

// withCaller wraps ctx with the supplied identity so handler calls see
// the caller as if AuthMiddleware had run.
func withCaller(ctx context.Context, id CallerIdentity) context.Context {
	return context.WithValue(ctx, userKey, id)
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
	ctx := withCaller(context.Background(), CallerIdentity{UserID: "u_a", Source: "session"})

	// Seed a row to update.
	doc, err := s.docs.Create(ctx, document.Document{
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

	wsA, err := s.workspaces.Create(ctx, "user_alice", "alpha", time.Now().UTC())
	if err != nil {
		t.Fatalf("ws A: %v", err)
	}
	wsB, err := s.workspaces.Create(ctx, "user_bob", "beta", time.Now().UTC())
	if err != nil {
		t.Fatalf("ws B: %v", err)
	}
	if _, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: wsA.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "alice-only",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("alice role: %v", err)
	}
	if _, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: wsB.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "bob-only",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("bob role: %v", err)
	}

	aliceCtx := withCaller(ctx, CallerIdentity{UserID: "user_alice", Source: "session"})
	bobCtx := withCaller(ctx, CallerIdentity{UserID: "user_bob", Source: "session"})

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

// TestHandleDocumentCreate_ScopeSystemRejectsProjectID confirms the
// scope-vs-project-id invariant is enforced at the handler layer (not
// only in document.Validate at the store layer).
func TestHandleDocumentCreate_ScopeSystemRejectsProjectID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), CallerIdentity{UserID: "u_a", Source: "session"})

	res, err := s.handleDocumentCreate(ctx, newCallToolReq("document_create", map[string]any{
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

// TestHandleDocumentCreate_ScopeSystemDropsWorkspaceID covers
// sty_e2512dbd: when the caller creates a scope=system row, the
// handler MUST NOT stamp the caller's workspace on it. The system
// tier is non-tenant; a stamped workspace would violate Validate()
// and pull downstream readers into the wrong tenancy.
func TestHandleDocumentCreate_ScopeSystemDropsWorkspaceID(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), CallerIdentity{UserID: "user_creator", Source: "session"})

	res, err := s.handleDocumentCreate(ctx, newCallToolReq("document_create", map[string]any{
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

	doc, err := f.server.docs.GetByID(context.Background(), devID, nil)
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

	if _, err := s.workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}

	// sty_e2512dbd: scope=system rows are non-tenant — no workspace_id.
	if _, err := s.docs.Create(ctx, document.Document{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Name:  "global-principle",
		Tags:  []string{"system"},
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed principle: %v", err)
	}

	otherCtx := withCaller(ctx, CallerIdentity{UserID: "user_other", Source: "session"})

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
// (workspace-blind) plus rows in the caller's memberships, deduped by
// id, with cross-tenant rows hidden.
func TestHandleDocumentList_MixedScopeUnion(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := context.Background()

	wsAlice, err := s.workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}
	wsBob, err := s.workspaces.Create(ctx, "user_bob", "bob-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("bob ws: %v", err)
	}

	// sty_e2512dbd: scope=system rows have no workspace_id; tenant rows do.
	mk := func(wsID, scope, name string, projectID *string) {
		t.Helper()
		typ := document.TypePrinciple
		if scope == document.ScopeWorkspace {
			typ = document.TypeRole
		}
		doc := document.Document{
			Type:  typ,
			Scope: scope,
			Name:  name,
		}
		if scope != document.ScopeSystem {
			doc.WorkspaceID = wsID
		}
		if scope == document.ScopeProject {
			doc.ProjectID = projectID
		}
		if _, err := s.docs.Create(ctx, doc, time.Now().UTC()); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	mk("", document.ScopeSystem, "system-row", nil)
	aliceProj := "proj_alice"
	mk(wsAlice.ID, document.ScopeProject, "alice-row", &aliceProj)
	bobProj := "proj_bob"
	mk(wsBob.ID, document.ScopeProject, "bob-row", &bobProj)

	aliceCtx := withCaller(ctx, CallerIdentity{UserID: "user_alice", Source: "session"})
	res, _ := s.handleDocumentList(aliceCtx, newCallToolReq("document_list", map[string]any{
		"type": "principle",
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

	wsAlice, err := s.workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}
	if _, err := s.workspaces.Create(ctx, "user_bob", "bob-tier", time.Now().UTC()); err != nil {
		t.Fatalf("bob ws: %v", err)
	}

	aliceProj := "proj_alice"
	if _, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: wsAlice.ID,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeProject,
		ProjectID:   &aliceProj,
		Name:        "alice-only",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("alice principle: %v", err)
	}

	bobCtx := withCaller(ctx, CallerIdentity{UserID: "user_bob", Source: "session"})
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

	if _, err := s.workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}

	doc, err := s.docs.Create(ctx, document.Document{
		Type:  document.TypeAgent,
		Scope: document.ScopeSystem,
		Name:  "developer_agent",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	otherCtx := withCaller(ctx, CallerIdentity{UserID: "user_other", Source: "session"})
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

	wsAlice, err := s.workspaces.Create(ctx, "user_alice", "alice-tier", time.Now().UTC())
	if err != nil {
		t.Fatalf("alice ws: %v", err)
	}
	if _, err := s.workspaces.Create(ctx, "user_bob", "bob-tier", time.Now().UTC()); err != nil {
		t.Fatalf("bob ws: %v", err)
	}

	doc, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: wsAlice.ID,
		Type:        document.TypeRole,
		Scope:       document.ScopeWorkspace,
		Name:        "alice-only",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("alice role: %v", err)
	}

	bobCtx := withCaller(ctx, CallerIdentity{UserID: "user_bob", Source: "session"})
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

	if _, err := s.workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}
	if _, err := s.docs.Create(ctx, document.Document{
		Type:  document.TypeAgent,
		Scope: document.ScopeSystem,
		Name:  "developer_agent",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	otherCtx := withCaller(ctx, CallerIdentity{UserID: "user_other", Source: "session"})
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

// TestAgentListWrapper_SystemScopeWorkspaceBlind covers the wrapper
// layer (agent_list → handleDocumentList) end-to-end so a regression
// in wrapperList that bypassed the helper would surface here.
func TestAgentListWrapper_SystemScopeWorkspaceBlind(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	s.registerDocumentWrappers()
	ctx := context.Background()

	if _, err := s.workspaces.Create(ctx, "user_other", "other-tier", time.Now().UTC()); err != nil {
		t.Fatalf("other ws: %v", err)
	}
	for _, name := range []string{"developer_agent", "releaser_agent", "story_close_agent"} {
		if _, err := s.docs.Create(ctx, document.Document{
			Type:  document.TypeAgent,
			Scope: document.ScopeSystem,
			Name:  name,
		}, time.Now().UTC()); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	otherCtx := withCaller(ctx, CallerIdentity{UserID: "user_other", Source: "session"})
	handler := s.wrapperList(document.TypeAgent)
	res, _ := handler(otherCtx, newCallToolReq("agent_list", map[string]any{
		"scope": "system",
	}))
	rows := decodeArray(t, res)
	if len(rows) != 3 {
		t.Errorf("agent_list(scope=system) = %d rows, want 3 (developer_agent, releaser_agent, story_close_agent)", len(rows))
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

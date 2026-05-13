package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// newKVTestServer wires the dependencies the unified KV verbs need:
// the ledger store (for read+write), workspaces (for membership
// resolution), projects + stories + contracts + docs (so the
// registration site at mcp.go:376 advertises the kv_* tools).
func newKVTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)
	ledStore := ledger.NewMemoryStore()
	docStore := document.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	projStore := project.NewMemoryStore()
	wsStore := workspace.NewMemoryStore()
	sessionStore := session.NewMemoryStore()
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:  docStore,
			Projects:   projStore,
			Ledger:     ledStore,
			Stories:    storyStore,
			Workspaces: wsStore,
			Sessions:   sessionStore,
		},
	})
}

// TestKVTools_Registered confirms the four kv_* MCP tools land in the
// ListTools surface when the registration site's deps are wired.
func TestKVTools_Registered(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	tools := s.mcp.ListTools()
	for _, want := range []string{"kv_get", "kv_set", "kv_delete", "kv_list"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("registered tools missing %q (story_3d392258)", want)
		}
	}
}

func TestKVHandlers_RoundTrip_System(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	admin := auth.CallerIdentity{UserID: "u_admin", Source: "session", GlobalAdmin: true}
	ctx := withCaller(context.Background(), admin)

	// kv_set
	setRes, err := s.handleKVSet(ctx, newCallToolReq("kv_set", map[string]any{
		"scope": "system",
		"key":   "default_theme",
		"value": "dark",
	}))
	if err != nil {
		t.Fatalf("kv_set: %v", err)
	}
	if setRes.IsError {
		t.Fatalf("kv_set IsError: %s", firstText(setRes))
	}

	// kv_get
	getRes, err := s.handleKVGet(ctx, newCallToolReq("kv_get", map[string]any{
		"scope": "system",
		"key":   "default_theme",
	}))
	if err != nil {
		t.Fatalf("kv_get: %v", err)
	}
	if getRes.IsError {
		t.Fatalf("kv_get IsError: %s", firstText(getRes))
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(firstText(getRes)), &got)
	if got["value"] != "dark" {
		t.Errorf("kv_get value = %v, want dark", got["value"])
	}

	// kv_delete
	delRes, err := s.handleKVDelete(ctx, newCallToolReq("kv_delete", map[string]any{
		"scope": "system",
		"key":   "default_theme",
	}))
	if err != nil {
		t.Fatalf("kv_delete: %v", err)
	}
	if delRes.IsError {
		t.Fatalf("kv_delete IsError: %s", firstText(delRes))
	}

	// kv_get again — must be not_found
	getRes2, _ := s.handleKVGet(ctx, newCallToolReq("kv_get", map[string]any{
		"scope": "system",
		"key":   "default_theme",
	}))
	if !getRes2.IsError {
		t.Errorf("kv_get after delete: IsError = false, want true")
	}
}

func TestKVHandlers_RoundTrip_Workspace(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed a workspace + member so memberships resolve.
	ws, err := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	alice := auth.CallerIdentity{UserID: "u_alice", Source: "session"}
	ctx = withCaller(ctx, alice)

	setRes, err := s.handleKVSet(ctx, newCallToolReq("kv_set", map[string]any{
		"scope":        "workspace",
		"workspace_id": ws.ID,
		"key":          "tier",
		"value":        "gold",
	}))
	if err != nil || setRes.IsError {
		t.Fatalf("kv_set: %v / %s", err, firstText(setRes))
	}
	listRes, err := s.handleKVList(ctx, newCallToolReq("kv_list", map[string]any{
		"scope":        "workspace",
		"workspace_id": ws.ID,
	}))
	if err != nil || listRes.IsError {
		t.Fatalf("kv_list: %v / %s", err, firstText(listRes))
	}
	var listed map[string]any
	_ = json.Unmarshal([]byte(firstText(listRes)), &listed)
	if c, ok := listed["count"].(float64); !ok || c != 1 {
		t.Errorf("workspace kv_list count = %v, want 1", listed["count"])
	}
}

func TestKVHandlers_RoundTrip_User(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)

	alice := auth.CallerIdentity{UserID: "u_alice", Source: "session"}
	ctx = withCaller(ctx, alice)

	setRes, _ := s.handleKVSet(ctx, newCallToolReq("kv_set", map[string]any{
		"scope":        "user",
		"workspace_id": ws.ID,
		"key":          "theme",
		"value":        "dark",
	}))
	if setRes.IsError {
		t.Fatalf("kv_set: %s", firstText(setRes))
	}
	getRes, _ := s.handleKVGet(ctx, newCallToolReq("kv_get", map[string]any{
		"scope":        "user",
		"workspace_id": ws.ID,
		"key":          "theme",
	}))
	if getRes.IsError {
		t.Fatalf("kv_get: %s", firstText(getRes))
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(firstText(getRes)), &got)
	if got["value"] != "dark" {
		t.Errorf("user kv_get value = %v, want dark", got["value"])
	}
}

func TestKVHandlers_InvalidScope(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	res, _ := s.handleKVGet(ctx, newCallToolReq("kv_get", map[string]any{
		"scope": "global",
		"key":   "x",
	}))
	if !res.IsError {
		t.Fatal("expected error for invalid scope")
	}
}

func TestKVHandlers_SystemSetRequiresGlobalAdmin(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session", GlobalAdmin: false})

	res, _ := s.handleKVSet(ctx, newCallToolReq("kv_set", map[string]any{
		"scope": "system",
		"key":   "policy",
		"value": "strict",
	}))
	if !res.IsError {
		t.Fatal("expected forbidden for non-admin scope=system write")
	}
}

func TestKVHandlers_WorkspaceFKMissing(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	res, _ := s.handleKVSet(ctx, newCallToolReq("kv_set", map[string]any{
		"scope": "workspace",
		"key":   "x",
		"value": "y",
	}))
	if !res.IsError {
		t.Fatal("expected error when workspace_id is missing for scope=workspace")
	}
}

// TestKVHandlers_AuthMatrix exercises every scope × caller-role combo
// the v1 auth gate covers (story_eb17cb16). Read paths are intentionally
// not gated; only kv_set is exercised here.
func TestKVHandlers_AuthMatrix(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Two workspaces with different admins; a project owned by alice in ws1.
	ws1, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws1.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws1.ID, "u_bob", workspace.RoleMember, "u_alice", now)

	ws2, _ := s.deps.Workspaces.Create(ctx, "u_carol", "bravo", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws2.ID, "u_carol", workspace.RoleAdmin, "u_carol", now)

	proj, _ := s.deps.Projects.Create(ctx, "u_alice", ws1.ID, "alpha-1", now)

	type tc struct {
		name      string
		caller    auth.CallerIdentity
		args      map[string]any
		wantError bool
	}
	cases := []tc{
		// system
		{"system/admin/ok", auth.CallerIdentity{UserID: "u_admin", GlobalAdmin: true}, map[string]any{"scope": "system", "key": "policy", "value": "v"}, false},
		{"system/non-admin/forbidden", auth.CallerIdentity{UserID: "u_alice"}, map[string]any{"scope": "system", "key": "policy", "value": "v"}, true},
		// workspace
		{"workspace/admin/ok", auth.CallerIdentity{UserID: "u_alice"}, map[string]any{"scope": "workspace", "workspace_id": ws1.ID, "key": "tier", "value": "gold"}, false},
		{"workspace/global-admin/ok", auth.CallerIdentity{UserID: "u_admin", GlobalAdmin: true}, map[string]any{"scope": "workspace", "workspace_id": ws1.ID, "key": "tier", "value": "gold"}, false},
		{"workspace/member/forbidden", auth.CallerIdentity{UserID: "u_bob"}, map[string]any{"scope": "workspace", "workspace_id": ws1.ID, "key": "tier", "value": "gold"}, true},
		{"workspace/non-member/forbidden", auth.CallerIdentity{UserID: "u_carol"}, map[string]any{"scope": "workspace", "workspace_id": ws1.ID, "key": "tier", "value": "gold"}, true},
		// project
		{"project/owner/ok", auth.CallerIdentity{UserID: "u_alice"}, map[string]any{"scope": "project", "project_id": proj.ID, "key": "feat", "value": "on"}, false},
		{"project/ws-admin/ok", auth.CallerIdentity{UserID: "u_alice"}, map[string]any{"scope": "project", "project_id": proj.ID, "key": "feat", "value": "on"}, false},
		{"project/member-not-owner/forbidden", auth.CallerIdentity{UserID: "u_bob"}, map[string]any{"scope": "project", "project_id": proj.ID, "key": "feat", "value": "on"}, true},
		{"project/other-workspace/forbidden", auth.CallerIdentity{UserID: "u_carol"}, map[string]any{"scope": "project", "project_id": proj.ID, "key": "feat", "value": "on"}, true},
		// user
		{"user/self/ok", auth.CallerIdentity{UserID: "u_alice"}, map[string]any{"scope": "user", "workspace_id": ws1.ID, "key": "theme", "value": "dark"}, false},
		{"user/cross-user/forbidden", auth.CallerIdentity{UserID: "u_bob"}, map[string]any{"scope": "user", "workspace_id": ws1.ID, "user_id": "u_alice", "key": "theme", "value": "dark"}, true},
		{"user/global-admin-cross/forbidden", auth.CallerIdentity{UserID: "u_admin", GlobalAdmin: true}, map[string]any{"scope": "user", "workspace_id": ws1.ID, "user_id": "u_alice", "key": "theme", "value": "dark"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			authedCtx := withCaller(ctx, c.caller)
			res, _ := s.handleKVSet(authedCtx, newCallToolReq("kv_set", c.args))
			if c.wantError && !res.IsError {
				t.Fatalf("kv_set: expected error, got success: %s", firstText(res))
			}
			if !c.wantError && res.IsError {
				t.Fatalf("kv_set: expected success, got error: %s", firstText(res))
			}
		})
	}
}

// TestKVHandlers_GetResolved exercises the precedence chain end-to-end
// through the MCP handler. story_405b7221.
func TestKVHandlers_GetResolved(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)
	proj, _ := s.deps.Projects.Create(ctx, "u_alice", ws.ID, "alpha-1", now)

	admin := auth.CallerIdentity{UserID: "u_admin", GlobalAdmin: true}
	alice := auth.CallerIdentity{UserID: "u_alice"}

	// Seed every tier with the same key.
	_, _ = s.handleKVSet(withCaller(ctx, admin), newCallToolReq("kv_set", map[string]any{
		"scope": "system", "key": "theme", "value": "system-value",
	}))
	_, _ = s.handleKVSet(withCaller(ctx, alice), newCallToolReq("kv_set", map[string]any{
		"scope": "workspace", "workspace_id": ws.ID, "key": "theme", "value": "ws-value",
	}))
	_, _ = s.handleKVSet(withCaller(ctx, alice), newCallToolReq("kv_set", map[string]any{
		"scope": "project", "project_id": proj.ID, "key": "theme", "value": "proj-value",
	}))
	_, _ = s.handleKVSet(withCaller(ctx, alice), newCallToolReq("kv_set", map[string]any{
		"scope": "user", "workspace_id": ws.ID, "user_id": "u_alice", "key": "theme", "value": "user-value",
	}))

	// system wins
	res, _ := s.handleKVGetResolved(withCaller(ctx, alice), newCallToolReq("kv_get_resolved", map[string]any{
		"key":          "theme",
		"workspace_id": ws.ID,
		"project_id":   proj.ID,
	}))
	if res.IsError {
		t.Fatalf("kv_get_resolved error: %s", firstText(res))
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(firstText(res)), &got)
	if got["value"] != "system-value" {
		t.Errorf("system-tier present but got %v, want system-value", got["value"])
	}
	if got["resolved_scope"] != "system" {
		t.Errorf("resolved_scope = %v, want system", got["resolved_scope"])
	}
}

// TestKVHandlers_GetResolved_NotFound — kv_get_resolved returns
// not_found when no scope has the key.
func TestKVHandlers_GetResolved_NotFound(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)

	res, _ := s.handleKVGetResolved(withCaller(ctx, auth.CallerIdentity{UserID: "u_alice"}),
		newCallToolReq("kv_get_resolved", map[string]any{
			"key":          "nonexistent",
			"workspace_id": ws.ID,
		}))
	if !res.IsError {
		t.Fatal("expected not_found error")
	}
}

// TestKVHandlers_DeleteHonoursAuth confirms kv_delete uses the same
// gate as kv_set.
func TestKVHandlers_DeleteHonoursAuth(t *testing.T) {
	t.Parallel()
	s := newKVTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_bob", workspace.RoleMember, "u_alice", now)

	// Workspace member (non-admin) cannot delete a workspace-scope key.
	bobCtx := withCaller(ctx, auth.CallerIdentity{UserID: "u_bob"})
	res, _ := s.handleKVDelete(bobCtx, newCallToolReq("kv_delete", map[string]any{
		"scope":        "workspace",
		"workspace_id": ws.ID,
		"key":          "tier",
	}))
	if !res.IsError {
		t.Fatal("kv_delete: workspace member expected forbidden")
	}
}

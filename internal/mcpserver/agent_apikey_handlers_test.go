package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// newAPIKeyTestServer wires the minimal Server surface the
// agent_apikey_* handlers consume: an in-memory APIKeyStore, an
// in-memory ledger (so the kind:agent-apikey-* rows land somewhere
// inspectable), and project/workspace stores so the project_id
// branch resolves a workspace_id.
func newAPIKeyTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	led := ledger.NewMemoryStore()
	wss := workspace.NewMemoryStore()
	projects := project.NewMemoryStore()
	keyStore := auth.NewMemoryAgentAPIKeyStore()
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Ledger:    led,
			Workspaces: wss,
			Projects:   projects,
			APIKeys:    keyStore,
		},
		NowFunc:        func() time.Time { return now },
	})
}

func mintViaHandler(t *testing.T, s *Server, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.handleAgentAPIKeyCreate(ctx, newCallToolReq("agent_apikey_create", args))
	if err != nil {
		t.Fatalf("handleAgentAPIKeyCreate err: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleAgentAPIKeyCreate isError: %+v", res.Content)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, text)
	}
	return out
}

// TestAgentAPIKeyCreate_HappyPath asserts the create response
// contains the cleartext key once, the row in the store has
// hash+salt+prefix populated, and a kind:agent-apikey-created
// ledger row is appended.
func TestAgentAPIKeyCreate_HappyPath(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Email: "alice@local", Source: "session"})

	resp := mintViaHandler(t, s, ctx, map[string]any{"name": "alice-laptop"})

	id, _ := resp["id"].(string)
	cleartext, _ := resp["key"].(string)
	prefix, _ := resp["prefix"].(string)
	if id == "" || cleartext == "" || prefix == "" {
		t.Fatalf("create resp missing fields: %+v", resp)
	}
	if !strings.HasPrefix(cleartext, auth.APIKeyPrefix) {
		t.Errorf("cleartext %q missing %q prefix", cleartext, auth.APIKeyPrefix)
	}
	if len(cleartext) < 40 {
		t.Errorf("cleartext len = %d, want >= 40", len(cleartext))
	}

	row, err := s.deps.APIKeys.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.KeyHash == "" || row.KeySalt == "" || row.Prefix == "" {
		t.Errorf("row missing hash/salt/prefix: %+v", row)
	}
	if row.OwnerUserID != "u_alice" {
		t.Errorf("OwnerUserID = %q, want u_alice", row.OwnerUserID)
	}

	// kind:agent-apikey-created ledger row was written.
	rows, _ := s.deps.Ledger.List(context.Background(), "", ledger.ListOptions{Limit: 10}, nil)
	found := false
	for _, r := range rows {
		for _, tag := range r.Tags {
			if tag == "kind:agent-apikey-created" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected kind:agent-apikey-created ledger row")
	}
}

// TestAgentAPIKeyCreate_HashAtRest asserts the on-disk row's
// KeyHash equals hex(sha256(KeySalt || cleartext)) and that the
// cleartext does NOT appear in the marshalled row JSON. AC4.
func TestAgentAPIKeyCreate_HashAtRest(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	resp := mintViaHandler(t, s, ctx, map[string]any{"name": "hash-at-rest"})
	id := resp["id"].(string)
	cleartext := resp["key"].(string)

	row, _ := s.deps.APIKeys.Get(context.Background(), id)
	saltBytes, err := hex.DecodeString(row.KeySalt)
	if err != nil {
		t.Fatalf("salt hex: %v", err)
	}
	expected := sha256.Sum256(append(saltBytes, []byte(cleartext)...))
	want := hex.EncodeToString(expected[:])
	if row.KeyHash != want {
		t.Errorf("KeyHash = %q, want %q", row.KeyHash, want)
	}

	rowJSON, _ := json.Marshal(row)
	if strings.Contains(string(rowJSON), cleartext) {
		t.Errorf("row JSON leaks cleartext: %s", rowJSON)
	}
}

// TestAgentAPIKeyList_RedactsHashAndSalt asserts the list response
// (the operator-facing wire shape) carries id+prefix+name etc but
// MUST NOT include key, key_hash, or key_salt. AC2.
func TestAgentAPIKeyList_RedactsHashAndSalt(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	mintViaHandler(t, s, ctx, map[string]any{"name": "k1"})

	res, _ := s.handleAgentAPIKeyList(ctx, newCallToolReq("agent_apikey_list", map[string]any{}))
	if res.IsError {
		t.Fatalf("list isError: %+v", res.Content)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	for _, forbidden := range []string{`"key"`, `"key_hash"`, `"key_salt"`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("list response leaks %q field: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"prefix"`) {
		t.Errorf("list response missing prefix field: %s", text)
	}
}

// TestAgentAPIKeyList_FiltersByOwnerAndProject covers AC2's
// per-owner + per-project filter behaviour through the handler
// surface.
func TestAgentAPIKeyList_FiltersByOwnerAndProject(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	aliceCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	bobCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_bob", Source: "session"})

	mintViaHandler(t, s, aliceCtx, map[string]any{"name": "alice-1"})
	mintViaHandler(t, s, aliceCtx, map[string]any{"name": "alice-2"})
	mintViaHandler(t, s, bobCtx, map[string]any{"name": "bob-1"})

	listAlice, _ := s.handleAgentAPIKeyList(aliceCtx, newCallToolReq("agent_apikey_list", map[string]any{}))
	textA := listAlice.Content[0].(mcpgo.TextContent).Text
	var outA map[string]any
	_ = json.Unmarshal([]byte(textA), &outA)
	if int(outA["count"].(float64)) != 2 {
		t.Errorf("alice list count = %v, want 2", outA["count"])
	}
	listBob, _ := s.handleAgentAPIKeyList(bobCtx, newCallToolReq("agent_apikey_list", map[string]any{}))
	textB := listBob.Content[0].(mcpgo.TextContent).Text
	var outB map[string]any
	_ = json.Unmarshal([]byte(textB), &outB)
	if int(outB["count"].(float64)) != 1 {
		t.Errorf("bob list count = %v, want 1", outB["count"])
	}
	if strings.Contains(textB, "alice-1") {
		t.Errorf("bob list leaks alice's row: %s", textB)
	}
}

// TestAgentAPIKeyDelete_HappyPath covers AC3: soft-delete flips
// status, list excludes the row, and a kind:agent-apikey-archived
// ledger row is appended.
func TestAgentAPIKeyDelete_HappyPath(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	mint := mintViaHandler(t, s, ctx, map[string]any{"name": "delete-me"})
	id := mint["id"].(string)

	res, _ := s.handleAgentAPIKeyDelete(ctx, newCallToolReq("agent_apikey_delete", map[string]any{"id": id}))
	if res.IsError {
		t.Fatalf("delete isError: %+v", res.Content)
	}
	row, _ := s.deps.APIKeys.Get(context.Background(), id)
	if row.Status != auth.APIKeyStatusArchived {
		t.Errorf("Status = %q, want archived", row.Status)
	}

	listRes, _ := s.handleAgentAPIKeyList(ctx, newCallToolReq("agent_apikey_list", map[string]any{}))
	listText := listRes.Content[0].(mcpgo.TextContent).Text
	if strings.Contains(listText, id) {
		t.Errorf("post-delete list still shows id %q: %s", id, listText)
	}

	rows, _ := s.deps.Ledger.List(context.Background(), "", ledger.ListOptions{Limit: 10}, nil)
	found := false
	for _, r := range rows {
		for _, tag := range r.Tags {
			if tag == "kind:agent-apikey-archived" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected kind:agent-apikey-archived ledger row")
	}
}

// TestAgentAPIKeyDelete_CrossOwnerForbidden covers AC3: caller B
// trying to archive caller A's row gets a forbidden error and the
// row stays active.
func TestAgentAPIKeyDelete_CrossOwnerForbidden(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	aliceCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	bobCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_bob", Source: "session"})
	mint := mintViaHandler(t, s, aliceCtx, map[string]any{"name": "alice-secret"})
	id := mint["id"].(string)

	res, _ := s.handleAgentAPIKeyDelete(bobCtx, newCallToolReq("agent_apikey_delete", map[string]any{"id": id}))
	if !res.IsError {
		t.Fatalf("expected forbidden, got success: %+v", res.Content)
	}
	row, _ := s.deps.APIKeys.Get(context.Background(), id)
	if row.Status != auth.APIKeyStatusActive {
		t.Errorf("post-forbidden Status = %q, want active", row.Status)
	}
}

// TestAgentAPIKeyDelete_GlobalAdminCanDeleteAny covers AC3:
// GlobalAdmin=true caller can soft-delete another user's row.
func TestAgentAPIKeyDelete_GlobalAdminCanDeleteAny(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	aliceCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	adminCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_admin", Source: "session", GlobalAdmin: true})
	mint := mintViaHandler(t, s, aliceCtx, map[string]any{"name": "alice-key"})
	id := mint["id"].(string)

	res, _ := s.handleAgentAPIKeyDelete(adminCtx, newCallToolReq("agent_apikey_delete", map[string]any{"id": id}))
	if res.IsError {
		t.Fatalf("admin delete isError: %+v", res.Content)
	}
	row, _ := s.deps.APIKeys.Get(context.Background(), id)
	if row.Status != auth.APIKeyStatusArchived {
		t.Errorf("admin-deleted Status = %q, want archived", row.Status)
	}
}

// TestAgentAPIKeyCreate_RejectsInvalidExpiresAt covers AC1: malformed
// RFC3339 in expires_at returns a structured validation error.
func TestAgentAPIKeyCreate_RejectsInvalidExpiresAt(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	res, _ := s.handleAgentAPIKeyCreate(ctx, newCallToolReq("agent_apikey_create", map[string]any{
		"name":       "bad-expiry",
		"expires_at": "yesterday",
	}))
	if !res.IsError {
		t.Fatalf("expected validation error: %+v", res.Content)
	}
	body := res.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(body, "invalid_expires_at") {
		t.Errorf("error body missing invalid_expires_at: %s", body)
	}
}

// TestAgentAPIKeyCreate_RejectsEmptyName covers AC1's required-name
// validation.
func TestAgentAPIKeyCreate_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	res, _ := s.handleAgentAPIKeyCreate(ctx, newCallToolReq("agent_apikey_create", map[string]any{}))
	if !res.IsError {
		t.Fatalf("expected required-arg error: %+v", res.Content)
	}
}

// TestAgentAPIKeyList_GlobalAdminSeesAllOwners covers AC2: an admin
// caller's list spans every owner.
func TestAgentAPIKeyList_GlobalAdminSeesAllOwners(t *testing.T) {
	t.Parallel()
	s := newAPIKeyTestServer(t)
	aliceCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice", Source: "session"})
	bobCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_bob", Source: "session"})
	adminCtx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_admin", Source: "session", GlobalAdmin: true})
	mintViaHandler(t, s, aliceCtx, map[string]any{"name": "a"})
	mintViaHandler(t, s, bobCtx, map[string]any{"name": "b"})

	res, _ := s.handleAgentAPIKeyList(adminCtx, newCallToolReq("agent_apikey_list", map[string]any{}))
	if res.IsError {
		t.Fatalf("admin list isError: %+v", res.Content)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	var out map[string]any
	_ = json.Unmarshal([]byte(text), &out)
	if int(out["count"].(float64)) != 2 {
		t.Errorf("admin list count = %v, want 2", out["count"])
	}
}

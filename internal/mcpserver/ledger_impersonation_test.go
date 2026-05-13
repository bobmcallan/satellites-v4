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

// impersonationFixture builds a server with two workspaces and the
// caller as a member of one. Tests then call handleLedgerAppend against
// the home workspace and the foreign workspace to verify the
// impersonating_as_workspace stamping behaviour. story_3548cde2.
type impersonationFixture struct {
	t              *testing.T
	server         *Server
	ctx            context.Context
	homeWS         string
	foreignWS      string
	homeProject    string
	foreignProject string
	caller         auth.CallerIdentity
}

func newImpersonationFixture(t *testing.T, callerGlobalAdmin bool) *impersonationFixture {
	t.Helper()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cfg := &config.Config{Env: "dev"}

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	projStore := project.NewMemoryStore()
	sessionStore := session.NewMemoryStore()

	home, err := wsStore.Create(ctx, "u_alice", "home", now)
	if err != nil {
		t.Fatalf("home ws: %v", err)
	}
	foreign, err := wsStore.Create(ctx, "u_other", "foreign", now)
	if err != nil {
		t.Fatalf("foreign ws: %v", err)
	}
	homeProj, err := projStore.Create(ctx, "u_alice", home.ID, "p_home", now)
	if err != nil {
		t.Fatalf("home proj: %v", err)
	}
	foreignProj, err := projStore.Create(ctx, "u_other", foreign.ID, "p_foreign", now)
	if err != nil {
		t.Fatalf("foreign proj: %v", err)
	}

	server := New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:  docStore,
			Projects:   projStore,
			Ledger:     ledStore,
			Stories:    storyStore,
			Workspaces: wsStore,
			Sessions:   sessionStore,
		},
	})

	caller := auth.CallerIdentity{
		Email:       "alice@example.com",
		UserID:      "u_alice",
		Source:      "session",
		GlobalAdmin: callerGlobalAdmin,
	}

	return &impersonationFixture{
		t:              t,
		server:         server,
		ctx:            ctx,
		homeWS:         home.ID,
		foreignWS:      foreign.ID,
		homeProject:    homeProj.ID,
		foreignProject: foreignProj.ID,
		caller:         caller,
	}
}

func (f *impersonationFixture) callerCtx() context.Context {
	return auth.WithCaller(f.ctx, f.caller)
}

// readEntry parses the handleLedgerAppend response into a LedgerEntry.
func (f *impersonationFixture) readEntry(t *testing.T, raw string) ledger.LedgerEntry {
	t.Helper()
	var e ledger.LedgerEntry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("parse: %v (body=%q)", err, raw)
	}
	return e
}

// TestLedgerAppend_GlobalAdminInOwnWorkspace_FieldEmpty covers AC5 (a):
// when a global_admin writes into a workspace they are a member of,
// the impersonating_as_workspace field stays empty.
func TestLedgerAppend_GlobalAdminInOwnWorkspace_FieldEmpty(t *testing.T) {
	t.Parallel()
	f := newImpersonationFixture(t, true)

	res, err := f.server.handleLedgerAppend(f.callerCtx(), newCallToolReq("ledger_add", map[string]any{
		"project_id":  f.homeProject,
		"type":        ledger.TypeDecision,
		"content":     "in own workspace",
		"durability":  ledger.DurabilityDurable,
		"source_type": ledger.SourceAgent,
	}))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	entry := f.readEntry(t, firstText(res))
	if entry.ImpersonatingAsWorkspace != "" {
		t.Errorf("expected empty ImpersonatingAsWorkspace for in-workspace write; got %q", entry.ImpersonatingAsWorkspace)
	}
}

// TestLedgerAppend_GlobalAdminCrossWorkspace_FieldStamped covers AC5 (b):
// when a global_admin writes into a workspace they are NOT a member of,
// the impersonating_as_workspace field is set to that workspace_id.
func TestLedgerAppend_GlobalAdminCrossWorkspace_FieldStamped(t *testing.T) {
	t.Parallel()
	f := newImpersonationFixture(t, true)

	res, err := f.server.handleLedgerAppend(f.callerCtx(), newCallToolReq("ledger_add", map[string]any{
		"project_id":  f.foreignProject,
		"type":        ledger.TypeDecision,
		"content":     "cross-workspace assist",
		"durability":  ledger.DurabilityDurable,
		"source_type": ledger.SourceAgent,
	}))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %s", firstText(res))
	}
	entry := f.readEntry(t, firstText(res))
	if entry.ImpersonatingAsWorkspace != f.foreignWS {
		t.Errorf("expected ImpersonatingAsWorkspace=%q for cross-workspace write; got %q", f.foreignWS, entry.ImpersonatingAsWorkspace)
	}
}

// TestLedgerAppend_NonAdminCrossWorkspace_Denied covers AC5 (c): the
// impersonation field is admin-only — non-admin callers do not bypass
// the access check, so a cross-workspace write is denied at
// resolveProjectID. The point: the only path that *could* stamp the
// field requires global_admin first; non-admin can never produce a
// stamped row.
func TestLedgerAppend_NonAdminCrossWorkspace_Denied(t *testing.T) {
	t.Parallel()
	f := newImpersonationFixture(t, false)

	res, err := f.server.handleLedgerAppend(f.callerCtx(), newCallToolReq("ledger_add", map[string]any{
		"project_id":  f.foreignProject,
		"type":        ledger.TypeDecision,
		"content":     "non-admin attempt",
		"durability":  ledger.DurabilityDurable,
		"source_type": ledger.SourceAgent,
	}))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected access-denied error for non-admin cross-workspace write; got body=%s", firstText(res))
	}
}

// TestLedgerAppend_CtxImpersonationCopiedToEntry covers AC4: an internal
// caller can stamp impersonation via ctx and Append picks it up even
// when the entry is constructed without the field.
func TestLedgerAppend_CtxImpersonationCopiedToEntry(t *testing.T) {
	t.Parallel()
	f := newImpersonationFixture(t, false)

	ctx := ledger.WithImpersonatingWorkspace(f.callerCtx(), f.foreignWS)
	written, err := f.server.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: f.foreignWS,
		ProjectID:   f.foreignProject,
		Type:        ledger.TypeDecision,
		Content:     "internal call with ctx",
		Durability:  ledger.DurabilityDurable,
		SourceType:  ledger.SourceAgent,
		CreatedBy:   "u_alice",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if written.ImpersonatingAsWorkspace != f.foreignWS {
		t.Errorf("expected ctx-stamped ImpersonatingAsWorkspace=%q; got %q", f.foreignWS, written.ImpersonatingAsWorkspace)
	}
}

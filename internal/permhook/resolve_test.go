package permhook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/session"
)

// resolveFixture wires the three memory stores the resolver needs.
type resolveFixture struct {
	r      *Resolver
	ctx    context.Context
	now    time.Time
	userID string
	sessID string
	wsID   string
	docs   document.Store
	ledger ledger.Store
}

func newResolveFixture(t *testing.T) *resolveFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	docs := document.NewMemoryStore()
	led := ledger.NewMemoryStore()
	ses := session.NewMemoryStore()

	userID := "u_alice"
	sessID := "session-aaaa"
	if _, err := ses.Register(ctx, userID, sessID, session.SourceSessionStart, now); err != nil {
		t.Fatalf("register session: %v", err)
	}

	return &resolveFixture{
		r: &Resolver{
			Sessions: ses,
			Ledger:   led,
			Docs:     docs,
		},
		ctx:    ctx,
		now:    now,
		userID: userID,
		sessID: sessID,
		wsID:   "wksp_x",
		docs:   docs,
		ledger: led,
	}
}

func (f *resolveFixture) appendActionClaim(t *testing.T, agentID string, patterns []string, status string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"agent_id":          agentID,
		"permissions_claim": patterns,
		"source":            "agent_document",
	})
	row, err := f.ledger.Append(f.ctx, ledger.LedgerEntry{
		WorkspaceID: f.wsID,
		Type:        ledger.TypeActionClaim,
		Tags:        []string{"kind:action-claim"},
		Content:     "test action claim",
		Structured:  body,
		CreatedBy:   f.userID,
	}, f.now)
	if err != nil {
		t.Fatalf("append action_claim: %v", err)
	}
	if status == ledger.StatusDereferenced {
		if _, err := f.ledger.Dereference(f.ctx, row.ID, "test", f.userID, f.now, nil); err != nil {
			t.Fatalf("dereference: %v", err)
		}
	}
}

func (f *resolveFixture) appendSessionDefault(t *testing.T, agentID string, patterns []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"session_id":          f.sessID,
		"agent_id":            agentID,
		"permission_patterns": patterns,
	})
	if _, err := f.ledger.Append(f.ctx, ledger.LedgerEntry{
		WorkspaceID: f.wsID,
		Type:        ledger.TypeDecision,
		Tags:        []string{"kind:session-default-install", "session:" + f.sessID},
		Content:     "session default install",
		Structured:  body,
		CreatedBy:   f.userID,
	}, f.now); err != nil {
		t.Fatalf("append session-default: %v", err)
	}
}

// TestResolver_NoSession returns session_not_registered when the
// session id doesn't resolve.
func TestResolver_NoSession(t *testing.T) {
	t.Parallel()
	f := newResolveFixture(t)
	got := f.r.Resolve(f.ctx, "ghost-session", "Bash:git_status")
	if got.Decision != DecisionDeny || got.Reason != "session_not_registered" {
		t.Errorf("ghost session = %+v, want deny session_not_registered", got)
	}
}

// TestResolver_NoResolution denies when no claim and no session-default
// rows exist.
func TestResolver_NoResolution(t *testing.T) {
	t.Parallel()
	f := newResolveFixture(t)
	got := f.r.Resolve(f.ctx, f.sessID, "Bash:git_status")
	if got.Decision != DecisionDeny || got.Reason != "no_resolved_permissions" {
		t.Errorf("no resolution = %+v, want deny no_resolved_permissions", got)
	}
}

// TestResolver_ActiveCI sources patterns from the most recent active
// kind:action-claim row.
func TestResolver_ActiveCI(t *testing.T) {
	t.Parallel()
	f := newResolveFixture(t)
	f.appendActionClaim(t, "agent_dev", []string{"Edit:**", "Bash:go_test"}, ledger.StatusActive)

	got := f.r.Resolve(f.ctx, f.sessID, "Edit:internal/foo.go")
	if got.Decision != DecisionAllow {
		t.Errorf("active ci edit = %+v, want allow", got)
	}
	if got.Source != SourceActiveCI {
		t.Errorf("source = %q, want active_ci", got.Source)
	}
	if got.AgentID != "agent_dev" {
		t.Errorf("agent_id = %q, want agent_dev", got.AgentID)
	}

	// Tool not in the agent's patterns → deny.
	deny := f.r.Resolve(f.ctx, f.sessID, "Bash:git_push")
	if deny.Decision != DecisionDeny || deny.Source != SourceActiveCI {
		t.Errorf("active ci deny = %+v, want deny active_ci", deny)
	}
}

// TestResolver_SessionDefault falls back to the session-default-install
// row when no action-claim is active.
func TestResolver_SessionDefault(t *testing.T) {
	t.Parallel()
	f := newResolveFixture(t)
	f.appendSessionDefault(t, "agent_orch", []string{"Read:**", "mcp__satellites__satellites_*"})

	allow := f.r.Resolve(f.ctx, f.sessID, "Read:foo.go")
	if allow.Decision != DecisionAllow || allow.Source != SourceSessionDefault {
		t.Errorf("orchestrator read = %+v, want allow session_default", allow)
	}

	// Edit is NOT in orchestrator patterns → deny.
	deny := f.r.Resolve(f.ctx, f.sessID, "Edit:foo.go")
	if deny.Decision != DecisionDeny || deny.Source != SourceSessionDefault {
		t.Errorf("orchestrator edit = %+v, want deny session_default", deny)
	}
}

// TestResolver_FallbackAfterDereference verifies AC6: when the
// action-claim row is dereferenced (CI closed/amended), resolution
// falls back to session default.
func TestResolver_FallbackAfterDereference(t *testing.T) {
	t.Parallel()
	f := newResolveFixture(t)
	f.appendSessionDefault(t, "agent_orch", []string{"Read:**"})
	f.appendActionClaim(t, "agent_dev", []string{"Edit:**"}, ledger.StatusDereferenced)

	got := f.r.Resolve(f.ctx, f.sessID, "Read:foo.go")
	if got.Decision != DecisionAllow || got.Source != SourceSessionDefault {
		t.Errorf("after dereference = %+v, want allow session_default (Edit dereferenced)", got)
	}
}

// TestLookupOrchestratorAgent_OverrideChain asserts AC3: project >
// workspace > system precedence on the orchestrator agent lookup.
func TestLookupOrchestratorAgent_OverrideChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	docs := document.NewMemoryStore()

	// System seed.
	if _, err := docs.Create(ctx, document.Document{
		Type:   document.TypeAgent,
		Scope:  document.ScopeSystem,
		Status: document.StatusActive,
		Name:   "agent_claude_orchestrator",
		Body:   "system seed",
	}, now); err != nil {
		t.Fatalf("seed system agent: %v", err)
	}

	// System fallback when no override.
	d, ok := LookupOrchestratorAgent(ctx, docs, "wksp_x", "proj_x")
	if !ok || d.Name != "agent_claude_orchestrator" {
		t.Fatalf("system fallback = %+v ok=%v", d, ok)
	}

	// Project override wins.
	if _, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_x",
		ProjectID:   strPtr("proj_x"),
		Type:        document.TypeAgent,
		Scope:       document.ScopeProject,
		Status:      document.StatusActive,
		Name:        "orchestrator_role",
		Body:        "project override",
	}, now); err != nil {
		t.Fatalf("seed project agent: %v", err)
	}

	d, ok = LookupOrchestratorAgent(ctx, docs, "wksp_x", "proj_x")
	if !ok || d.Body != "project override" {
		t.Fatalf("project override = %+v ok=%v", d, ok)
	}

	// Workspace-scope agents are not permitted by the document
	// substrate (type=role only). The chain collapses to project >
	// system; passing no project_id falls through to system.
	d, ok = LookupOrchestratorAgent(ctx, docs, "wksp_x", "")
	if !ok || d.Name != "agent_claude_orchestrator" {
		t.Fatalf("no project scope = %+v, want system fallback", d)
	}
}

func strPtr(s string) *string { return &s }

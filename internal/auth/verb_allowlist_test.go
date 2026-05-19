package auth_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/auth"
)

// TestAssertVerbAllowed_ProjectScopedBypass — un-narrowed callers
// (project-scoped api-key / OAuth / session) leave AllowedVerbs nil
// and bypass the gate entirely. sty_056b68f6.
func TestAssertVerbAllowed_ProjectScopedBypass(t *testing.T) {
	t.Parallel()
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_alice", Source: "apikey:apk_xyz",
		// AllowedVerbs is nil → bypass.
	})
	if denied := auth.AssertVerbAllowed(ctx, "task_add"); denied != nil {
		t.Errorf("expected bypass for nil AllowedVerbs, got denied=%v", denied)
	}
}

// TestAssertVerbAllowed_NoCaller — when ctx has no CallerIdentity
// (e.g. unauthenticated test paths) the gate is a no-op. The auth
// middleware would have already rejected the request before this.
func TestAssertVerbAllowed_NoCaller(t *testing.T) {
	t.Parallel()
	if denied := auth.AssertVerbAllowed(context.Background(), "task_add"); denied != nil {
		t.Errorf("expected bypass without caller, got denied=%v", denied)
	}
}

// TestAssertVerbAllowed_AllowedVerbPasses — explicit task-scoped
// allowlist permits an in-set verb.
func TestAssertVerbAllowed_AllowedVerbPasses(t *testing.T) {
	t.Parallel()
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_alice", Source: "apikey:apk_xyz",
		TaskID:       "tsk_1",
		AllowedVerbs: []string{"task_update", "ledger_append"},
	})
	if denied := auth.AssertVerbAllowed(ctx, "task_update"); denied != nil {
		t.Errorf("expected pass for in-set verb, got denied=%v", denied)
	}
}

// TestAssertVerbAllowed_DeniedVerbReturnsEnvelope — out-of-set verbs
// return *VerbDeniedError whose Body() carries the wire envelope
// `{"error":"verb_not_in_allowlist","verb":"<v>","allowed":[…]}`.
// This is the AC2 wire-shape contract.
func TestAssertVerbAllowed_DeniedVerbReturnsEnvelope(t *testing.T) {
	t.Parallel()
	allowed := []string{"task_update", "ledger_append"}
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_alice", Source: "apikey:apk_xyz",
		TaskID:       "tsk_1",
		AllowedVerbs: allowed,
	})
	denied := auth.AssertVerbAllowed(ctx, "task_add")
	if denied == nil {
		t.Fatal("expected denial, got nil")
	}
	if denied.Error() != "verb_not_in_allowlist" {
		t.Errorf("Error() = %q, want verb_not_in_allowlist", denied.Error())
	}
	// Decode the wire envelope and assert the three required keys.
	var env map[string]any
	if err := json.Unmarshal([]byte(denied.Body()), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nbody=%s", err, denied.Body())
	}
	if got, _ := env["error"].(string); got != "verb_not_in_allowlist" {
		t.Errorf("envelope.error = %q, want verb_not_in_allowlist", got)
	}
	if got, _ := env["verb"].(string); got != "task_add" {
		t.Errorf("envelope.verb = %q, want task_add", got)
	}
	listRaw, ok := env["allowed"].([]any)
	if !ok {
		t.Fatalf("envelope.allowed missing or wrong type: %T (%s)", env["allowed"], denied.Body())
	}
	got := make([]string, 0, len(listRaw))
	for _, v := range listRaw {
		s, _ := v.(string)
		got = append(got, s)
	}
	if strings.Join(got, ",") != strings.Join(allowed, ",") {
		t.Errorf("envelope.allowed = %v, want %v", got, allowed)
	}
}

// TestAssertVerbAllowed_EmptyAllowlistDeniesAll — an explicit empty
// AllowedVerbs slice (non-nil but len=0) means "no verbs allowed";
// every verb is denied. This is distinct from the nil bypass.
func TestAssertVerbAllowed_EmptyAllowlistDeniesAll(t *testing.T) {
	t.Parallel()
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: "u_alice", Source: "apikey:apk_xyz",
		TaskID:       "tsk_1",
		AllowedVerbs: []string{},
	})
	denied := auth.AssertVerbAllowed(ctx, "task_update")
	if denied == nil {
		t.Fatal("expected denial for empty allowlist, got nil")
	}
	// Body must still emit `allowed: []` (not null).
	if !strings.Contains(denied.Body(), `"allowed":[]`) {
		t.Errorf("body missing empty allowed[]: %s", denied.Body())
	}
}

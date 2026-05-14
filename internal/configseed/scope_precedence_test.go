package configseed

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// contains is a thin alias for strings.Contains scoped to this test
// file so the per-test prose stays close to the assertion.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// regexCompileStoryID returns the canonical satellites story-id
// pattern used by the system-tier non-drift sweep (AC20).
func regexCompileStoryID(t *testing.T) *regexp.Regexp {
	t.Helper()
	return regexp.MustCompile(`sty_[a-f0-9]{8}`)
}

// scope_precedence_test asserts the workspace-scope override wins
// over the system-tier doc with the same name. Regression for the
// precedence model the de-projected system orchestrator + workflow
// rely on: a workspace that DOES author an override needs the
// substrate's name resolution to return the workspace body, not the
// system body. The system-tier docs in this repo are the universal
// default; workspaces only override when their flow actually differs.

// TestScopePrecedence_WorkspaceOverridesSystem_Agent: seed both a
// system-tier and a workspace-tier `claude_orchestrator` agent with
// distinct body markers; resolve by name through a workspace caller's
// memberships and assert the workspace body is returned.
func TestScopePrecedence_WorkspaceOverridesSystem_Agent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const wsID = "wksp_precedence"

	if err := os.MkdirAll(filepath.Join(dir, "system", "agents"), 0o755); err != nil {
		t.Fatalf("mkdir system agents: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, wsID, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir workspace agents: %v", err)
	}

	const systemAgent = `---
name: claude_orchestrator
tags: [v4, system]
---
SYSTEM_DEFAULT_BODY_MARKER
`
	const workspaceAgent = `---
name: claude_orchestrator
tags: [v4, workspace, override]
---
WORKSPACE_OVERRIDE_BODY_MARKER
`
	if err := os.WriteFile(filepath.Join(dir, "system", "agents", "claude_orchestrator.md"), []byte(systemAgent), 0o644); err != nil {
		t.Fatalf("write system agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsID, "agents", "claude_orchestrator.md"), []byte(workspaceAgent), 0o644); err != nil {
		t.Fatalf("write workspace agent: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	if _, err := Run(ctx, docs, dir, "system", "system", now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := RunWorkspace(ctx, docs, dir, wsID, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}

	resolved, err := docs.ResolveByName(ctx, document.TypeAgent, "claude_orchestrator", wsID, "", []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName: %v", err)
	}
	if resolved.Scope != document.ScopeWorkspace {
		t.Errorf("resolved scope = %q, want %q (workspace must win over system)", resolved.Scope, document.ScopeWorkspace)
	}
	if !contains(resolved.Body, "WORKSPACE_OVERRIDE_BODY_MARKER") {
		t.Errorf("resolved body does not carry workspace marker: %q", resolved.Body)
	}
	if contains(resolved.Body, "SYSTEM_DEFAULT_BODY_MARKER") {
		t.Errorf("resolved body still carries system marker — workspace override did not win: %q", resolved.Body)
	}
}

// TestScopePrecedence_WorkspaceOverridesSystem_Workflow asserts the
// same precedence rule for workflow docs: a workspace-scope `default`
// workflow wins over the system-tier `default` workflow.
func TestScopePrecedence_WorkspaceOverridesSystem_Workflow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const wsID = "wksp_precedence_wf"

	if err := os.MkdirAll(filepath.Join(dir, "system", "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir system workflows: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, wsID, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir workspace workflows: %v", err)
	}

	const systemWorkflow = `---
name: default
tags: [v4, system]
---
SYSTEM_DEFAULT_WORKFLOW_MARKER
`
	const workspaceWorkflow = `---
name: default
tags: [v4, workspace, override]
---
WORKSPACE_OVERRIDE_WORKFLOW_MARKER
`
	if err := os.WriteFile(filepath.Join(dir, "system", "workflows", "default.md"), []byte(systemWorkflow), 0o644); err != nil {
		t.Fatalf("write system workflow: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsID, "workflows", "default.md"), []byte(workspaceWorkflow), 0o644); err != nil {
		t.Fatalf("write workspace workflow: %v", err)
	}

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	if _, err := Run(ctx, docs, dir, "system", "system", now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := RunWorkspace(ctx, docs, dir, wsID, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}

	resolved, err := docs.ResolveByName(ctx, document.TypeWorkflow, "default", wsID, "", []string{wsID})
	if err != nil {
		t.Fatalf("ResolveByName: %v", err)
	}
	if resolved.Scope != document.ScopeWorkspace {
		t.Errorf("resolved scope = %q, want %q (workspace must win over system)", resolved.Scope, document.ScopeWorkspace)
	}
	if !contains(resolved.Body, "WORKSPACE_OVERRIDE_WORKFLOW_MARKER") {
		t.Errorf("resolved body does not carry workspace marker: %q", resolved.Body)
	}
}

// TestScopePrecedence_RealSatellitesWorkspace_AdoptsSystemDefault is
// AC10's positive regression: the real wksp_5b3257d1 seed tree must
// NOT carry workspace-scope override files for claude_orchestrator or
// the default workflow. Without overrides the substrate's name
// resolution falls back to system tier, which is exactly what
// pr_contract_separation requires for projects whose chain IS the
// canonical default.
func TestScopePrecedence_RealSatellitesWorkspace_AdoptsSystemDefault(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	overrides := []string{
		filepath.Join(seedDir, "wksp_5b3257d1", "agents", "claude_orchestrator.md"),
		filepath.Join(seedDir, "wksp_5b3257d1", "workflows", "default.md"),
	}
	for _, path := range overrides {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("workspace override should NOT exist at %s — the satellites workspace adopts the system default (AC10)", path)
		}
	}
}

// TestScopePrecedence_SystemTier_NoProjectStoryIDs is the
// non-drift assertion (AC20). System-tier `claude_orchestrator`,
// `workflows/default`, and `artifacts/default_agent_process` must not
// cite project-specific story ids. Reseed-time invariant; runs every
// CI cycle.
func TestScopePrecedence_SystemTier_NoProjectStoryIDs(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	targets := []string{
		filepath.Join(seedDir, "system", "agents", "claude_orchestrator.md"),
		filepath.Join(seedDir, "system", "workflows", "default.md"),
		filepath.Join(seedDir, "system", "artifacts", "default_agent_process.md"),
	}
	storyIDPattern := regexCompileStoryID(t)
	for _, path := range targets {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("read %s: %v", path, readErr)
			continue
		}
		matches := storyIDPattern.FindAllString(string(body), -1)
		if len(matches) > 0 {
			t.Errorf("%s contains satellites-project story ids %v — system tier must be de-projected (AC7/AC8/AC9/AC20)", path, matches)
		}
	}
}

// Package client — `substrate_audit` typed method (sty_2f0db922).
//
// Operator-invoked verb that mints a kind=work
// action=contract:substrate_audit task naming the substrate_auditor
// agent. Per pr_substrate_model the audit rubric lives in markdown
// (the contract body + agent body lifts it verbatim); this typed
// method is the thin mint-and-return surface. The dispatched agent
// runs the rubric and writes the kind:audit-report ledger row — the
// actual audit logic is NOT in Go.
//
// pr_mcp_cli_shared_path: the MCP handler and the CLI verb both
// delegate to this method.
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bobmcallan/satellites/internal/document"
)

// SubstrateAuditInput is the caller-supplied input for SubstrateAudit.
// Both fields are optional; empty values trigger the wire-layer's
// resolution-default chain (caller's bound workspace / project).
type SubstrateAuditInput struct {
	// ProjectID, when set, scopes the audit task to a specific
	// project. When empty, falls back to the workspace_id (if set)
	// or the caller's resolution chain.
	ProjectID string `json:"project_id,omitempty"`

	// WorkspaceID, when set, scopes the audit task to a specific
	// workspace. Empty defers to the caller's resolution chain.
	WorkspaceID string `json:"workspace_id,omitempty"`

	// Memberships passes the caller's workspace memberships through
	// to TaskAdd for the agent-scope-aware capability check.
	Memberships []string

	// Resolve threads the wire-layer's session + URL context into
	// the underlying TaskAdd call (active project, project's
	// workspace, default project). Same shape TaskAddResolveDeps
	// expects.
	Resolve TaskAddResolveDeps
}

// SubstrateAuditOutput is the wire payload for substrate_audit.
// scope names whether the audit was minted for system | workspace |
// project context; the dispatched agent writes the audit-report
// row tagged with this scope so multi-project ledgers stay
// filterable.
type SubstrateAuditOutput struct {
	TaskID  string `json:"task_id"`
	StoryID string `json:"story_id"`
	AgentID string `json:"agent_id"`
	Scope   string `json:"scope"`
}

// substrateAuditAgentName is the seeded substrate_auditor's
// document name. The seed pipeline plants this at boot;
// SubstrateAudit resolves it through the doc store's tier ladder.
const substrateAuditAgentName = "substrate_auditor"

// substrateAuditAction is the contract action string the
// dispatched task carries. The substrate_auditor agent declares
// `delivers: ["contract:substrate_audit"]` so the capability check
// at task_add time matches.
const substrateAuditAction = "contract:substrate_audit"

// substrateAuditPrompt is the dispatched agent's prompt body. The
// rubric itself lives in the contract + agent markdown; the prompt
// names the contract so the agent reads the rubric at run time.
const substrateAuditPrompt = "Run the substrate_audit rubric against the current substrate. Read the six rubric checks from the substrate_audit contract body, apply each, and emit one kind:audit-report ledger row carrying the structured findings + a verdict tag (verdict:audit:pass | verdict:audit:warn | verdict:audit:fail). Close own task via task_update(status=closed, outcome=success, evidence_ledger_ids=[<report_id>])."

// SubstrateAudit mints a kind=work action=contract:substrate_audit
// task naming the substrate_auditor agent. Read-only with respect to
// substrate documents — the only write is the task row + the
// substrate's task_add ledger trace. The dispatched agent owns the
// audit-report row.
func (c *Client) SubstrateAudit(ctx context.Context, caller Caller, in SubstrateAuditInput) (SubstrateAuditOutput, error) {
	if c.deps.Documents == nil {
		return SubstrateAuditOutput{}, errors.New("substrate_audit unavailable: document store not configured")
	}

	// Resolve substrate_auditor's doc id via the tier ladder.
	// System-scope agents are workspace-blind so the supplied
	// workspace_id / project_id only matter for an eventual
	// workspace-tier override (none seeded today).
	doc, err := c.deps.Documents.ResolveByName(
		ctx,
		document.TypeAgent,
		substrateAuditAgentName,
		in.WorkspaceID,
		in.ProjectID,
		in.Memberships,
	)
	if err != nil {
		return SubstrateAuditOutput{}, fmt.Errorf("substrate_audit: substrate_auditor agent not seeded: %w", err)
	}

	scope := resolveSubstrateAuditScope(doc.Scope, in.WorkspaceID, in.ProjectID)

	out, err := c.TaskAdd(ctx, caller, TaskAddInput{
		AgentID:     doc.ID,
		Prompt:      substrateAuditPrompt,
		Kind:        "work",
		Action:      substrateAuditAction,
		Memberships: in.Memberships,
		Resolve:     in.Resolve,
	})
	if err != nil {
		return SubstrateAuditOutput{}, err
	}

	return SubstrateAuditOutput{
		TaskID:  out.TaskID,
		StoryID: out.StoryID,
		AgentID: doc.ID,
		Scope:   scope,
	}, nil
}

// resolveSubstrateAuditScope stamps the scope label the dispatched
// audit-report row carries. Precedence: explicit project_id →
// explicit workspace_id → "system".
func resolveSubstrateAuditScope(agentScope, workspaceID, projectID string) string {
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		return "project:" + projectID
	}
	if workspaceID = strings.TrimSpace(workspaceID); workspaceID != "" {
		return "workspace:" + workspaceID
	}
	return "system"
}

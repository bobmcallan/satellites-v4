// Package client — system + project seed-run verbs (sty_f3f7bf9b slice 10).
//
// Slice 10 of sty_f3f7bf9b lifted the seed-run business logic
// (configseed.RunAll + configseed.RunProject orchestration, ledger
// audit-row emission, workspace lookup) out of
// mcpserver/system_seed_handler.go + mcpserver/project_seed_handler.go
// into this file. The mcpserver adapters now resolve the caller,
// run the admin gate, call the typed surface, and re-pack into the
// wire-shape SystemSeedRunResult / ProjectSeedRunResult struct —
// byte-identical to the pre-extraction wire shape.
//
// The wire-shape result structs live in mcpserver (the tests reference
// them as wire-format types). The typed surface defines its own
// SystemSeedOutput / ProjectSeedOutput with the same field shape;
// the adapter copies field-for-field. Per pr_no_unrequested_compat
// we do not introduce a re-export alias — the two layers each own
// their wire-vs-typed concern.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// SystemSeedInput captures the system_seed_run request shape. Now
// overrides the timestamp written on the audit row + the StartedAt
// field; zero falls back to time.Now().UTC(). Actor falls back to
// caller.UserID when empty.
type SystemSeedInput struct {
	Actor string
	Now   time.Time
}

// SystemSeedOutput is the typed-surface mirror of the wire-shape
// SystemSeedRunResult emitted by handleSystemSeedRun. JSON tags
// match the wire shape verbatim so the ledger audit-row payload —
// emitted from inside the typed method — is byte-identical to the
// pre-extraction shape.
type SystemSeedOutput struct {
	Loaded    int                     `json:"loaded"`
	Created   int                     `json:"created"`
	Updated   int                     `json:"updated"`
	Skipped   int                     `json:"skipped"`
	Errors    []configseed.ErrorEntry `json:"errors,omitempty"`
	LedgerID  string                  `json:"ledger_id,omitempty"`
	StartedAt time.Time               `json:"started_at"`
}

// SystemSeedRun re-invokes configseed.RunAll and records the outcome
// on the ledger. Admin-gated: non-admin callers receive a forbidden
// error matching the prior wire-shape behaviour. Mirrors the
// pre-extraction logic in *Server.RunSystemSeed verbatim.
func (c *Client) SystemSeedRun(ctx context.Context, caller Caller, in SystemSeedInput) (SystemSeedOutput, error) {
	if !caller.GlobalAdmin {
		return SystemSeedOutput{}, errors.New("forbidden")
	}
	return c.runSystemSeed(ctx, callerActor(caller, in.Actor), nowOrTime(in.Now))
}

// RunSystemSeed is the un-gated internal entry point used by the
// boot goroutine in cmd/satellites-server. Skips the admin check so
// the system actor can seed without a Caller envelope. Returns the
// same typed output.
func (c *Client) RunSystemSeed(ctx context.Context, actor string) (SystemSeedOutput, error) {
	return c.runSystemSeed(ctx, actor, time.Now().UTC())
}

// runSystemSeed is the shared substrate logic. Resolves the system
// workspace, runs configseed.RunAll, appends a kind:system-seed-run
// ledger row. Errors during the configseed pass propagate; ledger
// failure is swallowed (the seed itself succeeded) — same as the
// pre-extraction shape.
func (c *Client) runSystemSeed(ctx context.Context, actor string, now time.Time) (SystemSeedOutput, error) {
	workspaceID := ""
	if c.deps.Workspaces != nil {
		if list, err := c.deps.Workspaces.ListByMember(ctx, "system"); err == nil && len(list) > 0 {
			workspaceID = list[0].ID
		}
	}
	summary, err := configseed.RunAll(ctx,
		c.deps.Documents,
		configseed.ResolveSeedDir(),
		configseed.ResolveHelpDir(),
		workspaceID, actor, now)
	out := SystemSeedOutput{
		Loaded:    summary.Loaded,
		Created:   summary.Created,
		Updated:   summary.Updated,
		Skipped:   summary.Skipped,
		Errors:    summary.Errors,
		StartedAt: now,
	}
	if err != nil {
		return out, err
	}
	if c.deps.Ledger != nil {
		structured, _ := json.Marshal(out)
		row := ledger.LedgerEntry{
			WorkspaceID: workspaceID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:system-seed-run"},
			Content:     "system seed run",
			Structured:  structured,
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			CreatedBy:   actor,
		}
		if written, lerr := c.deps.Ledger.Append(ctx, row, now); lerr == nil {
			out.LedgerID = written.ID
		}
	}
	return out, nil
}

// ProjectSeedInput captures the project_seed_run request shape.
type ProjectSeedInput struct {
	ProjectID string
	Actor     string
	Now       time.Time
}

// ProjectSeedOutput is the typed-surface mirror of the wire-shape
// ProjectSeedRunResult emitted by handleProjectSeedRun. JSON tags
// match the wire shape verbatim so the ledger audit-row payload is
// byte-identical to the pre-extraction shape.
type ProjectSeedOutput struct {
	ProjectID string                  `json:"project_id"`
	Loaded    int                     `json:"loaded"`
	Created   int                     `json:"created"`
	Updated   int                     `json:"updated"`
	Skipped   int                     `json:"skipped"`
	Errors    []configseed.ErrorEntry `json:"errors,omitempty"`
	LedgerID  string                  `json:"ledger_id,omitempty"`
	StartedAt time.Time               `json:"started_at"`
}

// ProjectSeedRun runs configseed.RunProject for one project, appending
// a kind:project-seed-run ledger row attached to that project.
// Admin-gated: non-admin callers receive a forbidden error.
func (c *Client) ProjectSeedRun(ctx context.Context, caller Caller, in ProjectSeedInput) (ProjectSeedOutput, error) {
	if !caller.GlobalAdmin {
		return ProjectSeedOutput{}, errors.New("forbidden")
	}
	if in.ProjectID == "" {
		return ProjectSeedOutput{}, errors.New("project_id required")
	}
	return c.runProjectSeed(ctx, in.ProjectID, callerActor(caller, in.Actor), nowOrTime(in.Now))
}

// RunProjectSeed is the un-gated internal entry point used by the
// boot goroutine in cmd/satellites-server. Skips the admin check.
func (c *Client) RunProjectSeed(ctx context.Context, projectID, actor string) (ProjectSeedOutput, error) {
	return c.runProjectSeed(ctx, projectID, actor, time.Now().UTC())
}

// runProjectSeed is the shared substrate logic. Resolves the
// project + workspace, runs configseed.RunProject, appends a
// kind:project-seed-run ledger row scoped to the project.
func (c *Client) runProjectSeed(ctx context.Context, projectID, actor string, now time.Time) (ProjectSeedOutput, error) {
	out := ProjectSeedOutput{ProjectID: projectID, StartedAt: now}
	if c.deps.Projects == nil {
		return out, fmt.Errorf("project store not wired")
	}
	proj, err := c.deps.Projects.GetByID(ctx, projectID, nil)
	if err != nil {
		return out, fmt.Errorf("project not found: %w", err)
	}
	summary, err := configseed.RunProject(ctx,
		c.deps.Documents,
		configseed.ResolveSeedDir(),
		projectID, proj.WorkspaceID, actor, now)
	out.Loaded = summary.Loaded
	out.Created = summary.Created
	out.Updated = summary.Updated
	out.Skipped = summary.Skipped
	out.Errors = summary.Errors
	if err != nil {
		return out, err
	}
	if c.deps.Ledger != nil {
		structured, _ := json.Marshal(out)
		row := ledger.LedgerEntry{
			WorkspaceID: proj.WorkspaceID,
			ProjectID:   projectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:project-seed-run"},
			Content:     "project seed run",
			Structured:  structured,
			Durability:  ledger.DurabilityDurable,
			SourceType:  ledger.SourceAgent,
			CreatedBy:   actor,
		}
		if written, lerr := c.deps.Ledger.Append(ctx, row, now); lerr == nil {
			out.LedgerID = written.ID
		}
	}
	return out, nil
}

// callerActor returns the explicit actor when supplied, else the
// caller's user id. Mirrors the pre-extraction call sites that pass
// caller.UserID through to the audit row.
func callerActor(caller Caller, override string) string {
	if override != "" {
		return override
	}
	return caller.UserID
}

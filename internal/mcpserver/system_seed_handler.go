package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// SystemSeedRunResult is the JSON payload returned by the
// system_seed_run verb and recorded on the kind:system-seed-run
// ledger row. Mirrors configseed.Summary so the audit trail and the
// caller see the same shape. story_33e1a323.
type SystemSeedRunResult struct {
	Loaded    int                     `json:"loaded"`
	Created   int                     `json:"created"`
	Updated   int                     `json:"updated"`
	Skipped   int                     `json:"skipped"`
	Errors    []configseed.ErrorEntry `json:"errors,omitempty"`
	LedgerID  string                  `json:"ledger_id,omitempty"`
	StartedAt time.Time               `json:"started_at"`
}

// handleSystemSeedRun re-invokes configseed.RunAll and records the
// outcome on the ledger. Gated to global_admin: non-admins receive a
// structured "forbidden" error.
func (s *Server) handleSystemSeedRun(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	if !caller.GlobalAdmin {
		body, _ := json.Marshal(map[string]any{"error": "forbidden"})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	result, err := s.RunSystemSeed(ctx, caller.UserID)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(result)
	return mcpgo.NewToolResultText(string(body)), nil
}

// RunSystemSeed is the shared internal entry point used by both the
// MCP verb and the portal admin POST. Re-runs configseed.RunAll
// against the configured directories and writes a
// kind:system-seed-run ledger row. Returns the structured outcome.
func (s *Server) RunSystemSeed(ctx context.Context, actor string) (SystemSeedRunResult, error) {
	now := s.nowUTC()
	workspaceID := ""
	// Reuse the system workspace seeded at boot. When no workspace
	// store is wired (rare; tests sometimes elide it), the seed runs
	// with empty workspace_id — which the document store accepts for
	// system-scope rows.
	if s.workspaces != nil {
		// First admin row on the system workspace is the canonical
		// signal — match by member rather than by ID so we don't
		// hard-code a workspace identifier.
		// Falls back to empty when the system workspace isn't yet
		// resolvable (cold-boot or test fixture).
		if list, err := s.workspaces.ListByMember(ctx, "system"); err == nil && len(list) > 0 {
			workspaceID = list[0].ID
		}
	}

	summary, err := configseed.RunAll(ctx,
		s.docs,
		configseed.ResolveSeedDir(),
		configseed.ResolveHelpDir(),
		workspaceID, actor, now)
	result := SystemSeedRunResult{
		Loaded:    summary.Loaded,
		Created:   summary.Created,
		Updated:   summary.Updated,
		Skipped:   summary.Skipped,
		Errors:    summary.Errors,
		StartedAt: now,
	}
	if err != nil {
		return result, err
	}

	if s.ledger != nil {
		structured, _ := json.Marshal(result)
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
		if written, lerr := s.ledger.Append(ctx, row, now); lerr == nil {
			result.LedgerID = written.ID
		}
	}
	return result, nil
}

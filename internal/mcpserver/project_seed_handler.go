// Project-seed-run handler (sty_8868eaf4). Mirror of
// system_seed_handler.go but scoped to one project_id at a time.
// Loads <seed_dir>/<project_id>/<kind>/*.md as scope=project documents
// and writes a kind:project-seed-run ledger row attached to that
// project. Global-admin gated, same as system_seed_run.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// ProjectSeedRunResult mirrors SystemSeedRunResult shape so callers
// can treat the two seed paths uniformly. ProjectID is included so
// the JSON payload is self-describing on the ledger.
type ProjectSeedRunResult struct {
	ProjectID string                  `json:"project_id"`
	Loaded    int                     `json:"loaded"`
	Created   int                     `json:"created"`
	Updated   int                     `json:"updated"`
	Skipped   int                     `json:"skipped"`
	Errors    []configseed.ErrorEntry `json:"errors,omitempty"`
	LedgerID  string                  `json:"ledger_id,omitempty"`
	StartedAt time.Time               `json:"started_at"`
}

func (s *Server) handleProjectSeedRun(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	if !caller.GlobalAdmin {
		body, _ := json.Marshal(map[string]any{"error": "forbidden"})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	projectID, _ := req.RequireString("project_id")
	if projectID == "" {
		body, _ := json.Marshal(map[string]any{"error": "project_id required"})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	result, err := s.RunProjectSeed(ctx, projectID, caller.UserID)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(result)
	return mcpgo.NewToolResultText(string(body)), nil
}

// RunProjectSeed is the shared internal entry point used by the MCP
// verb and the boot goroutine in main. Resolves the project's
// workspace_id, runs configseed.RunProject against the configured
// seed dir, and writes a kind:project-seed-run ledger row attached to
// the project. Returns the structured outcome including any per-file
// errors — a missing project row, a malformed seed file, or an upsert
// failure all surface here without panicking the caller.
func (s *Server) RunProjectSeed(ctx context.Context, projectID, actor string) (ProjectSeedRunResult, error) {
	now := s.nowUTC()
	result := ProjectSeedRunResult{ProjectID: projectID, StartedAt: now}

	if s.projects == nil {
		return result, fmt.Errorf("project store not wired")
	}
	proj, err := s.projects.GetByID(ctx, projectID, nil)
	if err != nil {
		return result, fmt.Errorf("project not found: %w", err)
	}

	summary, err := configseed.RunProject(ctx,
		s.docs,
		configseed.ResolveSeedDir(),
		projectID, proj.WorkspaceID, actor, now)
	result.Loaded = summary.Loaded
	result.Created = summary.Created
	result.Updated = summary.Updated
	result.Skipped = summary.Skipped
	result.Errors = summary.Errors
	if err != nil {
		return result, err
	}

	if s.ledger != nil {
		structured, _ := json.Marshal(result)
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
		if written, lerr := s.ledger.Append(ctx, row, now); lerr == nil {
			result.LedgerID = written.ID
		}
	}
	return result, nil
}

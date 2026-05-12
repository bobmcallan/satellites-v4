// project_seed_run handler (sty_f3f7bf9b slice 10 adapter).
//
// Slice 10 lifted the project-seed-run business logic into
// internal/client/seed.go. This file is now the thin wire-layer
// adapter. The ProjectSeedRunResult struct stays here as the
// wire-format type (referenced by *_test.go and by the public
// *Server.RunProjectSeed API consumed at cmd/satellites-server/main.go).
package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/configseed"
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
	projectID, _ := req.RequireString("project_id")
	out, err := s.cli().ProjectSeedRun(ctx, toClientCaller(caller), client.ProjectSeedInput{ProjectID: projectID, Now: s.nowUTC()})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(projectSeedWireResult(out))
	return mcpgo.NewToolResultText(string(body)), nil
}

// RunProjectSeed preserves the public *Server.RunProjectSeed surface
// for callers that bypass the wire layer (the boot goroutine in
// cmd/satellites-server). Forwards to *client.Client.RunProjectSeed
// (un-gated) and re-packs into the wire-shape struct.
func (s *Server) RunProjectSeed(ctx context.Context, projectID, actor string) (ProjectSeedRunResult, error) {
	out, err := s.cli().RunProjectSeed(ctx, projectID, actor)
	return projectSeedWireResult(out), err
}

// projectSeedWireResult copies the typed-surface output into the
// wire-shape struct field-for-field.
func projectSeedWireResult(o client.ProjectSeedOutput) ProjectSeedRunResult {
	return ProjectSeedRunResult{
		ProjectID: o.ProjectID,
		Loaded:    o.Loaded,
		Created:   o.Created,
		Updated:   o.Updated,
		Skipped:   o.Skipped,
		Errors:    o.Errors,
		LedgerID:  o.LedgerID,
		StartedAt: o.StartedAt,
	}
}

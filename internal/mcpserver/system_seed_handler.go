// system_seed_run handler (sty_f3f7bf9b slice 10 adapter).
//
// Slice 10 lifted the seed-run business logic into
// internal/client/seed.go. This file is now the thin wire-layer
// adapter: arg parse → admin gate → typed call → wire-shape marshal.
// The SystemSeedRunResult struct stays here as the wire-format type
// (referenced by *_test.go and by the public *Server.RunSystemSeed
// API consumed at cmd/satellites-server/main.go).
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

// handleSystemSeedRun is the wire adapter: admin gate at the wire
// layer matches the pre-extraction error shape; the substrate logic
// lives in *client.Client.SystemSeedRun.
func (s *Server) handleSystemSeedRun(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().SystemSeedRun(ctx, toClientCaller(caller), client.SystemSeedInput{Now: s.nowUTC()})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(systemSeedWireResult(out))
	return mcpgo.NewToolResultText(string(body)), nil
}

// RunSystemSeed preserves the public *Server.RunSystemSeed surface
// for callers that bypass the wire layer (the boot goroutine in
// cmd/satellites-server). Forwards to *client.Client.RunSystemSeed
// (un-gated) and re-packs into the wire-shape struct.
func (s *Server) RunSystemSeed(ctx context.Context, actor string) (SystemSeedRunResult, error) {
	out, err := s.cli().RunSystemSeed(ctx, actor)
	return systemSeedWireResult(out), err
}

// systemSeedWireResult copies the typed-surface output into the
// wire-shape struct field-for-field. JSON shape is identical by
// construction (matching json tags on both sides).
func systemSeedWireResult(o client.SystemSeedOutput) SystemSeedRunResult {
	return SystemSeedRunResult{
		Loaded:    o.Loaded,
		Created:   o.Created,
		Updated:   o.Updated,
		Skipped:   o.Skipped,
		Errors:    o.Errors,
		LedgerID:  o.LedgerID,
		StartedAt: o.StartedAt,
	}
}

// Package mcpserver — `satellites_init` MCP verb (sty_796b8fe1).
//
// Thin wire-layer adapter for the typed *client.Client.SatellitesInit
// method. Decodes optional caller args (current_version, os, arch),
// invokes the typed surface, and marshals the wire payload. All
// payload-assembly logic lives on the typed surface per
// pr_mcp_cli_shared_path.
package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleSatellitesInit is the satellites_init tool adapter.
func (s *Server) handleSatellitesInit(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := req.GetArguments()
	in := client.SatellitesInitInput{
		CurrentVersion: stringArg(args, "current_version"),
		OS:             stringArg(args, "os"),
		Arch:           stringArg(args, "arch"),
	}
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().SatellitesInit(ctx, toClientCaller(caller), in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"state":               out.State,
		"target_install_path": out.TargetInstallPath,
		"target_config_path":  out.TargetConfigPath,
		"default_config":      out.DefaultConfig,
		"install":             out.Install,
		"auth_bootstrap":      out.AuthBootstrap,
		"current_version":     out.CurrentVersion,
		"fetched_at":          out.FetchedAt.Format(time.RFC3339),
	})
	return mcpgo.NewToolResultText(string(body)), nil
}

// stringArg returns the string-valued arg at key, or "" when the arg
// is missing / non-string. Mirrors the audit.go helper's contract but
// scoped to this file to avoid widening the audit package's surface.
func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

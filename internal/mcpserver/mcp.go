// Package mcpserver exposes the satellites MCP surface over Streamable HTTP.
// v4 ships a single tool (satellites_info); subsequent epics register more
// tools against the same Server.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/config"
)

// Server bundles the mcp-go MCPServer + StreamableHTTPServer with the
// satellites-specific dependencies needed by the tools.
type Server struct {
	cfg        *config.Config
	logger     arbor.ILogger
	startedAt  time.Time
	mcp        *mcpserver.MCPServer
	streamable *mcpserver.StreamableHTTPServer
}

// New constructs the MCP server with the satellites_info tool registered.
// Stateless mode is required because Fly rolling deploys move clients
// between machines (see memory note project_mcp_stateless).
func New(cfg *config.Config, logger arbor.ILogger, startedAt time.Time) *Server {
	s := &Server{cfg: cfg, logger: logger, startedAt: startedAt}

	s.mcp = mcpserver.NewMCPServer(
		"satellites",
		config.Version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions("Satellites v4 — walking skeleton. One tool: satellites_info."),
	)

	tool := mcpgo.NewTool("satellites_info",
		mcpgo.WithDescription("Return the satellites server's version metadata and the calling user's identity."),
	)
	s.mcp.AddTool(tool, s.handleInfo)

	s.streamable = mcpserver.NewStreamableHTTPServer(s.mcp,
		mcpserver.WithStateLess(true),
	)
	return s
}

// ServeHTTP implements http.Handler. AuthMiddleware is responsible for
// establishing the user context before this handler runs.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.streamable.ServeHTTP(w, r)
}

// handleInfo is the satellites_info tool implementation.
func (s *Server) handleInfo(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	var userEmail string
	if u, ok := UserFrom(ctx); ok {
		userEmail = u.Email
	}
	payload := map[string]any{
		"version":    config.Version,
		"build":      config.Build,
		"commit":     config.GitCommit,
		"user_email": userEmail,
		"started_at": s.startedAt.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "satellites_info").
		Str("user_email", userEmail).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

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
	"github.com/bobmcallan/satellites/internal/document"
)

// Server bundles the mcp-go MCPServer + StreamableHTTPServer with the
// satellites-specific dependencies needed by the tools.
type Server struct {
	cfg        *config.Config
	logger     arbor.ILogger
	startedAt  time.Time
	mcp        *mcpserver.MCPServer
	streamable *mcpserver.StreamableHTTPServer
	docs       document.Store
	docsDir    string
}

// Deps bundles the optional per-tool dependencies passed through to
// handlers. A nil DocStore disables document_ingest_file + document_get.
type Deps struct {
	DocStore document.Store
	DocsDir  string
}

// New constructs the MCP server with the satellites_info tool registered.
// Stateless mode is required because Fly rolling deploys move clients
// between machines (see memory note project_mcp_stateless).
func New(cfg *config.Config, logger arbor.ILogger, startedAt time.Time, deps Deps) *Server {
	s := &Server{
		cfg:       cfg,
		logger:    logger,
		startedAt: startedAt,
		docs:      deps.DocStore,
		docsDir:   deps.DocsDir,
	}

	s.mcp = mcpserver.NewMCPServer(
		"satellites",
		config.Version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions("Satellites v4 — walking skeleton."),
	)

	infoTool := mcpgo.NewTool("satellites_info",
		mcpgo.WithDescription("Return the satellites server's version metadata and the calling user's identity."),
	)
	s.mcp.AddTool(infoTool, s.handleInfo)

	if s.docs != nil {
		ingestTool := mcpgo.NewTool("document_ingest_file",
			mcpgo.WithDescription("Ingest a file from the server's DOCS_DIR into the document store. Path is repo-relative; server reads the file and upserts by filename."),
			mcpgo.WithString("path",
				mcpgo.Required(),
				mcpgo.Description("Repo-relative path inside DOCS_DIR."),
			),
		)
		s.mcp.AddTool(ingestTool, s.handleDocumentIngestFile)

		getTool := mcpgo.NewTool("document_get",
			mcpgo.WithDescription("Return the stored document body by filename."),
			mcpgo.WithString("filename",
				mcpgo.Required(),
				mcpgo.Description("Document filename (e.g. architecture.md)."),
			),
		)
		s.mcp.AddTool(getTool, s.handleDocumentGet)
	}

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

func (s *Server) handleDocumentIngestFile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	path, err := req.RequireString("path")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	res, err := document.IngestFile(ctx, s.docs, s.logger, s.docsDir, path, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	payload := map[string]any{
		"id":       res.Document.ID,
		"filename": res.Document.Filename,
		"version":  res.Document.Version,
		"changed":  res.Changed,
		"created":  res.Created,
	}
	body, _ := json.Marshal(payload)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_ingest_file").
		Str("filename", res.Document.Filename).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleDocumentGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	filename, err := req.RequireString("filename")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	doc, err := s.docs.GetByFilename(ctx, filename)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(doc)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "document_get").
		Str("filename", filename).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

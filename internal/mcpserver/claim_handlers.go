package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"os"
	"strconv"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/session"
)

// headerSessionID returns the session id mcp-go's Streamable HTTP
// transport attached to the request context (extracted from the
// Mcp-Session-Id header per the 2025-03-26 spec). Empty when the call
// originated from stdio, in-process tests, or a client that didn't
// echo the header. story_31975268.
func headerSessionID(ctx context.Context) string {
	sess := mcpserver.ClientSessionFromContext(ctx)
	if sess == nil {
		return ""
	}
	return sess.SessionID()
}

// resolveSessionID picks the session id for handlers that accept the
// id either on the request body (legacy / stdio path) or via the
// Mcp-Session-Id header (Streamable HTTP). The body argument wins on
// conflict so test callers can override. story_31975268.
func resolveSessionID(ctx context.Context, bodyValue string) string {
	if bodyValue != "" {
		return bodyValue
	}
	return headerSessionID(ctx)
}

// resolveSessionStaleness returns the configured claim-staleness window.
// Env SATELLITES_SESSION_STALENESS (seconds) overrides the default.
func resolveSessionStaleness() time.Duration {
	if raw := os.Getenv("SATELLITES_SESSION_STALENESS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return session.StalenessDefault
}

// handleSessionWhoami returns the caller's registered session row, or
// a structured not-registered error.
func (s *Server) handleSessionWhoami(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	sessionID := resolveSessionID(ctx, req.GetString("session_id", ""))
	out, err := s.cli().SessionWhoami(ctx, client.Caller{UserID: caller.UserID},
		client.SessionWhoamiInput{SessionID: sessionID})
	if err != nil {
		switch err {
		case client.ErrSessionIDRequired:
			body, _ := json.Marshal(map[string]any{
				"error":   "session_id_required",
				"message": "session_whoami needs a session id — supply via Mcp-Session-Id header or body session_id arg",
			})
			return mcpgo.NewToolResultError(string(body)), nil
		case client.ErrSessionNotRegistered:
			body, _ := json.Marshal(map[string]any{"error": "session_not_registered"})
			return mcpgo.NewToolResultError(string(body)), nil
		default:
			return mcpgo.NewToolResultError(err.Error()), nil
		}
	}
	// Match the legacy wire shape: ActiveProjectID was not previously
	// emitted on whoami responses; we keep that omission to preserve
	// byte-identical output.
	payload := map[string]any{
		"user_id":       out.UserID,
		"session_id":    out.SessionID,
		"source":        out.Source,
		"registered_at": out.Registered,
		"last_seen_at":  out.LastSeenAt,
	}
	if out.WorkspaceID != "" {
		payload["workspace_id"] = out.WorkspaceID
	}
	body, _ := json.Marshal(payload)
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleSessionRegister lets the SessionStart hook and API-key flows
// populate the registry. In production this is driven by the harness;
// exposing it as a verb keeps tests honest and gives callers a way to
// re-register after an unexpected restart.
func (s *Server) handleSessionRegister(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().SessionRegister(ctx, client.Caller{UserID: caller.UserID}, client.SessionRegisterInput{
		SessionID:   resolveSessionID(ctx, req.GetString("session_id", "")),
		Source:      req.GetString("source", session.SourceSessionStart),
		WorkspaceID: req.GetString("workspace_id", ""),
		ProjectID:   req.GetString("project_id", ""),
		Now:         s.nowUTC(),
		Staleness:   resolveSessionStaleness(),
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	payload := map[string]any{
		"user_id":       out.UserID,
		"session_id":    out.SessionID,
		"source":        out.Source,
		"registered_at": out.Registered,
		"last_seen_at":  out.LastSeenAt,
		"resumed":       out.Resumed,
	}
	if out.WorkspaceID != "" {
		payload["workspace_id"] = out.WorkspaceID
	}
	if out.ActiveProjectID != "" {
		payload["active_project_id"] = out.ActiveProjectID
	}
	body, _ := json.Marshal(payload)
	return mcpgo.NewToolResultText(string(body)), nil
}

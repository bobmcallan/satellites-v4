// Changelog MCP handlers — V3 parity port (sty_12af0bdc). The five
// verbs are gated at registration on s.changelog != nil; the handlers
// themselves assume the store is non-nil because the registration site
// is the only path that wires them.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/changelog"
)

// parseChangelogEffectiveDate reads `effective_date` as RFC3339 when
// present, otherwise leaves the zero value (callers default to now).
// Returns a structured error on malformed input.
func parseChangelogEffectiveDate(req mcpgo.CallToolRequest) (time.Time, error) {
	args := req.GetArguments()
	v, ok := args["effective_date"]
	if !ok {
		return time.Time{}, nil
	}
	s, _ := v.(string)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("effective_date: parse RFC3339: %w", err)
	}
	return t, nil
}

func (s *Server) handleChangelogAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	projectID, err := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	service, err := req.RequireString("service")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	content, err := req.RequireString("content")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	eff, err := parseChangelogEffectiveDate(req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	now := time.Now().UTC()
	if eff.IsZero() {
		eff = now
	}
	wsID := s.resolveProjectWorkspaceID(ctx, projectID)
	row := changelog.Changelog{
		WorkspaceID:   wsID,
		ProjectID:     projectID,
		Service:       service,
		VersionFrom:   req.GetString("version_from", ""),
		VersionTo:     req.GetString("version_to", ""),
		Content:       content,
		EffectiveDate: eff,
		CreatedBy:     caller.UserID,
	}
	out, err := s.changelog.Create(ctx, row, now)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_add").Str("id", out.ID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleChangelogGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	row, err := s.changelog.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("changelog not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, row.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("changelog not found"), nil
	}
	body, _ := json.Marshal(row)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_get").Str("id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleChangelogList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	projectID, err := s.resolveProjectID(ctx, req.GetString("project_id", ""), caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	rows, err := s.changelog.List(ctx, changelog.ListOptions{
		ProjectID: projectID,
		Service:   req.GetString("service", ""),
		Limit:     int(req.GetFloat("limit", 0)),
	}, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_list").Str("project_id", projectID).Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleChangelogUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	current, err := s.changelog.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("changelog not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, current.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("changelog not found"), nil
	}
	args := req.GetArguments()
	fields := changelog.UpdateFields{}
	if _, ok := args["version_from"]; ok {
		v := req.GetString("version_from", "")
		fields.VersionFrom = &v
	}
	if _, ok := args["version_to"]; ok {
		v := req.GetString("version_to", "")
		fields.VersionTo = &v
	}
	if _, ok := args["content"]; ok {
		v := req.GetString("content", "")
		fields.Content = &v
	}
	if _, ok := args["effective_date"]; ok {
		eff, err := parseChangelogEffectiveDate(req)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		fields.EffectiveDate = &eff
	}
	out, err := s.changelog.Update(ctx, id, fields, time.Now().UTC(), memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_update").Str("id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleChangelogDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	current, err := s.changelog.GetByID(ctx, id, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("changelog not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, current.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("changelog not found"), nil
	}
	if err := s.changelog.Delete(ctx, id, memberships); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{"id": id, "deleted": true})
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_delete").Str("id", id).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

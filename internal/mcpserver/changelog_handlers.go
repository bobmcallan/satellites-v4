// Changelog MCP handlers — slice 9 of sty_f3f7bf9b. The business
// logic for all five verbs (add/get/list/update/delete) lives on
// *client.Client under internal/client/changelog.go; the handlers
// in this file are thin parse/marshal adapters. The verbs remain
// gated at registration on s.changelog != nil.
package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

func (s *Server) handleChangelogAdd(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	eff, err := client.ParseChangelogEffectiveDate(req.GetString("effective_date", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	out, err := s.cli().ChangelogAdd(ctx, toClientCaller(caller), client.ChangelogAddInput{
		ProjectID:     req.GetString("project_id", ""),
		Service:       req.GetString("service", ""),
		VersionFrom:   req.GetString("version_from", ""),
		VersionTo:     req.GetString("version_to", ""),
		Content:       req.GetString("content", ""),
		EffectiveDate: eff,
		Memberships:   memberships,
	})
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
	memberships := s.resolveCallerMemberships(ctx, caller)
	row, err := s.cli().ChangelogGet(ctx, toClientCaller(caller), client.ChangelogGetInput{
		ID:          req.GetString("id", ""),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(row)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_get").Str("id", row.ID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleChangelogList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	rows, err := s.cli().ChangelogList(ctx, toClientCaller(caller), client.ChangelogListInput{
		ProjectID:   req.GetString("project_id", ""),
		Service:     req.GetString("service", ""),
		Limit:       int(req.GetFloat("limit", 0)),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(rows)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_list").Int("count", len(rows)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleChangelogUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	in, err := buildChangelogUpdateInput(req, s.resolveCallerMemberships(ctx, caller))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	out, err := s.cli().ChangelogUpdate(ctx, toClientCaller(caller), in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_update").Str("id", in.ID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// buildChangelogUpdateInput translates the wire's "field present" arg
// semantics into the pointer-typed client.ChangelogUpdateInput. Pure
// helper — no side effects, no Server state.
func buildChangelogUpdateInput(req mcpgo.CallToolRequest, memberships []string) (client.ChangelogUpdateInput, error) {
	in := client.ChangelogUpdateInput{ID: req.GetString("id", ""), Memberships: memberships}
	args := req.GetArguments()
	if _, ok := args["version_from"]; ok {
		v := req.GetString("version_from", "")
		in.VersionFrom = &v
	}
	if _, ok := args["version_to"]; ok {
		v := req.GetString("version_to", "")
		in.VersionTo = &v
	}
	if _, ok := args["content"]; ok {
		v := req.GetString("content", "")
		in.Content = &v
	}
	if _, ok := args["effective_date"]; ok {
		eff, err := client.ParseChangelogEffectiveDate(req.GetString("effective_date", ""))
		if err != nil {
			return client.ChangelogUpdateInput{}, err
		}
		in.EffectiveDate = &eff
	}
	return in, nil
}

func (s *Server) handleChangelogDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().ChangelogDelete(ctx, toClientCaller(caller), client.ChangelogDeleteInput{
		ID:          req.GetString("id", ""),
		Memberships: memberships,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "changelog_delete").Str("id", out.ID).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}


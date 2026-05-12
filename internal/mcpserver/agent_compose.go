// Package mcpserver — agent_compose + agent_ephemeral_summary adapters.
// Slice 6 of sty_f3f7bf9b lifted the business logic into
// internal/client/agent.go; this file now houses the thin wire-layer
// adapters that parse the MCP request, call the typed surface, and
// marshal the wire envelope.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// defaultEphemeralAgentRetentionHours mirrors the typed-client constant
// so the agent_compose tests can reference it without importing the
// client package. The authoritative value lives in
// internal/client/agent.go as client.DefaultEphemeralAgentRetentionHours.
const defaultEphemeralAgentRetentionHours = client.DefaultEphemeralAgentRetentionHours

// handleAgentCompose parses the MCP request, calls the typed
// AgentCompose surface, and marshals the wire response. Validation
// envelopes (unknown_skill_ref, unknown_permission_pattern, …) ride on
// *client.AgentComposeError so the adapter can stringify the JSON
// envelope verbatim.
func (s *Server) handleAgentCompose(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().AgentCompose(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.AgentComposeInput{
		Name: name, ProjectID: req.GetString("project_id", ""),
		SkillRefs: req.GetStringSlice("skill_refs", nil), PermissionPatterns: req.GetStringSlice("permission_patterns", nil),
		Ephemeral: req.GetBool("ephemeral", false), StoryID: req.GetString("story_id", ""),
		Reason: req.GetString("reason", ""), Memberships: memberships,
	})
	if err != nil {
		var envErr *client.AgentComposeError
		if errors.As(err, &envErr) {
			return mcpgo.NewToolResultError(envErr.Body()), nil
		}
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{"agent": out.Agent, "agent_compose_ledger_id": out.AgentComposeLedgerID, "principles_context": out.PrinciplesContext})
	s.logger.Info().Str("method", "tools/call").Str("tool", "agent_compose").Str("agent_id", out.Agent.ID).Str("story_id", req.GetString("story_id", "")).Bool("ephemeral", req.GetBool("ephemeral", false)).Int("skill_count", out.SkillRefCount).Int("perm_count", out.PermPatternCount).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleAgentEphemeralSummary parses the request and delegates to the
// typed AgentEphemeralSummary surface.
func (s *Server) handleAgentEphemeralSummary(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	projectID := req.GetString("project_id", "")
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().AgentEphemeralSummary(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.AgentEphemeralSummaryInput{ProjectID: projectID, Memberships: memberships})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	s.logger.Info().Str("method", "tools/call").Str("tool", "agent_ephemeral_summary").Str("project_id", projectID).Int("total", out.EphemeralAgentCount).Int("groups", len(out.BySkillSet)).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// archiveEphemeralAgentsForStory is the wire-layer adapter retained so
// the storystatus reconciler + the agent_compose_test suite keep their
// existing call sites. Delegates to the typed
// Client.AgentArchiveEphemeralForStory surface.
func (s *Server) archiveEphemeralAgentsForStory(ctx context.Context, storyID string, terminalAt time.Time, memberships []string) (int, error) {
	caller, _ := auth.UserFrom(ctx)
	return s.cli().AgentArchiveEphemeralForStory(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships}, client.AgentArchiveEphemeralInput{StoryID: storyID, TerminalAt: terminalAt, Memberships: memberships})
}

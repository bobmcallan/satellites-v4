// chain_handlers.go — sty_4fb2d985.
//
// Thin transport forwarders for `chain_status`, `chain_advance`,
// `chain_run`. Per pr_mcp_cli_shared_path the router logic lives on
// *client.Client; the handlers parse + envelope only. All three pass
// DispatchHook=nil — the substrate never owns the local daemon
// socket; the CLI is the only caller that wraps `postEnqueue`.

package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// handleChainStatus is the read-only chain snapshot.
func (s *Server) handleChainStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().ChainStatus(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships},
		client.ChainStatusInput{StoryID: storyID, Memberships: memberships})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleChainAdvance computes the next dispatchable task id. The MCP
// variant always returns Dispatched=false — substrate doesn't dispatch.
func (s *Server) handleChainAdvance(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().ChainAdvance(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships},
		client.ChainAdvanceInput{
			StoryID:     storyID,
			Memberships: memberships,
			// DispatchHook = nil — substrate never owns the daemon socket.
		})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleChainRun loops ChainAdvance + poll. The MCP variant is an
// observability surface — every iteration is a substrate roundtrip;
// the operator-side dispatcher uses the CLI.
func (s *Server) handleChainRun(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	pollSeconds := req.GetFloat("poll_interval_seconds", 30)
	timeoutSeconds := req.GetFloat("timeout_seconds", 0)

	caller, _ := auth.UserFrom(ctx)
	memberships := s.resolveCallerMemberships(ctx, caller)
	out, err := s.cli().ChainRun(ctx, client.Caller{UserID: caller.UserID, Email: caller.Email, Memberships: memberships},
		client.ChainRunInput{
			StoryID:      storyID,
			Memberships:  memberships,
			PollInterval: time.Duration(pollSeconds * float64(time.Second)),
			Timeout:      time.Duration(timeoutSeconds * float64(time.Second)),
			// DispatchHook = nil — substrate-side observability only.
		})
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": err.Error()})
		return mcpgo.NewToolResultError(string(body)), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

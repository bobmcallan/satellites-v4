// portal_replicate wire adapter (sty_f3f7bf9b slice 12).
//
// Slice 12 lifted the portal_replicate business logic into
// internal/client/portal.go. This file is now the thin wire-layer
// adapter: tool registration + server-field accessors (boot-time
// wiring) + a ≤25-line handler that parses args, delegates to the
// typed *client.Client.PortalReplicate, and wraps the result in the
// pre-extraction `{summary, results}` JSON envelope.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/portalreplicate"
)

// SetReplicateVocabulary installs the action-alias map the
// portal_replicate handler consults to translate caller-friendly
// action names (e.g. "tap", "go-to") into canonical types. Wired
// from main.go after configseed loads the replicate_vocabulary
// document. nil keeps the default canonical-only vocabulary.
// Sty_088f6d5c.
func (s *Server) SetReplicateVocabulary(v *portalreplicate.Vocabulary) {
	s.replicateVocab = v
}

// ReplicateVocabulary returns the currently-loaded action vocabulary.
// Exposed so the /api/v1 layer (sty_e68ce6fb) can read the same vocab
// the MCP handler uses without re-loading from the doc store.
func (s *Server) ReplicateVocabulary() *portalreplicate.Vocabulary {
	return s.replicateVocab
}

// ReplicateRunner returns the currently-installed runner override.
// Returns nil when no SetReplicateRunner has been called — callers
// fall back to portalreplicate.Run in that case.
func (s *Server) ReplicateRunner() func(ctx context.Context, opts portalreplicate.RunOptions, actions []portalreplicate.Action) ([]portalreplicate.Result, portalreplicate.Summary, error) {
	return s.replicateRunner
}

// SetReplicateRunner overrides the chromedp-driven runner with a
// custom function. Tests inject a stub that returns deterministic
// Results; production leaves it nil and the handler falls back to
// portalreplicate.Run. Sty_088f6d5c.
func (s *Server) SetReplicateRunner(fn func(ctx context.Context, opts portalreplicate.RunOptions, actions []portalreplicate.Action) ([]portalreplicate.Result, portalreplicate.Summary, error)) {
	s.replicateRunner = fn
}

// registerPortalReplicate wires the portal_replicate MCP tool. Called
// from Server.New when the dep prerequisites (stories + ledger) are
// non-nil.
func (s *Server) registerPortalReplicate() {
	tool := mcpgo.NewTool("portal_replicate",
		mcpgo.WithDescription("Drive a headless browser through a sequence of actions against a target URL and ledger the captured DOM / console / screenshot evidence onto a story. Sty_088f6d5c. Action types: navigate, wait_visible, click, dom_snapshot, console_capture, screenshot, dom_visible — plus any natural-language alias declared by the replicate_vocabulary document."),
		mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story to attach evidence to (sty_<8hex>).")),
		mcpgo.WithString("target_url", mcpgo.Required(), mcpgo.Description("Absolute base URL to navigate against. The first navigate action without a Value loads this URL.")),
		mcpgo.WithString("actions", mcpgo.Required(), mcpgo.Description("JSON array of {type, selector?, value?, timeout_ms?, label?}. Type may be a canonical action or a vocabulary alias.")),
		mcpgo.WithString("cookies", mcpgo.Description("Optional JSON array of {name, value, domain?, path?, secure?, http_only?}. Domain defaults to target_url host.")),
	)
	s.mcp.AddTool(tool, s.handlePortalReplicate)
}

// handlePortalReplicate is the wire adapter: parse JSON args, call
// the typed surface, marshal the wire-shape envelope. Substrate
// logic lives in *client.Client.PortalReplicate.
func (s *Server) handlePortalReplicate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	storyID, _ := req.RequireString("story_id")
	targetURL, _ := req.RequireString("target_url")
	actionsJSON := req.GetString("actions", "")
	if actionsJSON == "" {
		return mcpgo.NewToolResultError("actions is required (JSON array)"), nil
	}
	var actions []portalreplicate.Action
	if err := json.Unmarshal([]byte(actionsJSON), &actions); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("actions must be valid JSON: %v", err)), nil
	}
	var cookies []portalreplicate.Cookie
	if cookiesJSON := req.GetString("cookies", ""); cookiesJSON != "" {
		if err := json.Unmarshal([]byte(cookiesJSON), &cookies); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("cookies must be valid JSON: %v", err)), nil
		}
	}
	caller, _ := auth.UserFrom(ctx)
	out, err := s.cli().PortalReplicate(ctx, toClientCaller(caller), client.PortalReplicateInput{
		StoryID: storyID, TargetURL: targetURL, Actions: actions, Cookies: cookies,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

// requireReplicatePrereqs returns nil when the runner has the
// dependencies it needs (stories + ledger). Used by Server.New to
// gate tool registration.
func (s *Server) requireReplicatePrereqs() error {
	if s.stories == nil {
		return errors.New("portal_replicate: stories store unavailable")
	}
	if s.ledger == nil {
		return errors.New("portal_replicate: ledger store unavailable")
	}
	return nil
}

// LoadReplicateVocabularyFromDoc reads the configured
// replicate_vocabulary document via the typed client and stores the
// result on the Server. Forwards to *client.Client.LoadReplicateVocabularyFromDoc;
// failures fall back to the canonical-only vocabulary so the tool
// stays callable with built-in action names. Called from main.go
// after configseed.RunAll completes.
func (s *Server) LoadReplicateVocabularyFromDoc(ctx context.Context, name string) error {
	v, err := s.cli().LoadReplicateVocabularyFromDoc(ctx, name)
	s.replicateVocab = v
	return err
}

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
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

// portalReplicateRequest mirrors the JSON arguments parsed by the
// handler. The actions / cookies sub-payloads come in as JSON
// strings so the mcpgo schema stays simple.
type portalReplicateRequest struct {
	StoryID   string                   `json:"story_id"`
	TargetURL string                   `json:"target_url"`
	Actions   []portalreplicate.Action `json:"actions"`
	Cookies   []portalreplicate.Cookie `json:"cookies"`
}

func (s *Server) handlePortalReplicate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	targetURL, err := req.RequireString("target_url")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	actionsJSON := req.GetString("actions", "")
	if actionsJSON == "" {
		return mcpgo.NewToolResultError("actions is required (JSON array)"), nil
	}
	var actions []portalreplicate.Action
	if err := json.Unmarshal([]byte(actionsJSON), &actions); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("actions must be valid JSON: %v", err)), nil
	}
	if len(actions) == 0 {
		return mcpgo.NewToolResultError("actions array is empty"), nil
	}

	var cookies []portalreplicate.Cookie
	if cookiesJSON := req.GetString("cookies", ""); cookiesJSON != "" {
		if err := json.Unmarshal([]byte(cookiesJSON), &cookies); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("cookies must be valid JSON: %v", err)), nil
		}
	}

	memberships := s.resolveCallerMemberships(ctx, caller)
	st, err := s.stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	if _, err := s.resolveProjectID(ctx, st.ProjectID, caller, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}

	// Resolve vocabulary aliases → canonical types. Unknown types
	// surface as a clear pre-flight error so the runner doesn't waste
	// a chromium tab on a typo.
	resolved, err := resolveActions(actions, s.replicateVocab)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	runner := s.replicateRunner
	if runner == nil {
		runner = portalreplicate.Run
	}
	results, summary, err := runner(ctx, portalreplicate.RunOptions{
		TargetURL: targetURL,
		Cookies:   cookies,
		Headless:  true,
	}, resolved)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("replicate run failed: %v", err)), nil
	}

	// Ledger one row per action plus a summary row. Each result row
	// carries the structured Result so future tools can re-render
	// evidence; the content is a one-line human-readable summary.
	for i, r := range results {
		if appendErr := s.appendReplicateResult(ctx, st.WorkspaceID, st.ProjectID, st.ID, i, r, caller.UserID); appendErr != nil {
			s.logger.Warn().Str("story_id", st.ID).Int("action_index", i).Str("error", appendErr.Error()).Msg("ledger append for portal_replicate result failed")
		}
	}
	if appendErr := s.appendReplicateSummary(ctx, st.WorkspaceID, st.ProjectID, st.ID, summary, caller.UserID); appendErr != nil {
		s.logger.Warn().Str("story_id", st.ID).Str("error", appendErr.Error()).Msg("ledger append for portal_replicate summary failed")
	}

	body, _ := json.Marshal(map[string]any{
		"summary": summary,
		"results": results,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "portal_replicate").
		Str("story_id", st.ID).
		Str("target_url", targetURL).
		Int("actions", len(actions)).
		Int("passed", summary.Passed).
		Int("failed", summary.Failed).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// resolveActions translates each Action's Type field through the
// vocabulary, returning a new slice with canonical types. Returns
// an error naming the offending action when any type is neither a
// known canonical nor a registered alias.
func resolveActions(in []portalreplicate.Action, vocab *portalreplicate.Vocabulary) ([]portalreplicate.Action, error) {
	out := make([]portalreplicate.Action, len(in))
	for i, a := range in {
		canon := a.Type
		if !portalreplicate.IsKnownAction(canon) {
			if vocab == nil {
				return nil, fmt.Errorf("action %d: unknown type %q (no vocabulary loaded)", i, a.Type)
			}
			resolved, ok := vocab.Resolve(string(a.Type))
			if !ok || !portalreplicate.IsKnownAction(resolved) {
				return nil, fmt.Errorf("action %d: type %q is neither canonical nor a registered alias", i, a.Type)
			}
			canon = resolved
		}
		out[i] = a
		out[i].Type = canon
	}
	return out, nil
}

// appendReplicateResult ledgers a single per-action Result onto the
// story. The Result (DOM, screenshot, console) lives in the
// Structured payload; Content carries a one-line summary.
func (s *Server) appendReplicateResult(ctx context.Context, workspaceID, projectID, storyID string, index int, r portalreplicate.Result, actor string) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("[%d] %s %s — %s in %dms", index, r.Action.Type, r.Action.Selector, r.Status, r.Duration.Milliseconds())
	if r.Action.Label != "" {
		content = fmt.Sprintf("[%d] %s (%s) — %s in %dms", index, r.Action.Type, r.Action.Label, r.Status, r.Duration.Milliseconds())
	}
	if r.Status == portalreplicate.StatusFailed {
		content += " :: " + r.Error
	}
	tags := []string{"portal-replicate", "action:" + string(r.Action.Type), "status:" + string(r.Status)}
	_, err = s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeEvidence,
		Tags:        tags,
		Content:     content,
		Structured:  payload,
		CreatedBy:   actor,
	}, time.Now().UTC())
	return err
}

// appendReplicateSummary ledgers the run-level summary as a single
// row. Distinct from per-action rows so the project page (when it
// lands) can collapse a run's per-action rows under a single
// summary header.
func (s *Server) appendReplicateSummary(ctx context.Context, workspaceID, projectID, storyID string, sum portalreplicate.Summary, actor string) error {
	payload, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("portal_replicate run %s — %d/%d passed, %d failed, %d skipped in %dms against %s",
		sum.Status, sum.Passed, sum.Total, sum.Failed, sum.Skipped, sum.Duration.Milliseconds(), sum.TargetURL)
	tags := []string{"portal-replicate", "summary", "status:" + string(sum.Status)}
	_, err = s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeEvidence,
		Tags:        tags,
		Content:     content,
		Structured:  payload,
		CreatedBy:   actor,
	}, time.Now().UTC())
	return err
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
// replicate_vocabulary document via the Server's doc store and
// installs the resulting Vocabulary. Falls back to the canonical-only
// default when no document is registered. Called from main.go after
// configseed.RunAll completes.
func (s *Server) LoadReplicateVocabularyFromDoc(ctx context.Context, name string) error {
	if s.docs == nil {
		s.replicateVocab = portalreplicate.NewVocabulary()
		return nil
	}
	if strings.TrimSpace(name) == "" {
		name = "default"
	}
	doc, err := s.docs.GetByName(ctx, "", name, nil)
	if err != nil {
		s.replicateVocab = portalreplicate.NewVocabulary()
		return nil
	}
	if doc.Type != document.TypeReplicateVocabulary {
		s.replicateVocab = portalreplicate.NewVocabulary()
		return nil
	}
	v, err := portalreplicate.LoadFromDocument(doc)
	if err != nil {
		s.replicateVocab = portalreplicate.NewVocabulary()
		return err
	}
	s.replicateVocab = v
	return nil
}

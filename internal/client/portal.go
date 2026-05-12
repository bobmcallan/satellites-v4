// Package client — portal_replicate verb (sty_f3f7bf9b slice 12).
//
// Slice 12 of sty_f3f7bf9b lifted the portal_replicate business logic
// (story scope resolution, vocabulary alias expansion, runner
// dispatch, per-action + summary ledger emission) out of
// mcpserver/portal_replicate.go into this file. The mcpserver adapter
// now resolves the caller, calls the typed surface, and re-packs the
// wire-shape `{summary, results}` JSON envelope — byte-identical to
// the pre-extraction shape.
//
// The vocabulary + runner accessors stay on *Server (boot-time wiring
// path) but the typed method consumes them via Deps.ReplicateVocab /
// Deps.ReplicateRunner. Server.cli() threads both fields through to
// the Client, so MCP and /api/v1 share one substrate path.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/portalreplicate"
)

// PortalReplicateInput captures the parsed arguments of the
// portal_replicate verb.
//
// The typed Actions/Cookies fields are populated when the wire layer
// has already-decoded structs (the HTTP /api/v1 path uses its own JSON
// body decoder). The ActionsJSON/CookiesJSON fields let the MCP wire
// adapter pass the raw request strings through without importing
// internal/portalreplicate — the client owns the JSON shape. When both
// typed and JSON forms are set, the typed slices win; when only the
// JSON form is set, this method unmarshals it before substrate work.
// Sty_4db0e025 slice A8.
type PortalReplicateInput struct {
	StoryID      string
	TargetURL    string
	Actions      []portalreplicate.Action
	Cookies      []portalreplicate.Cookie
	ActionsJSON  string
	CookiesJSON  string
	// Now overrides the per-row ledger timestamp; zero falls back to
	// time.Now().UTC(). Tests inject a fixture time for determinism.
	Now time.Time
}

// PortalReplicateOutput is the typed mirror of the wire shape emitted
// by the portal_replicate verb. JSON tags match the pre-extraction
// shape verbatim so callers that round-trip the payload see no drift.
type PortalReplicateOutput struct {
	Summary portalreplicate.Summary  `json:"summary"`
	Results []portalreplicate.Result `json:"results"`
}

// PortalReplicate drives a headless browser through Input.Actions
// against Input.TargetURL, ledgering each per-action Result + a
// summary row onto the supplied story. Mirrors the pre-extraction
// substrate logic from mcpserver.handlePortalReplicate verbatim.
//
// Errors map to the pre-extraction wire shape:
//   - story not found / project mismatch → "story not found"
//   - unknown action type / vocabulary miss → resolveActions error text
//   - runner failure → "replicate run failed: <err>"
//
// Ledger append failures for individual rows are swallowed (logged at
// the wire layer when a logger is wired); the run's wire payload
// returns whatever the runner produced.
func (c *Client) PortalReplicate(ctx context.Context, caller Caller, in PortalReplicateInput) (PortalReplicateOutput, error) {
	if c.deps.Stories == nil || c.deps.Ledger == nil {
		return PortalReplicateOutput{}, errors.New("portal_replicate unavailable: stores not configured")
	}
	if in.StoryID == "" {
		return PortalReplicateOutput{}, errors.New("story_id is required")
	}
	if in.TargetURL == "" {
		return PortalReplicateOutput{}, errors.New("target_url is required")
	}
	// MCP wire-layer path: caller passes raw JSON strings via
	// ActionsJSON/CookiesJSON so internal/portalreplicate stays out of
	// internal/mcpserver/portal_replicate.go. Typed slices, when set,
	// take precedence (HTTP /api/v1 + tests use that path). Empty
	// ActionsJSON with empty Actions falls through to the actions-empty
	// guard below. Sty_4db0e025 slice A8.
	if len(in.Actions) == 0 && in.ActionsJSON != "" {
		if err := json.Unmarshal([]byte(in.ActionsJSON), &in.Actions); err != nil {
			return PortalReplicateOutput{}, fmt.Errorf("actions must be valid JSON: %w", err)
		}
	}
	if len(in.Cookies) == 0 && in.CookiesJSON != "" {
		if err := json.Unmarshal([]byte(in.CookiesJSON), &in.Cookies); err != nil {
			return PortalReplicateOutput{}, fmt.Errorf("cookies must be valid JSON: %w", err)
		}
	}
	if len(in.Actions) == 0 {
		return PortalReplicateOutput{}, errors.New("actions array is empty")
	}

	memberships := caller.Memberships
	if memberships == nil {
		memberships = c.ResolveCallerMemberships(ctx, caller)
	}
	st, err := c.deps.Stories.GetByID(ctx, in.StoryID, memberships)
	if err != nil {
		return PortalReplicateOutput{}, errors.New("story not found")
	}
	if _, err := c.ResolveProjectID(ctx, st.ProjectID, "", caller, memberships); err != nil {
		return PortalReplicateOutput{}, errors.New("story not found")
	}

	resolved, err := resolvePortalActions(in.Actions, c.deps.ReplicateVocab)
	if err != nil {
		return PortalReplicateOutput{}, err
	}

	runner := c.deps.ReplicateRunner
	if runner == nil {
		runner = portalreplicate.Run
	}
	results, summary, err := runner(ctx, portalreplicate.RunOptions{
		TargetURL: in.TargetURL,
		Cookies:   in.Cookies,
		Headless:  true,
	}, resolved)
	if err != nil {
		return PortalReplicateOutput{}, fmt.Errorf("replicate run failed: %w", err)
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for i, r := range results {
		if appendErr := c.appendReplicateResult(ctx, st.WorkspaceID, st.ProjectID, st.ID, i, r, caller.UserID, now); appendErr != nil && c.deps.Logger != nil {
			c.deps.Logger.Warn().Str("story_id", st.ID).Int("action_index", i).Str("error", appendErr.Error()).Msg("ledger append for portal_replicate result failed")
		}
	}
	if appendErr := c.appendReplicateSummary(ctx, st.WorkspaceID, st.ProjectID, st.ID, summary, caller.UserID, now); appendErr != nil && c.deps.Logger != nil {
		c.deps.Logger.Warn().Str("story_id", st.ID).Str("error", appendErr.Error()).Msg("ledger append for portal_replicate summary failed")
	}

	return PortalReplicateOutput{Summary: summary, Results: results}, nil
}

// ReplicateVocab returns the vocabulary the Client is currently
// configured with. The wire layer (mcpserver.Server, main.go) reads
// this after boot-time vocabulary loading so the /api/v1 portal client
// can share the same vocab the MCP verb consumes.
func (c *Client) ReplicateVocab() *portalreplicate.Vocabulary {
	return c.deps.ReplicateVocab
}

// NewReplicateVocabDefault returns a canonical-only Vocabulary. Used
// by the wire layer at boot to install a fallback vocab before
// LoadReplicateVocabularyFromDoc swaps in the configseed-loaded alias
// map. Mirrors portalreplicate.NewVocabulary; the type lives on the
// client so the wire layer can call this without importing
// internal/portalreplicate directly.
func (c *Client) NewReplicateVocabDefault() *portalreplicate.Vocabulary {
	return portalreplicate.NewVocabulary()
}

// SetReplicateVocab installs a new vocabulary on the Client's Deps.
// Boot-time wiring path (sty_088f6d5c) — called from main.go after
// configseed loads the replicate_vocabulary document. Tests that need
// a deterministic vocabulary call this directly.
func (c *Client) SetReplicateVocab(v *portalreplicate.Vocabulary) {
	c.deps.ReplicateVocab = v
}

// ReplicateRunner returns the currently-installed runner override.
// Nil means callers fall back to portalreplicate.Run (the default
// chromedp-driven runner). Sty_088f6d5c.
func (c *Client) ReplicateRunner() func(ctx context.Context, opts portalreplicate.RunOptions, actions []portalreplicate.Action) ([]portalreplicate.Result, portalreplicate.Summary, error) {
	return c.deps.ReplicateRunner
}

// SetReplicateRunner overrides the chromedp-driven runner with a
// custom function. Tests inject a stub that returns deterministic
// Results.
func (c *Client) SetReplicateRunner(fn func(ctx context.Context, opts portalreplicate.RunOptions, actions []portalreplicate.Action) ([]portalreplicate.Result, portalreplicate.Summary, error)) {
	c.deps.ReplicateRunner = fn
}

// LoadReplicateVocabularyFromDoc reads the configured
// replicate_vocabulary document via the Client's doc store and
// returns the resulting Vocabulary. Falls back to the canonical-only
// default when no document is registered or its type doesn't match.
// Called by the wire layer (mcpserver.Server) at boot once configseed
// has loaded the seed file.
func (c *Client) LoadReplicateVocabularyFromDoc(ctx context.Context, name string) (*portalreplicate.Vocabulary, error) {
	if c.deps.Documents == nil {
		return portalreplicate.NewVocabulary(), nil
	}
	if name == "" {
		name = "default"
	}
	doc, err := c.deps.Documents.GetByName(ctx, "", name, nil)
	if err != nil {
		return portalreplicate.NewVocabulary(), nil
	}
	if doc.Type != document.TypeReplicateVocabulary {
		return portalreplicate.NewVocabulary(), nil
	}
	v, err := portalreplicate.LoadFromDocument(doc)
	if err != nil {
		return portalreplicate.NewVocabulary(), err
	}
	return v, nil
}

// resolvePortalActions translates each Action's Type field through the
// vocabulary, returning a new slice with canonical types. Errors when
// any type is neither a known canonical nor a registered alias.
func resolvePortalActions(in []portalreplicate.Action, vocab *portalreplicate.Vocabulary) ([]portalreplicate.Action, error) {
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
func (c *Client) appendReplicateResult(ctx context.Context, workspaceID, projectID, storyID string, index int, r portalreplicate.Result, actor string, now time.Time) error {
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
	_, err = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeEvidence,
		Tags:        tags,
		Content:     content,
		Structured:  payload,
		CreatedBy:   actor,
	}, now)
	return err
}

// appendReplicateSummary ledgers the run-level summary as a single
// row. Distinct from per-action rows so the project page can collapse
// a run's per-action rows under a single summary header.
func (c *Client) appendReplicateSummary(ctx context.Context, workspaceID, projectID, storyID string, sum portalreplicate.Summary, actor string, now time.Time) error {
	payload, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("portal_replicate run %s — %d/%d passed, %d failed, %d skipped in %dms against %s",
		sum.Status, sum.Passed, sum.Total, sum.Failed, sum.Skipped, sum.Duration.Milliseconds(), sum.TargetURL)
	tags := []string{"portal-replicate", "summary", "status:" + string(sum.Status)}
	_, err = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeEvidence,
		Tags:        tags,
		Content:     content,
		Structured:  payload,
		CreatedBy:   actor,
	}, now)
	return err
}

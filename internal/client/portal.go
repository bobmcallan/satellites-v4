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
// portal_replicate verb. Actions/Cookies arrive already-decoded (the
// wire layer is responsible for JSON unmarshalling the request).
type PortalReplicateInput struct {
	StoryID   string
	TargetURL string
	Actions   []portalreplicate.Action
	Cookies   []portalreplicate.Cookie
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

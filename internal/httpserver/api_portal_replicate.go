// api_portal_replicate.go — /api/v1/portal/replicate handler shipped
// under sty_e68ce6fb. Mirrors mcpserver.handlePortalReplicate; the
// vocabulary + runner are read from client.Deps (main.go threads them
// from the mcp server's loaded state so both wire surfaces resolve
// aliases identically).

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/portalreplicate"
)

func (a *APIRegistrar) handlePortalReplicate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StoryID   string                   `json:"story_id"`
		TargetURL string                   `json:"target_url"`
		Actions   []portalreplicate.Action `json:"actions"`
		Cookies   []portalreplicate.Cookie `json:"cookies"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.StoryID == "" {
		writeAPIStatus(w, http.StatusBadRequest, "story_id is required")
		return
	}
	if req.TargetURL == "" {
		writeAPIStatus(w, http.StatusBadRequest, "target_url is required")
		return
	}
	if len(req.Actions) == 0 {
		writeAPIStatus(w, http.StatusBadRequest, "actions array is empty")
		return
	}
	cc := a.clientCaller(r)
	cc.Memberships = a.client.ResolveCallerMemberships(r.Context(), cc)
	stores := a.client.Stores()
	if stores.Stories == nil || stores.Ledger == nil {
		writeAPIError(w, errors.New("portal_replicate unavailable: stores not configured"))
		return
	}
	st, err := stores.Stories.GetByID(r.Context(), req.StoryID, cc.Memberships)
	if err != nil {
		writeAPIStatus(w, http.StatusNotFound, "story not found")
		return
	}
	if _, err := a.client.ResolveProjectID(r.Context(), st.ProjectID, "", cc, cc.Memberships); err != nil {
		writeAPIStatus(w, http.StatusNotFound, "story not found")
		return
	}

	vocab := stores.ReplicateVocab
	if vocab == nil {
		vocab = portalreplicate.NewVocabulary()
	}
	resolved, err := resolvePortalActions(req.Actions, vocab)
	if err != nil {
		writeAPIStatus(w, http.StatusBadRequest, err.Error())
		return
	}

	runner := stores.ReplicateRunner
	if runner == nil {
		runner = portalreplicate.Run
	}
	results, summary, err := runner(r.Context(), portalreplicate.RunOptions{
		TargetURL: req.TargetURL,
		Cookies:   req.Cookies,
		Headless:  true,
	}, resolved)
	if err != nil {
		writeAPIError(w, fmt.Errorf("replicate run failed: %w", err))
		return
	}

	now := time.Now().UTC()
	for i, res := range results {
		_ = a.appendReplicateResultRow(r.Context(), st.WorkspaceID, st.ProjectID, st.ID, i, res, cc.UserID, now)
	}
	_ = a.appendReplicateSummaryRow(r.Context(), st.WorkspaceID, st.ProjectID, st.ID, summary, cc.UserID, now)

	writeAPIJSON(w, map[string]any{
		"summary": summary,
		"results": results,
	})
}

// resolvePortalActions mirrors mcpserver.resolveActions — translates
// each action's Type through the vocabulary, returning a new slice
// with canonical types. Errors when any type is neither a known
// canonical nor a registered alias.
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

func (a *APIRegistrar) appendReplicateResultRow(ctx context.Context, workspaceID, projectID, storyID string, index int, r portalreplicate.Result, actor string, now time.Time) error {
	stores := a.client.Stores()
	payload, _ := json.Marshal(r)
	content := fmt.Sprintf("[%d] %s %s — %s in %dms", index, r.Action.Type, r.Action.Selector, r.Status, r.Duration.Milliseconds())
	if r.Action.Label != "" {
		content = fmt.Sprintf("[%d] %s (%s) — %s in %dms", index, r.Action.Type, r.Action.Label, r.Status, r.Duration.Milliseconds())
	}
	if r.Status == portalreplicate.StatusFailed {
		content += " :: " + r.Error
	}
	tags := []string{"portal-replicate", "action:" + string(r.Action.Type), "status:" + string(r.Status)}
	_, err := stores.Ledger.Append(ctx, ledger.LedgerEntry{
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

func (a *APIRegistrar) appendReplicateSummaryRow(ctx context.Context, workspaceID, projectID, storyID string, sum portalreplicate.Summary, actor string, now time.Time) error {
	stores := a.client.Stores()
	payload, _ := json.Marshal(sum)
	content := fmt.Sprintf("portal_replicate run %s — %d/%d passed, %d failed, %d skipped in %dms against %s",
		sum.Status, sum.Passed, sum.Total, sum.Failed, sum.Skipped, sum.Duration.Milliseconds(), sum.TargetURL)
	tags := []string{"portal-replicate", "summary", "status:" + string(sum.Status)}
	_, err := stores.Ledger.Append(ctx, ledger.LedgerEntry{
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

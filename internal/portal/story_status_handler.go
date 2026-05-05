package portal

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/story"
)

// storyStatusRequest is the POST body for /api/stories/{id}/status.
// The portal accepts only the target status; status flips are free
// (V3 parity, sty_01f75142). The substrate's canonical
// kind:story.status_change ledger row written inside
// stores.UpdateStatus is the sole audit row.
type storyStatusRequest struct {
	Status string `json:"status"`
}

// handleStoryStatusUpdate is the POST endpoint that powers the per-row
// + bulk operator-status-override affordances on the stories panel.
// Authorisation: caller must own the story's project. Cross-owner
// returns 404 (mirroring handleProjectDetail's leak-prevention).
// sty_1d6751e9 / sty_01f75142.
func (p *Portal) handleStoryStatusUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.stories == nil {
		http.NotFound(w, r)
		return
	}
	storyID := r.PathValue("id")
	if storyID == "" {
		http.Error(w, "story id required", http.StatusBadRequest)
		return
	}
	var req storyStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	target := strings.TrimSpace(req.Status)
	if target == "" {
		http.Error(w, "status required", http.StatusBadRequest)
		return
	}

	_, _, memberships := p.activeWorkspace(r, user)
	existing, err := p.stories.GetByID(r.Context(), storyID, memberships)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Cross-owner protection: if the story's project is not owned by the
	// caller, hide its existence behind a 404. Mirrors handleProjectDetail.
	if p.projects != nil && existing.ProjectID != "" {
		proj, perr := p.projects.GetByID(r.Context(), existing.ProjectID, memberships)
		if perr != nil || proj.OwnerUserID != user.ID {
			http.NotFound(w, r)
			return
		}
	}

	now := time.Now().UTC()
	updated, err := p.stories.UpdateStatus(r.Context(), storyID, target, user.ID, now, memberships)
	if err != nil {
		// sty_0233fabd: when the open-tasks gate fires, surface the
		// list of task ids in the JSON body so the operator can
		// reconcile without reading server logs.
		var openErr *story.StoryHasOpenTasksError
		if errors.As(err, &openErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":         err.Error(),
				"open_task_ids": openErr.OpenTaskIDs,
			})
			return
		}
		// 422 keeps the operator's UI state intact while signalling that
		// the substrate rejected the transition (illegal jump, terminal
		// re-target, etc.).
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"id":     updated.ID,
		"status": updated.Status,
	})
}

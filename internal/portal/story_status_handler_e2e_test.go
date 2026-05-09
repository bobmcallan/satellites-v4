// sty_de9f10f9 — POST /api/stories/{id}/status end-to-end coverage:
// HTTP status + ledger row + store mutation, on both the happy and
// sad paths. Pre-cutover this also asserted the WS event publication
// via a hubemit.Publisher recorder; sty_010a0543 deleted that surface
// (surreallive picks up the row mutation directly), so the WS-emit
// axis lives in the wshandler tests instead.
package portal

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
)

// TestStoryStatusUpdate_E2E_HappyPath asserts the three axes that
// fire together when the substrate accepts the transition:
//
//	(a) HTTP 200
//	(b) one new kind:story.status_change ledger row (substrate-canonical)
//	(c) the store row's Status reflects the requested target
func TestStoryStatusUpdate_E2E_HappyPath(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, ledgerStore, stories := newTestPortal(t, &config.Config{Env: "dev"})

	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "happy-path", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	baselineRows, _ := ledgerStore.List(ctx, proj.ID, ledger.ListOptions{StoryID: st.ID, Tags: []string{"kind:story.status_change"}}, nil)

	httpRec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{"status": "ready"})
	if httpRec.Code != http.StatusOK {
		t.Fatalf("(a) HTTP status = %d, want 200; body=%s", httpRec.Code, httpRec.Body.String())
	}

	postRows, err := ledgerStore.List(ctx, proj.ID, ledger.ListOptions{StoryID: st.ID, Tags: []string{"kind:story.status_change"}}, nil)
	if err != nil {
		t.Fatalf("ledger list (post): %v", err)
	}
	if got := len(postRows) - len(baselineRows); got != 1 {
		t.Errorf("(b) kind:story.status_change row delta = %d, want 1", got)
	}

	final, err := stories.GetByID(ctx, st.ID, nil)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if final.Status != story.StatusReady {
		t.Errorf("(c) store Status = %q, want ready", final.Status)
	}
}

// TestStoryStatusUpdate_E2E_SadPath_IllegalTransition asserts that
// when the substrate rejects an illegal transition (done→backlog),
// none of the side-effects fire: 422, no new ledger row, store row
// unchanged.
func TestStoryStatusUpdate_E2E_SadPath_IllegalTransition(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, ledgerStore, stories := newTestPortal(t, &config.Config{Env: "dev"})

	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "terminal-target", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusReady, user.ID, now.Add(1*time.Second), nil)
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusInProgress, user.ID, now.Add(2*time.Second), nil)
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusDone, user.ID, now.Add(3*time.Second), nil)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	baselineRows, _ := ledgerStore.List(ctx, proj.ID, ledger.ListOptions{StoryID: st.ID, Tags: []string{"kind:story.status_change"}}, nil)

	httpRec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{"status": "backlog"})
	if httpRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("(a) HTTP status = %d, want 422; body=%s", httpRec.Code, httpRec.Body.String())
	}

	postRows, err := ledgerStore.List(ctx, proj.ID, ledger.ListOptions{StoryID: st.ID, Tags: []string{"kind:story.status_change"}}, nil)
	if err != nil {
		t.Fatalf("ledger list (post): %v", err)
	}
	if got := len(postRows) - len(baselineRows); got != 0 {
		t.Errorf("(b) kind:story.status_change row delta = %d, want 0", got)
	}

	final, err := stories.GetByID(ctx, st.ID, nil)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if final.Status != story.StatusDone {
		t.Errorf("(c) store Status = %q, want done (substrate must not have mutated)", final.Status)
	}
}

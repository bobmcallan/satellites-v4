package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/story"
)

// postStoryStatus drives a POST /api/stories/{id}/status request.
func postStoryStatus(t *testing.T, p *Portal, storyID, sessionCookie string, body any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/stories/"+storyID+"/status", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestStoryStatusUpdate_DerivableTransitionSucceeds asserts the
// canonical-step transition (backlog→ready) returns 200.
func TestStoryStatusUpdate_DerivableTransitionSucceeds(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "ready-flip", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{"status": "ready"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "ready" {
		t.Errorf("response status: %v", resp["status"])
	}
}

// TestStoryStatusUpdate_NonDerivableSucceedsWithoutReason asserts
// sty_01f75142: a forward jump bypassing intermediate states (e.g.
// backlog→done) returns 200 with NO reason in the request body. V3
// parity — status flips are free.
func TestStoryStatusUpdate_NonDerivableSucceedsWithoutReason(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "skip-jump", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{"status": "done"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "done" {
		t.Errorf("response status: %v", resp["status"])
	}
}

// TestStoryStatusUpdate_IllegalTransitionReturns422 asserts a
// transition the substrate rejects (e.g. done → backlog) returns 422.
func TestStoryStatusUpdate_IllegalTransitionReturns422(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "terminal", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	// Walk the story to done via the canonical chain so the substrate
	// transition matrix has a real terminal row.
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusReady, user.ID, now.Add(time.Second), nil)
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusInProgress, user.ID, now.Add(2*time.Second), nil)
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusDone, user.ID, now.Add(3*time.Second), nil)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{"status": "backlog"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// TestStoryStatusUpdate_CrossOwnerReturns404 asserts AC 6: posting
// against another user's story leaks neither status nor existence.
func TestStoryStatusUpdate_CrossOwnerReturns404(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	ctx := context.Background()
	now := time.Now().UTC()
	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)
	aliceProj, _ := projects.Create(ctx, alice.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: aliceProj.WorkspaceID, ProjectID: aliceProj.ID,
		Title: "alice-only", Status: story.StatusBacklog, CreatedBy: alice.ID,
	}, now)
	bobSess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)

	rec := postStoryStatus(t, p, st.ID, bobSess.ID, map[string]any{"status": "ready"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner POST should return 404, got %d", rec.Code)
	}
}

// TestStoryStatusUpdate_UnauthReturns401 asserts unauthenticated POST
// is rejected before any substrate write.
func TestStoryStatusUpdate_UnauthReturns401(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)
	body, _ := json.Marshal(map[string]any{"status": "ready"})
	req := httptest.NewRequest(http.MethodPost, "/api/stories/sty_x/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth POST should be 401, got %d", rec.Code)
	}
}

// TestStoryStatusPanel_RenderTimeAffordances asserts the stories panel
// renders the per-row status button-group, the row checkbox column,
// and the bulk action bar template (hidden via x-show until
// selectedIDs.size > 0). sty_01f75142: no reason-input testids — the
// reason field was removed alongside the reason-required gate.
func TestStoryStatusPanel_RenderTimeAffordances(t *testing.T) {
	t.Parallel()
	rec := renderWorkspace(t, "status:all", func(ctx context.Context, projectID string, stories *story.MemoryStore, _ *document.MemoryStore) {
		seedStory(t, stories, projectID, "render-target", "x", time.Now().UTC())
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		// Bulk action bar template.
		`data-testid="story-bulk-bar"`,
		`x-show="selectedIDs.size > 0"`,
		`data-testid="story-bulk-target"`,
		`data-testid="story-bulk-apply"`,
		`data-testid="story-bulk-clear"`,
		`data-testid="story-bulk-result"`,
		// Per-row checkbox column.
		`<th class="col-select"`,
		`data-testid="story-row-select-sty_`,
		`@change="toggleRowSelection(`,
		// Per-row status button-group inside the expand.
		`data-testid="story-status-buttons-sty_`,
		`data-testid="story-status-button-sty_`,
		`-backlog`,
		`-ready`,
		`-in_progress`,
		`-done`,
		`-cancelled`,
		// The current status (backlog by default for newly-seeded
		// rows) carries `disabled` so the operator can't no-op.
		`disabled aria-pressed="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Reason inputs were removed in sty_01f75142 — assert their
	// testids are gone so the bug doesn't regress silently.
	for _, gone := range []string{
		`data-testid="story-bulk-reason"`,
		`data-testid="story-status-reason-sty_`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("body contains removed reason-input %q", gone)
		}
	}
}

// TestStoryStatusUpdate_OpenTasksGateReturns422WithTaskIDs asserts
// sty_0233fabd: when the open-tasks gate fires, the handler returns
// 422 with a JSON body whose `open_task_ids` array lists every task
// id the operator must reconcile before retrying.
func TestStoryStatusUpdate_OpenTasksGateReturns422WithTaskIDs(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	stories.SetOpenTasksFunc(func(ctx context.Context, storyID string, memberships []string) ([]string, error) {
		return []string{"task_open1", "task_open2"}, nil
	})
	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "open-chain", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{"status": "done"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var resp struct {
		Error       string   `json:"error"`
		OpenTaskIDs []string `json:"open_task_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (raw=%s)", err, rec.Body.String())
	}
	if resp.Error == "" {
		t.Error("error field missing")
	}
	if len(resp.OpenTaskIDs) != 2 || resp.OpenTaskIDs[0] != "task_open1" || resp.OpenTaskIDs[1] != "task_open2" {
		t.Errorf("open_task_ids = %v, want [task_open1 task_open2]", resp.OpenTaskIDs)
	}

	// Status must remain backlog — gate must not let the write through.
	got, _ := stories.GetByID(ctx, st.ID, nil)
	if got.Status != story.StatusBacklog {
		t.Errorf("status leaked through gate: got %q, want backlog", got.Status)
	}
}

// TestStoryStatusUpdate_MissingStatusReturns400 asserts the body
// validation gate.
func TestStoryStatusUpdate_MissingStatusReturns400(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	ctx := context.Background()
	now := time.Now().UTC()
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "wksp_a", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		WorkspaceID: proj.WorkspaceID, ProjectID: proj.ID,
		Title: "missing", Status: story.StatusBacklog, CreatedBy: user.ID,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec := postStoryStatus(t, p, st.ID, sess.ID, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

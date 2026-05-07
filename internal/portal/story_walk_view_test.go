package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// renderStoryWalk drives a GET /stories/{sid}/walk request against p.
func renderStoryWalk(t *testing.T, p *Portal, storyID, sessionCookie string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/stories/"+storyID+"/walk", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestStoryWalk_LoopGroupsByContractName verifies sty_557df61e — a
// 3-lap develop story renders one group header carrying "develop ×3"
// with three nested cards, plus a separate push group with one card.
// sty_c6d76a5b checkpoint 14: the walk projects from kind:work tasks
// instead of contract_instance rows; cards group by Action.
func TestStoryWalk_LoopGroupsByContractName(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	tasks := p.tasks
	if tasks == nil {
		t.Fatalf("portal fixture missing task store")
	}
	ctx := context.Background()
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, err := projects.Create(ctx, user.ID, "", "alpha", now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}
	st, err := stories.Create(ctx, story.Story{
		ProjectID: proj.ID,
		Title:     "loop story",
		Status:    "in_progress",
		Priority:  "high",
		Category:  "feature",
		CreatedBy: user.ID,
	}, now)
	if err != nil {
		t.Fatalf("story create: %v", err)
	}

	// Three develop work tasks (retry chain via prior_task_id) + one push.
	dev1, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws", ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindWork, Action: task.ContractAction("develop"),
		Origin: task.OriginStoryStage, Priority: task.PriorityMedium,
		Status: task.StatusPublished,
	}, now)
	if err != nil {
		t.Fatalf("dev1 enqueue: %v", err)
	}
	if _, err := tasks.ClaimByID(ctx, dev1.ID, "worker", now.Add(time.Minute), nil); err != nil {
		t.Fatalf("dev1 claim: %v", err)
	}
	if _, err := tasks.Close(ctx, dev1.ID, task.OutcomeFailure, now.Add(2*time.Minute), nil); err != nil {
		t.Fatalf("dev1 close failure: %v", err)
	}
	dev2, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws", ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindWork, Action: task.ContractAction("develop"),
		PriorTaskID: dev1.ID,
		Origin:      task.OriginStoryStage, Priority: task.PriorityMedium,
		Status: task.StatusPublished,
	}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("dev2 enqueue: %v", err)
	}
	if _, err := tasks.ClaimByID(ctx, dev2.ID, "worker", now.Add(4*time.Minute), nil); err != nil {
		t.Fatalf("dev2 claim: %v", err)
	}
	if _, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws", ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindWork, Action: task.ContractAction("develop"),
		PriorTaskID: dev2.ID,
		Origin:      task.OriginStoryStage, Priority: task.PriorityMedium,
		Status: task.StatusPublished,
	}, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("dev3 enqueue: %v", err)
	}
	if _, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws", ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindWork, Action: task.ContractAction("push"),
		Origin: task.OriginStoryStage, Priority: task.PriorityMedium,
		Status: task.StatusPublished,
	}, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("push enqueue: %v", err)
	}

	sess, err := sessions.Create(user.ID, auth.DefaultSessionTTL)
	if err != nil {
		t.Fatalf("session create: %v", err)
	}

	rec := renderStoryWalk(t, p, st.ID, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="story-walk-header"`,
		`data-testid="story-walk-timeline"`,
		`data-testid="walk-group-develop"`,
		`develop &times;3`,
		`data-testid="walk-group-push"`,
		`develop #1`,
		`develop #2`,
		`develop #3`,
		`push #1`,
		`status-failed`,
		`status-claimed`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("walk body missing %q", want)
		}
	}
}

// TestStoryWalk_EmptyStoryShowsHint renders the "no contract walk yet"
// empty state.
func TestStoryWalk_EmptyStoryShowsHint(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	ctx := context.Background()
	now := time.Now().UTC()

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)
	st, _ := stories.Create(ctx, story.Story{
		ProjectID: proj.ID,
		Title:     "fresh",
		Status:    "backlog",
		Priority:  "low",
		Category:  "feature",
		CreatedBy: user.ID,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	rec := renderStoryWalk(t, p, st.ID, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="story-walk-empty"`,
		`workflow_claim`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

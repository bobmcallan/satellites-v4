package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/task"
)

func renderTasks(t *testing.T, p *Portal, sessionCookie string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	if sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func renderTaskDrawer(t *testing.T, p *Portal, taskID, sessionCookie string) (*httptest.ResponseRecorder, taskDrawer) {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil)
	if sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var d taskDrawer
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &d)
	}
	return rec, d
}

func TestTasksView_UnauthRedirects(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	rec := renderTasks(t, p, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect to login)", rec.Code)
	}
}

func TestTasksView_EmptyColumnsRender(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec := renderTasks(t, p, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="column-in-flight"`,
		`data-testid="column-enqueued"`,
		`data-testid="column-closed"`,
		`data-testid="in-flight-empty-ssr"`,
		`data-testid="enqueued-empty-ssr"`,
		`data-testid="closed-empty-ssr"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tasks empty body missing %q", want)
		}
	}
}

func TestTasksView_PopulatedColumns(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _, tasks := newTestPortalWithTasks(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	now := time.Now().UTC()
	ctx := context.Background()
	// Seed: one enqueued, one in_flight (claimed), one closed.
	enq, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws_test",
		StoryID:     "sty_demo01",
		Origin:      task.OriginStoryStage,
		Priority:    task.PriorityHigh,
		Status:      task.StatusEnqueued,
	}, now)
	if err != nil {
		t.Fatalf("seed enqueued: %v", err)
	}
	if _, err := tasks.Claim(ctx, "worker_a", []string{"ws_test"}, now.Add(time.Second)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	closed, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws_test",
		Origin:      task.OriginScheduled,
		Priority:    task.PriorityMedium,
		Status:      task.StatusEnqueued,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("seed closed-source: %v", err)
	}
	if _, err := tasks.Claim(ctx, "worker_b", []string{"ws_test"}, now.Add(3*time.Second)); err != nil {
		t.Fatalf("claim closed-source: %v", err)
	}
	if _, err := tasks.Close(ctx, closed.ID, task.OutcomeSuccess, now.Add(4*time.Second), nil); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := tasks.Enqueue(ctx, task.Task{
		WorkspaceID: "ws_test",
		Origin:      task.OriginStoryProducing,
		Priority:    task.PriorityLow,
		Status:      task.StatusEnqueued,
	}, now.Add(5*time.Second)); err != nil {
		t.Fatalf("seed second enqueued: %v", err)
	}

	rec := renderTasks(t, p, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Three different origins should all appear in the rendered SSR.
	for _, want := range []string{
		"story_stage", "scheduled", "story_producing",
		"sty_demo01", // story_id badge from t.StoryID
		"worker_a",   // claimed_by on in_flight card
		"outcome-success",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("populated body missing %q", want)
		}
	}
	// In-flight column should NOT show the "no tasks" SSR marker now.
	if strings.Contains(body, `data-testid="in-flight-empty-ssr"`) {
		t.Errorf("in-flight column still shows empty SSR marker after seeding")
	}
	// Drawer endpoint returns the enq task's payload.
	rec2, drawer := renderTaskDrawer(t, p, enq.ID, sess.ID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("drawer status = %d, want 200", rec2.Code)
	}
	if drawer.Task.ID != enq.ID {
		t.Errorf("drawer task id = %q, want %q", drawer.Task.ID, enq.ID)
	}
	if drawer.Task.StoryID != "sty_demo01" {
		t.Errorf("drawer story id = %q, want sty_demo01", drawer.Task.StoryID)
	}
}

func TestTasksView_DrawerNotFound(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _, _ := newTestPortalWithTasks(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec, _ := renderTaskDrawer(t, p, "task_missing", sess.ID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for missing task", rec.Code)
	}
}

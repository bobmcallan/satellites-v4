package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
)

func newTestPortal(t *testing.T, cfg *config.Config) (*Portal, *auth.MemoryUserStore, *auth.MemorySessionStore, *project.MemoryStore, *ledger.MemoryStore, *story.MemoryStore) {
	t.Helper()
	users := auth.NewMemoryUserStore()
	sessions := auth.NewMemorySessionStore()
	projects := project.NewMemoryStore()
	ledgerStore := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(ledgerStore)
	p, err := New(cfg, satarbor.New("info"), sessions, users, projects, ledgerStore, stories, time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, users, sessions, projects, ledgerStore, stories
}

func TestLanding_UnauthRedirects(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t,&config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want /login prefix", loc)
	}
}

func TestLanding_AuthRenders(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Signed in as") {
		t.Errorf("body missing \"Signed in as\": %s", body)
	}
	if !strings.Contains(body, "alice@local") {
		t.Errorf("body missing user email: %s", body)
	}
	if !strings.Contains(body, "version-chip") {
		t.Errorf("body missing version chip: %s", body)
	}
}

func TestLogin_RendersBasicForm(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, _, _, _, _, _ := newTestPortal(t,cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`action="/auth/login"`,
		`name="username"`,
		`name="password"`,
		`DevMode login`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login body missing %q", want)
		}
	}
}

func TestLogin_ShowsOAuthWhenConfigured(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Env: "dev", DevMode: true,
		GoogleClientID: "gid", GoogleClientSecret: "gs",
		GithubClientID: "hid", GithubClientSecret: "hs",
	}
	p, _, _, _, _, _ := newTestPortal(t,cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`/auth/google/start`,
		`/auth/github/start`,
		`Sign in with Google`,
		`Sign in with GitHub`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login body missing %q", want)
		}
	}
}

func TestLogin_HidesDevModeInProd(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "prod", DevMode: true, DBDSN: "x"}
	p, _, _, _, _, _ := newTestPortal(t,cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "DevMode login") {
		t.Errorf("prod body must not show DevMode affordance")
	}
}

func TestProjectsList_UnauthRedirects(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("Location = %q, want /login prefix", rec.Header().Get("Location"))
	}
}

func TestProjectsList_EmptyStateRenders(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "You don't own any projects yet") {
		t.Errorf("missing empty-state copy; body=%s", body)
	}
}

func TestProjectsList_RendersOwnedOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)

	now := time.Now().UTC()
	_, _ = projects.Create(ctx, alice.ID, "", "alpha", now)
	_, _ = projects.Create(ctx, alice.ID, "", "beta", now.Add(time.Hour))
	_, _ = projects.Create(ctx, bob.ID, "", "bob-only", now)

	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, body)
	}
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "bob-only") {
		t.Errorf("body must not leak bob's project: %s", body)
	}
}

func TestProjectDetail_OwnerRenders(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(alice)
	proj, _ := projects.Create(ctx, alice.ID, "", "alpha", time.Now().UTC())

	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{proj.ID, "alpha", alice.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
}

func TestProjectDetail_CrossOwner404(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)
	proj, _ := projects.Create(ctx, alice.ID, "", "alice-only", time.Now().UTC())

	sess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no existence leak)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alice-only") {
		t.Errorf("404 body must not leak project name")
	}
}

func TestProjectDetail_MissingIs404(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj_missing", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProjectLedger_UnauthRedirects(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj_any/ledger", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
}

func TestProjectLedger_CrossOwner404(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)
	proj, _ := projects.Create(ctx, alice.ID, "", "alice-only", time.Now().UTC())

	sess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/ledger", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alice-only") {
		t.Errorf("404 body leaked project name")
	}
}

func TestProjectLedger_RendersNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, ledgerStore, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{ProjectID: proj.ID, Type: "story.created", Actor: "u_alice", Content: "first-event"}, t0)
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{ProjectID: proj.ID, Type: "story.status_change", Actor: "u_alice", Content: "second-event"}, t0.Add(time.Hour))
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{ProjectID: proj.ID, Type: "document.ingest", Actor: "u_alice", Content: "third-event"}, t0.Add(2*time.Hour))

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/ledger", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"story.created", "story.status_change", "document.ingest", "first-event", "second-event", "third-event", "panel-body"} {
		if !strings.Contains(body, want) {
			t.Errorf("ledger body missing %q", want)
		}
	}
	// Newest-first: "third-event" content should appear before "first-event".
	if strings.Index(body, "third-event") > strings.Index(body, "first-event") {
		t.Errorf("entries not newest-first; body=%s", body)
	}
}

func TestProjectLedger_LimitParamTruncates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, ledgerStore, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())

	t0 := time.Now().UTC()
	for i := 0; i < 4; i++ {
		_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{ProjectID: proj.ID, Type: "t", Content: "entry-" + string(rune('A'+i))}, t0.Add(time.Duration(i)*time.Second))
	}

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/ledger?limit=2", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Two newest: D and C; A and B must not appear in content.
	if !strings.Contains(body, "entry-D") || !strings.Contains(body, "entry-C") {
		t.Errorf("body missing newest two entries")
	}
	if strings.Contains(body, "entry-A") || strings.Contains(body, "entry-B") {
		t.Errorf("limit=2 leaked older entries")
	}
}

func TestProjectLedger_EmptyState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/ledger", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No ledger entries yet") {
		t.Errorf("missing empty-state copy")
	}
}

func TestStoriesList_UnauthRedirects(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj_any/stories", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
}

func TestStoriesList_DefaultHidesTerminal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())

	// Seed 4 stories, one per terminal bucket.
	specs := []struct {
		title  string
		status string
	}{
		{"back", story.StatusBacklog},
		{"ready-now", story.StatusReady},
		{"wip", story.StatusInProgress},
		{"finished", story.StatusDone},
	}
	for i, sp := range specs {
		st, _ := stories.Create(ctx, story.Story{ProjectID: proj.ID, Title: sp.title, CreatedBy: user.ID}, time.Now().Add(time.Duration(i)*time.Second))
		// advance status as needed
		cur := story.StatusBacklog
		for cur != sp.status {
			var next string
			switch cur {
			case story.StatusBacklog:
				next = story.StatusReady
			case story.StatusReady:
				next = story.StatusInProgress
			case story.StatusInProgress:
				next = story.StatusDone
			}
			_, _ = stories.UpdateStatus(ctx, st.ID, next, user.ID, time.Now().Add(time.Duration(i)*time.Second+time.Millisecond))
			cur = next
			if next == sp.status {
				break
			}
		}
	}

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	// Default (no filter): done should NOT appear; others should.
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/stories", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"back", "ready-now", "wip"} {
		if !strings.Contains(body, want) {
			t.Errorf("default body missing %q", want)
		}
	}
	if strings.Contains(body, "finished") {
		t.Errorf("default body must not include done story; body=%s", body)
	}

	// ?status=all includes finished.
	reqAll := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/stories?status=all", nil)
	reqAll.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	recAll := httptest.NewRecorder()
	mux.ServeHTTP(recAll, reqAll)
	if recAll.Code != http.StatusOK {
		t.Fatalf("all status = %d, want 200", recAll.Code)
	}
	if !strings.Contains(recAll.Body.String(), "finished") {
		t.Errorf("?status=all should include done story")
	}
}

func TestStoriesList_RowColumns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())

	_, _ = stories.Create(ctx, story.Story{
		ProjectID: proj.ID,
		Title:     "the-title",
		Priority:  "high",
		Tags:      []string{"epic:v4-stories"},
		CreatedBy: user.ID,
	}, time.Now())

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/stories", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"the-title", "backlog", "high", "epic:v4-stories", "panel-body"} {
		if !strings.Contains(body, want) {
			t.Errorf("row body missing %q", want)
		}
	}
}

func TestStoriesList_CrossOwner404(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)
	proj, _ := projects.Create(ctx, alice.ID, "", "alice-only", time.Now().UTC())

	sess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/stories", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner status = %d, want 404", rec.Code)
	}
}

func TestStoryDetail_RendersFieldsAndHistory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())

	base := time.Now().UTC()
	st, _ := stories.Create(ctx, story.Story{
		ProjectID:          proj.ID,
		Title:              "the-subject",
		Description:        "the-description",
		AcceptanceCriteria: "the-AC",
		Priority:           "high",
		Category:           "feature",
		Tags:               []string{"epic:v4-stories"},
		CreatedBy:          user.ID,
	}, base)

	// Transition through the lifecycle.
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusReady, user.ID, base.Add(1*time.Second))
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusInProgress, user.ID, base.Add(2*time.Second))
	_, _ = stories.UpdateStatus(ctx, st.ID, story.StatusDone, user.ID, base.Add(3*time.Second))

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/stories/"+st.ID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"the-subject",
		"the-description",
		"the-AC",
		"the-AC",
		"epic:v4-stories",
		"status history",
		"backlog",
		"ready",
		"in_progress",
		"done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
}

func TestStoryDetail_CrossOwner404(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, _, stories := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)
	proj, _ := projects.Create(ctx, alice.ID, "", "alice-only", time.Now().UTC())
	st, _ := stories.Create(ctx, story.Story{ProjectID: proj.ID, Title: "secret", CreatedBy: alice.ID}, time.Now())

	sess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/stories/"+st.ID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("404 body leaked title")
	}
}

func TestHead_HasBlockingThemeScript(t *testing.T) {
	t.Parallel()
	p, _, _, _, _, _ := newTestPortal(t,&config.Config{Env: "dev", DevMode: true})
	mux := http.NewServeMux()
	p.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	// The blocking script must appear before </head> and must not have
	// defer/async attributes — otherwise it would be async and allow a
	// flash of the wrong palette.
	headEnd := strings.Index(body, "</head>")
	if headEnd < 0 {
		t.Fatal("no </head> in body")
	}
	head := body[:headEnd]
	if !strings.Contains(head, "localStorage.getItem('theme')") {
		t.Errorf("head missing theme-resolving script")
	}
	if strings.Contains(head, "<script defer>") || strings.Contains(head, "<script async>") {
		t.Errorf("theme script must NOT be defer/async (causes flash)")
	}
}

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
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/task"
)

func renderRepo(t *testing.T, p *Portal, sessionCookie string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/repo", nil)
	if sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestRepoView_EmptyState(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	rec := renderRepo(t, p, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-testid="repo-empty"`) {
		t.Errorf("expected repo-empty marker for no-project + no-repo state")
	}
}

func TestRepoView_HeaderRendersWhenRepoExists(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	ctx := context.Background()
	now := time.Now().UTC()
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)

	// Seed a repo via the portal's exposed `repos` field by reaching
	// into the helper. Since newTestPortal creates the repo store
	// internally, we use the `repos` MemoryStore.
	if _, err := p.repos.Create(ctx, repo.Repo{
		ProjectID:     proj.ID,
		GitRemote:     "git@example.com:alpha/main.git",
		DefaultBranch: "main",
		HeadSHA:       "abcdef0123",
		Status:        repo.StatusActive,
		SymbolCount:   42,
		FileCount:     7,
		IndexVersion:  3,
	}, now); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	rec := renderRepo(t, p, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="repo-header"`,
		`data-testid="symbol-search-panel"`,
		`data-testid="recent-commits-panel"`,
		`data-testid="branch-diff-panel"`,
		"git@example.com:alpha/main.git",
		"abcdef0123",
		`>42<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("repo body missing %q", want)
		}
	}
	if strings.Contains(body, `data-testid="repo-empty"`) {
		t.Errorf("repo body should not show empty state when repo exists")
	}
}

func TestRepoSymbols_IndexerStubReturns503(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	ctx := context.Background()
	now := time.Now().UTC()
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)
	repoRow, _ := p.repos.Create(ctx, repo.Repo{
		ProjectID:     proj.ID,
		GitRemote:     "git@example.com:alpha/main.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, now)

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/"+repoRow.ID+"/symbols?q=foo", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (stub indexer)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "index_unavailable") {
		t.Errorf("body missing index_unavailable error code: %s", body)
	}
}

func TestRepoView_CommitsRendered(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	ctx := context.Background()
	now := time.Now().UTC()
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)
	repoRow, _ := p.repos.Create(ctx, repo.Repo{
		ProjectID:     proj.ID,
		GitRemote:     "git@example.com:alpha/main.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, now)
	for i, sha := range []string{"sha_old", "sha_mid", "sha_new"} {
		_, _ = p.repos.UpsertCommit(ctx, repo.Commit{
			RepoID: repoRow.ID, SHA: sha,
			Subject:     "msg-" + sha,
			Author:      "Alice",
			CommittedAt: now.Add(time.Duration(i) * time.Minute),
		})
	}
	rec := renderRepo(t, p, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `data-testid="commits-empty"`) {
		t.Errorf("commits-empty marker present despite persisted commits")
	}
	if !strings.Contains(body, `data-testid="commit-list"`) {
		t.Errorf("commit-list marker missing from rendered repo view")
	}
	for _, sha := range []string{"sha_old", "sha_mid", "sha_new"} {
		if !strings.Contains(body, sha) {
			t.Errorf("commit list missing sha %s", sha)
		}
	}
}

func TestRepoDiffEndpoint_HappyPath(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	ctx := context.Background()
	now := time.Now().UTC()
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)
	repoRow, _ := p.repos.Create(ctx, repo.Repo{
		ProjectID:     proj.ID,
		GitRemote:     "git@example.com:alpha/main.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, now)
	chain := []repo.Commit{
		{RepoID: repoRow.ID, SHA: "c1", CommittedAt: now},
		{RepoID: repoRow.ID, SHA: "c2", ParentSHA: "c1", CommittedAt: now.Add(time.Minute)},
	}
	for _, c := range chain {
		_, _ = p.repos.UpsertCommit(ctx, c)
	}

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/"+repoRow.ID+"/diff?from=c1&to=c2", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"diff_source":"unavailable"`,
		`"from_ref":"c1"`,
		`"to_ref":"c2"`,
		`"sha":"c2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("diff response missing %q; body=%s", want, body)
		}
	}
}

func TestRepoDiffEndpoint_404OnUnknownRepo(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/repo_missing/diff?from=a&to=b", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRepoReindexEndpoint_AdminSucceeds_202(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	wsRow, _ := ws.Create(testCtx(), user.ID, "alpha", time.Now().UTC())
	// Admin role auto-granted to creator (workspace.MemoryStore.Create).
	repoRow, _ := p.repos.Create(testCtx(), repo.Repo{
		WorkspaceID:   wsRow.ID,
		ProjectID:     "proj_a",
		GitRemote:     "git@x:y.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, time.Now().UTC())

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/repos/"+repoRow.ID+"/reindex", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["task_id"] == nil || resp["task_id"] == "" {
		t.Errorf("task_id missing in response: %s", rec.Body.String())
	}
	if resp["repo_id"] != repoRow.ID {
		t.Errorf("repo_id = %v, want %s", resp["repo_id"], repoRow.ID)
	}
	tasks, _ := p.tasks.List(testCtx(), task.ListOptions{}, nil)
	if len(tasks) != 1 {
		t.Errorf("tasks queued = %d, want 1", len(tasks))
	}
}

func TestRepoReindexEndpoint_NonAdminRejected_403(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})
	owner := auth.User{ID: "u_owner", Email: "owner@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(owner)
	users.Add(bob)
	sess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)

	wsRow, _ := ws.Create(testCtx(), owner.ID, "alpha", time.Now().UTC())
	// Add bob as a member with viewer role.
	if err := ws.AddMember(testCtx(), wsRow.ID, bob.ID, "viewer", "test", time.Now().UTC()); err != nil {
		t.Fatalf("AddMember bob: %v", err)
	}
	// Sticky bob's session to the workspace.
	if err := sessions.SetActiveWorkspace(sess.ID, wsRow.ID); err != nil {
		t.Fatalf("SetActiveWorkspace: %v", err)
	}
	repoRow, _ := p.repos.Create(testCtx(), repo.Repo{
		WorkspaceID:   wsRow.ID,
		ProjectID:     "proj_a",
		GitRemote:     "git@x:y.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, time.Now().UTC())

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/repos/"+repoRow.ID+"/reindex", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	tasks, _ := p.tasks.List(testCtx(), task.ListOptions{}, nil)
	if len(tasks) != 0 {
		t.Errorf("tasks queued = %d on non-admin, want 0", len(tasks))
	}
}

func TestRepoReindexEndpoint_404OnUnknownRepo(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	_, _ = ws.Create(testCtx(), user.ID, "alpha", time.Now().UTC())

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/repos/repo_missing/reindex", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRepoView_AdminSeesReindexButton(t *testing.T) {
	t.Parallel()
	p, users, sessions, ws := newPortalWithWorkspace(t, &config.Config{Env: "dev"})
	admin := auth.User{ID: "u_admin", Email: "admin@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(admin)
	users.Add(bob)
	wsRow, _ := ws.Create(testCtx(), admin.ID, "alpha", time.Now().UTC())
	if err := ws.AddMember(testCtx(), wsRow.ID, bob.ID, "viewer", "test", time.Now().UTC()); err != nil {
		t.Fatalf("AddMember bob: %v", err)
	}
	// Need a project owned by each user so the repo view picks it up.
	// newPortalWithWorkspace's projects store is internal; reach into p.
	now := time.Now().UTC()
	adminProj, _ := p.projects.Create(testCtx(), admin.ID, wsRow.ID, "alpha-proj", now)
	bobProj, _ := p.projects.Create(testCtx(), bob.ID, wsRow.ID, "bob-proj", now)
	_, _ = p.repos.Create(testCtx(), repo.Repo{
		WorkspaceID:   wsRow.ID,
		ProjectID:     adminProj.ID,
		GitRemote:     "git@x:admin.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, now)
	_, _ = p.repos.Create(testCtx(), repo.Repo{
		WorkspaceID:   wsRow.ID,
		ProjectID:     bobProj.ID,
		GitRemote:     "git@x:bob.git",
		DefaultBranch: "main",
		Status:        repo.StatusActive,
	}, now)

	adminSess, _ := sessions.Create(admin.ID, auth.DefaultSessionTTL)
	_ = sessions.SetActiveWorkspace(adminSess.ID, wsRow.ID)
	bobSess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)
	_ = sessions.SetActiveWorkspace(bobSess.ID, wsRow.ID)

	adminRec := renderRepo(t, p, adminSess.ID)
	bobRec := renderRepo(t, p, bobSess.ID)
	if adminRec.Code != http.StatusOK || bobRec.Code != http.StatusOK {
		t.Fatalf("admin status=%d bob status=%d", adminRec.Code, bobRec.Code)
	}
	if !strings.Contains(adminRec.Body.String(), `data-testid="reindex-btn"`) {
		t.Errorf("admin response missing reindex button")
	}
	if strings.Contains(bobRec.Body.String(), `data-testid="reindex-btn"`) {
		t.Errorf("non-admin response leaked reindex button")
	}
}

func TestRepoSymbols_404OnUnknownRepo(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/repo_missing/symbols", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

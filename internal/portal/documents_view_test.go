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
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/story"
)

func renderDocuments(t *testing.T, p *Portal, sessionCookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	u := "/documents"
	if query != "" {
		u += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	if sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func renderDocumentDetail(t *testing.T, p *Portal, id, sessionCookie string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/documents/"+id, nil)
	if sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDocumentsList_Empty(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	rec := renderDocuments(t, p, sess.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="documents-header"`,
		`data-testid="type-tabs"`,
		`data-testid="tab-all"`,
		`data-testid="tab-contract"`,
		`data-testid="documents-empty-ssr"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("documents body missing %q", want)
		}
	}
}

func TestDocumentsList_TypeTabFilters(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	now := time.Now().UTC()
	ctx := context.Background()
	if _, err := docs.Create(ctx, document.Document{
		Type: "contract", Scope: "system", Name: "preplan", Status: "active",
		Body: "preplan contract body",
	}, now); err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	if _, err := docs.Create(ctx, document.Document{
		Type: "principle", Scope: "system", Name: "no-shortcuts", Status: "active",
		Body: "principle body",
	}, now); err != nil {
		t.Fatalf("seed principle: %v", err)
	}

	rec := renderDocuments(t, p, sess.ID, "type=contract")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "preplan") {
		t.Errorf("contract tab missing preplan name")
	}
	if strings.Contains(body, "no-shortcuts") {
		t.Errorf("contract tab leaked principle row")
	}
}

func TestDocumentDetail_LinkedStoriesByTag(t *testing.T) {
	t.Parallel()
	p, users, sessions, projects, _, stories, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	now := time.Now().UTC()
	ctx := context.Background()
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)
	projID := proj.ID
	doc, err := docs.Create(ctx, document.Document{
		Type: "artifact", Scope: "project", ProjectID: &projID, Name: "ui-design.md", Status: "active",
		Body: "ui design body",
	}, now)
	if err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	// Two stories; one cites the doc via source: tag, the other does not.
	matchStory, _ := stories.Create(ctx, story.Story{
		ProjectID: proj.ID, Title: "linked story",
		Status: "in_progress", Priority: "high", Category: "feature",
		Tags: []string{"source:ui-design.md#story-view"},
	}, now)
	_, _ = stories.Create(ctx, story.Story{
		ProjectID: proj.ID, Title: "unlinked story",
		Status: "in_progress", Priority: "high", Category: "feature",
		Tags: []string{"epic:portal-views"},
	}, now)

	rec := renderDocumentDetail(t, p, doc.ID, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, matchStory.ID) {
		t.Errorf("linked stories missing the matching story id")
	}
	if !strings.Contains(body, "linked story") {
		t.Errorf("linked stories missing the matching story title")
	}
	if strings.Contains(body, "unlinked story") {
		t.Errorf("unlinked story leaked into linked-stories panel")
	}
	if !strings.Contains(body, `data-testid="version-empty"`) {
		t.Errorf("version-history empty marker missing for v1 document")
	}
}

func TestDocumentDetail_404OnUnknownID(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	rec := renderDocumentDetail(t, p, "doc_missing", sess.ID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDocumentDetail_VersionHistoryPopulatedAfterUpdate(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	now := time.Now().UTC()
	ctx := context.Background()
	doc, err := docs.Create(ctx, document.Document{
		Type: "principle", Scope: "system", Name: "version-test", Status: "active",
		Body: "v1 body",
	}, now)
	if err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	body2 := "v2 body"
	if _, err := docs.Update(ctx, doc.ID, document.UpdateFields{Body: &body2}, "alice", now.Add(time.Minute), nil); err != nil {
		t.Fatalf("update v2: %v", err)
	}
	body3 := "v3 body"
	if _, err := docs.Update(ctx, doc.ID, document.UpdateFields{Body: &body3}, "alice", now.Add(2*time.Minute), nil); err != nil {
		t.Fatalf("update v3: %v", err)
	}

	rec := renderDocumentDetail(t, p, doc.ID, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="version-history"`) {
		t.Errorf("version-history list missing from rendered detail")
	}
	if strings.Contains(body, `data-testid="version-empty"`) {
		t.Errorf("version-empty placeholder leaked when versions exist")
	}
	if !strings.Contains(body, "/documents/"+doc.ID+"/versions/1") {
		t.Errorf("diff link to v1 missing")
	}
	if !strings.Contains(body, "/documents/"+doc.ID+"/versions/2") {
		t.Errorf("diff link to v2 missing")
	}
}

func TestDocumentVersionDetail_RendersHistoricalBody(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	now := time.Now().UTC()
	ctx := context.Background()
	doc, err := docs.Create(ctx, document.Document{
		Type: "principle", Scope: "system", Name: "version-render", Status: "active",
		Body: "original body content",
	}, now)
	if err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	body2 := "newer content"
	if _, err := docs.Update(ctx, doc.ID, document.UpdateFields{Body: &body2}, "alice", now.Add(time.Minute), nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/documents/"+doc.ID+"/versions/1", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "original body content") {
		t.Errorf("historical body missing from version-detail render")
	}
	if !strings.Contains(body, `data-testid="version-body"`) {
		t.Errorf("version-body marker missing")
	}
	if strings.Contains(body, "newer content") {
		t.Errorf("live body leaked into version-detail render")
	}
}

func TestDocumentVersionDetail_404OnUnknownVersion(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	now := time.Now().UTC()
	doc, err := docs.Create(context.Background(), document.Document{
		Type: "principle", Scope: "system", Name: "version-404", Status: "active",
		Body: "body",
	}, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mux := http.NewServeMux()
	p.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/documents/"+doc.ID+"/versions/99", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for non-existent version", rec.Code)
	}
}

func TestParseDocumentFilters_Variants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want documentFilters
	}{
		{"", documentFilters{}},
		{"type=skill", documentFilters{Type: "skill"}},
		{"q=v4&sort=name_asc", documentFilters{Query: "v4", Sort: "name_asc"}},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x?"+c.raw, nil)
		got := parseDocumentFilters(req)
		if got != c.want {
			t.Errorf("parseDocumentFilters(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

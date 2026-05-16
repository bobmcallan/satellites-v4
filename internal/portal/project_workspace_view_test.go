package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
)

// seedStory creates a story stamped with a deterministic UpdatedAt so
// sort assertions are stable. The MemoryStore lets us bypass UpdateStatus
// (which requires a ledger emission per row) and just write directly.
func seedStory(t *testing.T, stories *story.MemoryStore, projectID, title, desc string, updated time.Time) story.Story {
	t.Helper()
	s, err := stories.Create(context.Background(), story.Story{
		ProjectID:   projectID,
		Title:       title,
		Description: desc,
		Status:      "backlog",
		Priority:    "medium",
		Tags:        []string{"epic:test"},
	}, updated)
	if err != nil {
		t.Fatalf("seed story %q: %v", title, err)
	}
	return s
}

// seedDoc creates a document scoped to projectID (or system when
// projectID is empty) with deterministic UpdatedAt.
func seedDoc(t *testing.T, docs *document.MemoryStore, projectID, docType, name, body string, updated time.Time) document.Document {
	t.Helper()
	d := document.Document{
		Type:   docType,
		Name:   name,
		Body:   body,
		Status: "active",
	}
	if projectID == "" {
		d.Scope = document.ScopeSystem
	} else {
		d.Scope = document.ScopeProject
		pid := projectID
		d.ProjectID = &pid
	}
	out, err := docs.Create(context.Background(), d, updated)
	if err != nil {
		t.Fatalf("seed doc %q: %v", name, err)
	}
	return out
}

func TestProjectWorkspace_EmptyQueryReturnsRecentRows(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	docs := document.NewMemoryStore()

	now := time.Now().UTC()
	seedStory(t, stories, "proj_a", "alpha story", "first", now.Add(-2*time.Hour))
	seedStory(t, stories, "proj_a", "beta story", "second", now.Add(-1*time.Hour))
	seedDoc(t, docs, "proj_a", document.TypeArtifact, "design notes", "body", now.Add(-30*time.Minute))

	got := buildProjectWorkspaceComposite(context.Background(), stories, docs, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 25}, nil, false)

	if len(got.Stories) != 2 {
		t.Fatalf("Stories = %d, want 2", len(got.Stories))
	}
	if got.Stories[0].Title != "beta story" {
		t.Errorf("Stories[0].Title = %q, want %q (sort by UpdatedAt desc)", got.Stories[0].Title, "beta story")
	}
	if got.StoryTotal != 2 {
		t.Errorf("StoryTotal = %d, want 2", got.StoryTotal)
	}
	if got.DocTotal != 1 {
		t.Errorf("DocTotal = %d, want 1", got.DocTotal)
	}
}

func TestProjectWorkspace_ExcludesContractAndSkillDocs(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	docs := document.NewMemoryStore()

	now := time.Now().UTC()
	seedDoc(t, docs, "proj_a", document.TypeArtifact, "art", "x", now)
	seedDoc(t, docs, "proj_a", document.TypePrinciple, "prin", "x", now)
	contract := seedDoc(t, docs, "proj_a", document.TypeContract, "should-be-excluded", "x", now)
	// Skill needs a contract_binding pointing to a real active contract.
	skillBinding := contract.ID
	if _, err := docs.Create(context.Background(), document.Document{
		Type:            document.TypeSkill,
		Scope:           document.ScopeProject,
		ProjectID:       ptrTo("proj_a"),
		Name:            "should-be-excluded-2",
		Body:            "x",
		Status:          "active",
		ContractBinding: &skillBinding,
	}, now); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	got := buildProjectWorkspaceComposite(context.Background(), stories, docs, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 25}, nil, false)

	if len(got.Documents) != 2 {
		t.Fatalf("Documents = %d, want 2 (contract + skill excluded; artifact + principle remain)", len(got.Documents))
	}
	for _, d := range got.Documents {
		if d.Type == document.TypeContract || d.Type == document.TypeSkill {
			t.Errorf("Documents contains forbidden type %q", d.Type)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestProjectWorkspace_ProjectScopingExcludesOtherProjects(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	docs := document.NewMemoryStore()

	now := time.Now().UTC()
	seedStory(t, stories, "proj_a", "in-scope story", "x", now)
	seedStory(t, stories, "proj_other", "out-of-scope story", "x", now)
	seedDoc(t, docs, "proj_a", document.TypeArtifact, "in-scope art", "x", now)
	seedDoc(t, docs, "proj_other", document.TypeArtifact, "out-of-scope art", "x", now)

	got := buildProjectWorkspaceComposite(context.Background(), stories, docs, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 25}, nil, false)

	if len(got.Stories) != 1 || got.Stories[0].Title != "in-scope story" {
		t.Errorf("Stories = %#v, want only the proj_a story", got.Stories)
	}
	for _, d := range got.Documents {
		if d.Name == "out-of-scope art" {
			t.Errorf("Documents leaked cross-project doc %q", d.Name)
		}
	}
}

func TestProjectWorkspace_SystemScopeDocsIncluded(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	docs := document.NewMemoryStore()

	now := time.Now().UTC()
	seedDoc(t, docs, "proj_a", document.TypeArtifact, "project-art", "x", now)
	seedDoc(t, docs, "", document.TypePrinciple, "system-principle", "x", now)

	got := buildProjectWorkspaceComposite(context.Background(), stories, docs, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 25}, nil, false)

	names := map[string]bool{}
	for _, d := range got.Documents {
		names[d.Name] = true
	}
	if !names["project-art"] {
		t.Errorf("missing project-scope doc")
	}
	if !names["system-principle"] {
		t.Errorf("missing system-scope doc — workspace should include both scopes")
	}
}

func TestProjectWorkspace_DenyAllMembershipsReturnsEmpty(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	docs := document.NewMemoryStore()

	now := time.Now().UTC()
	seedStory(t, stories, "proj_a", "alpha", "x", now)
	seedDoc(t, docs, "proj_a", document.TypeArtifact, "art", "x", now)

	// Deny-all: empty (non-nil) memberships slice — both stores treat
	// an empty slice as "no workspace is visible".
	got := buildProjectWorkspaceComposite(context.Background(), stories, docs, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 25}, []string{}, false)

	if len(got.Stories) != 0 {
		t.Errorf("Stories under deny-all = %d, want 0", len(got.Stories))
	}
	if len(got.Documents) != 0 {
		t.Errorf("Documents under deny-all = %d, want 0", len(got.Documents))
	}
}

func TestProjectWorkspace_LimitCapsEachSection(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	docs := document.NewMemoryStore()

	now := time.Now().UTC()
	for i := 0; i < 30; i++ {
		seedStory(t, stories, "proj_a", "s"+strconv.Itoa(i), "x", now.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < 30; i++ {
		seedDoc(t, docs, "proj_a", document.TypeArtifact, "d"+strconv.Itoa(i), "x", now.Add(time.Duration(i)*time.Minute))
	}

	// sty_ca2e686b: stories now use Page/PageSize independently of
	// Limit. Limit still caps the non-story sections; stories are
	// capped by PageSize.
	got := buildProjectWorkspaceComposite(context.Background(), stories, docs, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 5, Page: 1, PageSize: 5}, nil, false)

	if len(got.Stories) != 5 {
		t.Errorf("Stories under PageSize=5 = %d, want 5", len(got.Stories))
	}
	if len(got.Documents) != 5 {
		t.Errorf("Documents under limit=5 = %d, want 5", len(got.Documents))
	}
}

// TestProjectWorkspace_StoryTotalReflectsFullCountBeyondPageSize covers
// sty_ca2e686b AC1 + AC2: seeding more stories than the default page
// size must surface the substrate-side count via StoryTotal — the bug
// was that StoryTotal collapsed to len(out.Stories) (the page window).
func TestProjectWorkspace_StoryTotalReflectsFullCountBeyondPageSize(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)

	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		seedStory(t, stories, "proj_a", "s"+strconv.Itoa(i), "x", now.Add(time.Duration(i)*time.Minute))
	}

	got := buildProjectWorkspaceComposite(context.Background(), stories, nil, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Page: 1, PageSize: projectWorkspacePageSize}, nil, false)

	if got.StoryTotal != 60 {
		t.Errorf("StoryTotal = %d, want 60 (pre-truncate substrate count)", got.StoryTotal)
	}
	if len(got.Stories) != projectWorkspacePageSize {
		t.Errorf("len(Stories) = %d, want %d (page window)", len(got.Stories), projectWorkspacePageSize)
	}
}

// TestProjectWorkspace_StoryPaginationReturnsCorrectSlice covers
// sty_ca2e686b AC3 + AC4: page=2 must return the second window of
// stories in UpdatedAt-desc order, not the first.
func TestProjectWorkspace_StoryPaginationReturnsCorrectSlice(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)

	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		seedStory(t, stories, "proj_a", "s"+strconv.Itoa(i), "x", now.Add(time.Duration(i)*time.Minute))
	}

	got := buildProjectWorkspaceComposite(context.Background(), stories, nil, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Page: 2, PageSize: 50}, nil, false)

	if len(got.Stories) != 10 {
		t.Fatalf("len(Stories) page=2 = %d, want 10", len(got.Stories))
	}
	// UpdatedAt desc: s59 newest. Page 1 covers s59..s10, so page 2
	// starts at s9.
	if got.Stories[0].Title != "s9" {
		t.Errorf("Stories[0].Title page=2 = %q, want %q (UpdatedAt-desc 51st row)", got.Stories[0].Title, "s9")
	}
	if got.StoryTotal != 60 {
		t.Errorf("StoryTotal page=2 = %d, want 60", got.StoryTotal)
	}
	if got.StoryPage != 2 || got.StoryPageSize != 50 || got.StoryPageCount != 2 {
		t.Errorf("paginator view-model = page %d / size %d / count %d, want 2/50/2", got.StoryPage, got.StoryPageSize, got.StoryPageCount)
	}
	if !got.StoryHasPrev || got.StoryHasNext {
		t.Errorf("page=2 of 2 should have prev only; got HasPrev=%v HasNext=%v", got.StoryHasPrev, got.StoryHasNext)
	}
}

func TestProjectWorkspace_NoStoreDegrades(t *testing.T) {
	t.Parallel()
	got := buildProjectWorkspaceComposite(context.Background(), nil, nil, nil, nil, nil, nil, "proj_a", projectWorkspaceFilters{Limit: 25}, nil, false)
	if len(got.Stories) != 0 || len(got.Documents) != 0 {
		t.Errorf("nil stores must yield empty composite, got %#v", got)
	}
}

func TestProjectWorkspace_ParseFiltersClampsLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want int
	}{
		{"", projectWorkspaceDefaultLimit},
		{"limit=10", 10},
		{"limit=0", projectWorkspaceDefaultLimit},
		{"limit=99999", projectWorkspaceMaxLimit},
		{"limit=notanumber", projectWorkspaceDefaultLimit},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/projects/x?"+tc.raw, nil)
		got := parseProjectWorkspaceFilters(req)
		if got.Limit != tc.want {
			t.Errorf("query %q: Limit = %d, want %d", tc.raw, got.Limit, tc.want)
		}
	}
}

// TestProjectWorkspace_ParseFiltersClampsStoriesPage covers sty_ca2e686b
// AC4: stories_page and stories_page_size query parameters must parse
// with defaults, lower-bound coercion, and PageSize clamping to
// projectWorkspaceListLimit.
func TestProjectWorkspace_ParseFiltersClampsStoriesPage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw          string
		wantPage     int
		wantPageSize int
	}{
		{"", 1, projectWorkspacePageSize},
		{"stories_page=", 1, projectWorkspacePageSize},
		{"stories_page=0", 1, projectWorkspacePageSize},
		{"stories_page=5", 5, projectWorkspacePageSize},
		{"stories_page=notanumber", 1, projectWorkspacePageSize},
		{"stories_page_size=", 1, projectWorkspacePageSize},
		{"stories_page_size=0", 1, projectWorkspacePageSize},
		{"stories_page_size=99999", 1, projectWorkspaceListLimit},
		{"stories_page_size=10", 1, 10},
		{"stories_page=3&stories_page_size=25", 3, 25},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/projects/x?"+tc.raw, nil)
		got := parseProjectWorkspaceFilters(req)
		if got.Page != tc.wantPage {
			t.Errorf("query %q: Page = %d, want %d", tc.raw, got.Page, tc.wantPage)
		}
		if got.PageSize != tc.wantPageSize {
			t.Errorf("query %q: PageSize = %d, want %d", tc.raw, got.PageSize, tc.wantPageSize)
		}
	}
}

// renderWorkspace boots a portal, signs in alice, creates her project,
// optionally seeds rows via seedFn, and returns the response from
// GET /projects/{id}?{query}.
func renderWorkspace(t *testing.T, query string, seedFn func(ctx context.Context, projectID string, stories *story.MemoryStore, docs *document.MemoryStore)) *httptest.ResponseRecorder {
	t.Helper()
	p, users, sessions, projects, _, stories, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(alice)

	ctx := context.Background()
	proj, err := projects.Create(ctx, alice.ID, "", "alpha", time.Now().UTC())
	if err != nil {
		t.Fatalf("project create: %v", err)
	}
	if seedFn != nil {
		seedFn(ctx, proj.ID, stories, docs)
	}

	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)
	url := "/projects/" + proj.ID
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// sty_9596b012 — the inline ledger panel is removed from the
// project detail page. The standalone /projects/{id}/ledger
// page is the canonical surface, and the LEDGER nav link
// (sty_5b92165a) routes there. No panel-ledger-* markers
// should appear on the project page.
func TestProjectWorkspaceRender_LedgerPanelRemoved(t *testing.T) {
	t.Parallel()
	rec := renderWorkspace(t, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, mustNot := range []string{
		`data-testid="panel-ledger-count"`,
		`data-testid="panel-ledger-body"`,
		`data-testid="panel-ledger-table"`,
		`data-testid="panel-ledger-empty"`,
		`data-testid="panel-ledger-open"`,
		`data-id="ledger"`,
	} {
		if strings.Contains(body, mustNot) {
			t.Errorf("project page still contains ledger-panel marker %q (sty_9596b012 removal incomplete)", mustNot)
		}
	}
	if !strings.Contains(body, `data-testid="panel-stories"`) {
		t.Errorf("stories panel missing — refactor likely overshot")
	}
}

func TestProjectWorkspaceRender_SectionsPresentNoSearchBox(t *testing.T) {
	t.Parallel()
	rec := renderWorkspace(t, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	wants := []string{
		`data-testid="panel-stories"`,
		`data-testid="panel-documents"`,
		`data-testid="panel-stories-empty"`,
		`data-testid="panel-documents-empty"`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	mustNot := []string{
		`data-testid="workspace-search-form"`,
		`data-testid="workspace-search-input"`,
		`data-testid="workspace-search-clear"`,
	}
	for _, no := range mustNot {
		if strings.Contains(body, no) {
			t.Errorf("body still contains workspace-search marker %q (story_59b11d8c moved search to /stories)", no)
		}
	}
}

func TestProjectWorkspaceRender_RowsRenderForSeededRows(t *testing.T) {
	t.Parallel()
	rec := renderWorkspace(t, "", func(ctx context.Context, projectID string, stories *story.MemoryStore, docs *document.MemoryStore) {
		now := time.Now().UTC()
		_, _ = stories.Create(ctx, story.Story{
			ProjectID: projectID, Title: "rendered story", Description: "x", Status: "backlog",
			Tags: []string{"epic:foo", "ui"},
		}, now)
		pid := projectID
		_, _ = docs.Create(ctx, document.Document{
			Type: document.TypeArtifact, Scope: document.ScopeProject, ProjectID: &pid,
			Name: "rendered doc", Body: "x", Status: "active",
		}, now)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"rendered story",
		"rendered doc",
		`data-testid="panel-stories-table"`,
		`data-testid="panel-documents-list"`,
		`href="/projects/`,
		`href="/documents/`,
		// sty_6300fb27 — story rows carry the data-* attributes the
		// client-side `order:<field>` and default-hide-done logic reads.
		`data-status="backlog"`,
		`data-priority=""`,
		`data-created="`,
		`data-updated="`,
		// sty_48198f3e — story rows now also carry data-category so the
		// V3-parity category: filter token can match without a server
		// round-trip.
		`data-category=""`,
		// Tags render in their own row below the title (V3 parity)
		// with a wrapper testid that exposes the per-story tag-row.
		`data-testid="story-row-tags-sty_`,
		`class="story-row-tags"`,
		`<button type="button" class="tag-chip is-clickable" data-tag="epic:foo"`,
		`@click.stop="addTagToQuery"`,
		// sty_48198f3e — V3-parity filter chip strip beneath the search
		// input. Defaults render dimmed; user-set chips render bright.
		`data-testid="panel-stories-chips"`,
		`class="panel-filter-chips"`,
		`x-for="chip in getEffectiveChips()"`,
		`@click="removeChip(chip.key, chip.value)"`,
		`@click="clearAllFilters"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The legacy "Open stories →" affordance is gone — the panel is the
	// primary surface now. sty_f4b87ea3 also removes the per-row "Open
	// story →" link from the expanded detail.
	for _, mustNot := range []string{
		`data-testid="panel-stories-open"`,
		`/projects/` + extractProjectID(body) + `/stories"`,
		`Open story →`,
	} {
		if mustNot != `/projects//stories"` && strings.Contains(body, mustNot) {
			t.Errorf("body should not contain %q after sty_6300fb27/sty_f4b87ea3", mustNot)
		}
	}
}

// TestProjectWorkspaceRender_StoriesPaginatorMarkup covers sty_ca2e686b
// AC3 + AC6: the paginator block renders when StoryTotal > PageSize and
// is absent when not; the panel-stories-count testid is preserved on
// the chip.
func TestProjectWorkspaceRender_StoriesPaginatorMarkup(t *testing.T) {
	t.Parallel()

	// >50 stories: paginator must render with next link and indicator;
	// count chip reports the substrate total (60), not the page size.
	recMany := renderWorkspace(t, "", func(ctx context.Context, projectID string, stories *story.MemoryStore, docs *document.MemoryStore) {
		now := time.Now().UTC()
		for i := 0; i < 60; i++ {
			_, _ = stories.Create(ctx, story.Story{
				ProjectID: projectID, Title: "s" + strconv.Itoa(i), Status: "backlog",
			}, now.Add(time.Duration(i)*time.Minute))
		}
	})
	if recMany.Code != http.StatusOK {
		t.Fatalf("status = %d", recMany.Code)
	}
	bodyMany := recMany.Body.String()
	for _, want := range []string{
		`data-testid="panel-stories-paginator"`,
		`data-testid="panel-stories-page-indicator"`,
		`data-testid="panel-stories-next"`,
		`page 1 of 2`,
		`data-testid="panel-stories-count">(60)`,
	} {
		if !strings.Contains(bodyMany, want) {
			t.Errorf("body (60 stories) missing %q", want)
		}
	}
	if strings.Contains(bodyMany, `data-testid="panel-stories-prev"`) {
		t.Errorf("page 1 should not render a prev link")
	}

	// page 2: prev link must appear; next must not.
	recPage2 := renderWorkspace(t, "stories_page=2", func(ctx context.Context, projectID string, stories *story.MemoryStore, docs *document.MemoryStore) {
		now := time.Now().UTC()
		for i := 0; i < 60; i++ {
			_, _ = stories.Create(ctx, story.Story{
				ProjectID: projectID, Title: "s" + strconv.Itoa(i), Status: "backlog",
			}, now.Add(time.Duration(i)*time.Minute))
		}
	})
	bodyPage2 := recPage2.Body.String()
	for _, want := range []string{
		`data-testid="panel-stories-paginator"`,
		`data-testid="panel-stories-prev"`,
		`page 2 of 2`,
	} {
		if !strings.Contains(bodyPage2, want) {
			t.Errorf("body page=2 missing %q", want)
		}
	}
	if strings.Contains(bodyPage2, `data-testid="panel-stories-next"`) {
		t.Errorf("page 2 of 2 should not render a next link")
	}

	// <=PageSize stories: paginator must NOT render. Use a small seed.
	recFew := renderWorkspace(t, "", func(ctx context.Context, projectID string, stories *story.MemoryStore, docs *document.MemoryStore) {
		now := time.Now().UTC()
		for i := 0; i < 3; i++ {
			_, _ = stories.Create(ctx, story.Story{
				ProjectID: projectID, Title: "s" + strconv.Itoa(i), Status: "backlog",
			}, now)
		}
	})
	bodyFew := recFew.Body.String()
	if strings.Contains(bodyFew, `data-testid="panel-stories-paginator"`) {
		t.Errorf("paginator must not render when StoryTotal <= PageSize")
	}
	if !strings.Contains(bodyFew, `data-testid="panel-stories-count">(3)`) {
		t.Errorf("count chip should report substrate total (3)")
	}
}

// extractProjectID is a tiny helper for the TestProjectWorkspaceRender_*
// suite — pulls the proj_<8hex> out of the rendered project_detail body
// so absent-affordance assertions can reference the live id without
// threading it through the harness.
func extractProjectID(body string) string {
	const prefix = `data-project-id="`
	idx := strings.Index(body, prefix)
	if idx < 0 {
		return ""
	}
	end := strings.Index(body[idx+len(prefix):], `"`)
	if end < 0 {
		return ""
	}
	return body[idx+len(prefix) : idx+len(prefix)+end]
}

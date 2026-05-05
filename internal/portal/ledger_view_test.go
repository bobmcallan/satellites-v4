package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
)

func renderLedgerJSON(t *testing.T, p *Portal, projID, sessionCookie, query string) (*httptest.ResponseRecorder, ledgerComposite) {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	u := "/projects/" + projID + "/api/ledger"
	if query != "" {
		u += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var c ledgerComposite
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &c)
	}
	return rec, c
}

func TestParseLedgerFilters_QueryString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want ledgerFilters
	}{
		{"", ledgerFilters{Tags: []string{}}},
		{"q=foo", ledgerFilters{Query: "foo", Tags: []string{}}},
		{"type=verdict&durability=durable&source_type=agent&status=active",
			ledgerFilters{Type: "verdict", Durability: "durable", SourceType: "agent", Status: "active", Tags: []string{}}},
		{"tag=kind:plan&tag=phase:plan", ledgerFilters{Tags: []string{"kind:plan", "phase:plan"}}},
		{"tag=a,b,c", ledgerFilters{Tags: []string{"a", "b", "c"}}},
		{"story_id=sty_x", ledgerFilters{StoryID: "sty_x", Tags: []string{}}},
	}
	for _, c := range cases {
		u, _ := url.Parse("/x?" + c.raw)
		r := &http.Request{URL: u}
		got := parseLedgerFilters(r)
		// Normalise tag nil vs [].
		if got.Tags == nil {
			got.Tags = []string{}
		}
		if c.want.Tags == nil {
			c.want.Tags = []string{}
		}
		if got.Query != c.want.Query || got.Type != c.want.Type || got.Durability != c.want.Durability ||
			got.SourceType != c.want.SourceType || got.Status != c.want.Status ||
			got.StoryID != c.want.StoryID ||
			!sameStringSlice(got.Tags, c.want.Tags) {
			t.Errorf("parseLedgerFilters(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProjectLedger_HeaderRendersUpgrade(t *testing.T) {
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
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="ledger-header"`,
		`data-testid="ledger-sidebar"`,
		`data-testid="ledger-search"`,
		`data-testid="tail-toggle"`,
		`data-testid="filter-type"`,
		`data-testid="filter-durability"`,
		`data-testid="filter-source"`,
		`data-testid="filter-status"`,
		`/static/ledger_view.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("upgraded ledger body missing %q", want)
		}
	}
}

func TestLedgerJSON_FiltersByTag(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, ledgerStore, _ := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())
	now := time.Now().UTC()
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{
		ProjectID:  proj.ID,
		Type:       ledger.TypeVerdict,
		Tags:       []string{"kind:verdict", "phase:plan"},
		Content:    "approved",
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceSystem,
		Status:     ledger.StatusActive,
	}, now)
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{
		ProjectID:  proj.ID,
		Type:       ledger.TypeArtifact,
		Tags:       []string{"kind:artifact"},
		Content:    "artifact-row",
		Durability: ledger.DurabilityPipeline,
		SourceType: ledger.SourceAgent,
		Status:     ledger.StatusActive,
	}, now.Add(time.Second))
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	rec, c := renderLedgerJSON(t, p, proj.ID, sess.ID, "tag=kind:verdict")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(c.Rows) != 1 {
		t.Fatalf("rows = %d, want 1; got %+v", len(c.Rows), c.Rows)
	}
	if c.Rows[0].Type != ledger.TypeVerdict {
		t.Errorf("row type = %q, want %q", c.Rows[0].Type, ledger.TypeVerdict)
	}
}

// sty_cdcd66a5: GET /projects/{id}/api/ledger?story_id=sty_<id>
// must return only rows tagged with that story id. The bug
// (filter ignored) leaks unrelated rows to the timeline view —
// reproduce by appending two rows scoped to two different
// stories and asserting the filter cuts to one.
func TestLedgerJSON_FiltersByStoryID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, ledgerStore, stories := newTestPortal(t, &config.Config{Env: "dev"})
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())
	now := time.Now().UTC()

	// Seed two stories so the ID is real (not synthetic).
	a := seedStory(t, stories, proj.ID, "story-a", "x", now)
	b := seedStory(t, stories, proj.ID, "story-b", "x", now)
	aID, bID := a.ID, b.ID
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{
		ProjectID:  proj.ID,
		StoryID:    &aID,
		Type:       ledger.TypeDecision,
		Tags:       []string{"kind:decision"},
		Content:    "row-a",
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceAgent,
		Status:     ledger.StatusActive,
	}, now)
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{
		ProjectID:  proj.ID,
		StoryID:    &bID,
		Type:       ledger.TypeDecision,
		Tags:       []string{"kind:decision"},
		Content:    "row-b",
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceAgent,
		Status:     ledger.StatusActive,
	}, now.Add(time.Second))

	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	rec, c := renderLedgerJSON(t, p, proj.ID, sess.ID, "story_id="+aID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(c.Rows) != 1 {
		t.Fatalf("rows under story_id=%s = %d, want 1; got %+v", aID, len(c.Rows), c.Rows)
	}
	if c.Rows[0].StoryID != aID {
		t.Errorf("row.StoryID = %q, want %q", c.Rows[0].StoryID, aID)
	}
	if c.Rows[0].Content != "row-a" {
		t.Errorf("row content = %q, want %q (cross-story leak)", c.Rows[0].Content, "row-a")
	}
}

// sty_cdcd66a5 cross-owner guard: a story id from another owner's
// project must yield zero rows under workspace-scoped memberships.
// The handler 404s when the route's project_id is owned elsewhere;
// when the project is the caller's but the story_id points at
// rows in a workspace the caller is not a member of, the ledger
// store's memberships scoping cuts the rows.
// sty_cdcd66a5 — the active-filter strip on the ledger page must
// surface a clear-pill for the story_id filter. The pill is bound
// to filters.story_id with x-if so it renders only when the
// filter is non-empty; the click handler is wired to clearStoryID.
func TestLedgerPage_RendersStoryIDFilterPill(t *testing.T) {
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
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="ledger-active-filters"`,
		`data-testid="filter-pill-story-id"`,
		`@click="clearStoryID"`,
		// The pill is x-if'd on filters.story_id so it never renders
		// when the filter is empty; the template literal must be in
		// the page so Alpine can bind on hydration.
		`x-if="filters.story_id"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ledger page missing filter-pill marker %q", want)
		}
	}
}

func TestLedgerView_ClearStoryIDDefined(t *testing.T) {
	t.Parallel()
	source := readCommonJS(t)
	// Sanity: the helper that strips story_id is defined in the
	// ledger_view source, not common.js. Check there.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	raw, err := os.ReadFile(filepath.Join(root, "pages", "static", "ledger_view.js"))
	if err != nil {
		t.Fatalf("read ledger_view.js: %v", err)
	}
	js := string(raw)
	if !strings.Contains(js, "clearStoryID") {
		t.Errorf("ledger_view.js missing clearStoryID handler — pill click would no-op")
	}
	_ = source
}

// sty_2b0ee8b3: when the story_id filter is set, the rows are
// reordered oldest-first (timeline order) so the operator reads
// the story as a narrative.
func TestLedgerComposite_StoryFilterOrdersAscending(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	now := time.Now().UTC()
	storyID := "sty_narrative"
	mk := func(content string, offset time.Duration) {
		_, _ = led.Append(t.Context(), ledger.LedgerEntry{
			ProjectID:  "proj_a",
			StoryID:    &storyID,
			Type:       ledger.TypeDecision,
			Tags:       []string{"kind:plan"},
			Content:    content,
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
		}, now.Add(offset))
	}
	mk("event-1-oldest", 1*time.Second)
	mk("event-2-middle", 2*time.Second)
	mk("event-3-newest", 3*time.Second)

	// With story filter → ascending.
	c := buildLedgerComposite(t.Context(), led, "proj_a",
		ledgerFilters{StoryID: storyID}, nil)
	if len(c.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(c.Rows))
	}
	if c.Rows[0].Content != "event-1-oldest" {
		t.Errorf("Rows[0].Content = %q, want oldest first under timeline mode", c.Rows[0].Content)
	}
	if c.Rows[2].Content != "event-3-newest" {
		t.Errorf("Rows[2].Content = %q, want newest last under timeline mode", c.Rows[2].Content)
	}

	// Without story filter → store default (newest first).
	c2 := buildLedgerComposite(t.Context(), led, "proj_a",
		ledgerFilters{}, nil)
	if len(c2.Rows) != 3 {
		t.Fatalf("project-wide rows = %d, want 3", len(c2.Rows))
	}
	if c2.Rows[0].Content != "event-3-newest" {
		t.Errorf("Rows[0].Content = %q, want newest first under feed mode", c2.Rows[0].Content)
	}
}

// sty_2b0ee8b3: each row carries kind_class + kind_label derived
// from the primary kind:<x> tag, plus structured payload extracts
// (verdict outcome, status-change from→to). The template uses
// these to render distinct chrome per event.
func TestLedgerRowView_PerKindMetadata(t *testing.T) {
	t.Parallel()
	led := ledger.NewMemoryStore()
	now := time.Now().UTC()
	storyID := "sty_kinds"

	type evt struct {
		tags       []string
		structured string
		content    string
	}
	rows := []evt{
		{tags: []string{"kind:plan"}, content: "plan body"},
		{tags: []string{"kind:evidence", "task_id:task_evdev", "phase:develop"}, content: "evidence body"},
		{tags: []string{"kind:verdict"}, content: "rationale body",
			structured: `{"verdict":"rejected","reasoning":"AC#3 missing"}`},
		{tags: []string{"kind:story.status_change"}, content: "",
			structured: `{"from":"in_progress","to":"done"}`},
		{tags: []string{"kind:operator-override"}, content: "override body"},
		{tags: []string{}, content: "no kind"},
	}
	for i, r := range rows {
		entry := ledger.LedgerEntry{
			ProjectID:  "proj_a",
			StoryID:    &storyID,
			Type:       ledger.TypeDecision,
			Tags:       r.tags,
			Content:    r.content,
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceAgent,
			Status:     ledger.StatusActive,
		}
		if r.structured != "" {
			entry.Structured = []byte(r.structured)
		}
		_, _ = led.Append(t.Context(), entry, now.Add(time.Duration(i)*time.Second))
	}

	c := buildLedgerComposite(t.Context(), led, "proj_a",
		ledgerFilters{StoryID: storyID}, nil)
	if len(c.Rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(c.Rows))
	}
	classes := make([]string, len(c.Rows))
	for i, r := range c.Rows {
		classes[i] = r.KindClass
	}
	want := []string{"plan", "evidence", "verdict", "status-change", "operator-override", "raw"}
	for i := range want {
		if classes[i] != want[i] {
			t.Errorf("Rows[%d].KindClass = %q, want %q", i, classes[i], want[i])
		}
	}

	// Per-kind extracts.
	if c.Rows[1].TaskID != "task_evdev" {
		t.Errorf("evidence row TaskID = %q, want task_evdev", c.Rows[1].TaskID)
	}
	if c.Rows[1].Phase != "develop" {
		t.Errorf("evidence row Phase = %q, want develop", c.Rows[1].Phase)
	}
	if c.Rows[2].VerdictOutcome != "rejected" {
		t.Errorf("verdict row VerdictOutcome = %q, want rejected", c.Rows[2].VerdictOutcome)
	}
	if c.Rows[2].VerdictReasoning != "AC#3 missing" {
		t.Errorf("verdict row VerdictReasoning = %q, want AC#3 missing", c.Rows[2].VerdictReasoning)
	}
	if c.Rows[3].StatusChangeFrom != "in_progress" || c.Rows[3].StatusChangeTo != "done" {
		t.Errorf("status-change row = (%q→%q), want (in_progress→done)",
			c.Rows[3].StatusChangeFrom, c.Rows[3].StatusChangeTo)
	}
	if c.Rows[5].KindLabel == "" {
		t.Errorf("raw row KindLabel must fall back to row Type, got empty")
	}
}

// sty_2b0ee8b3: SSR carries the timeline title block + per-kind
// chrome (kind-pill, verdict-pill, status-change-pill).
func TestLedgerPage_RendersTimelineForFilteredStory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, ledgerStore, stories := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	now := time.Now().UTC()
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", now)
	s := seedStory(t, stories, proj.ID, "narrative", "x", now)
	storyID := s.ID
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{
		ProjectID: proj.ID, StoryID: &storyID, Type: ledger.TypeVerdict,
		Tags: []string{"kind:verdict", "phase:develop"}, Content: "rejected — see reasoning",
		Structured: []byte(`{"verdict":"rejected","reasoning":"AC#3 missing"}`),
		Durability: ledger.DurabilityDurable, SourceType: ledger.SourceAgent, Status: ledger.StatusActive,
	}, now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/ledger?story_id="+storyID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`'ledger-timeline-title' : 'ledger-rows-title'`,
		`kind-verdict`,
		`verdict-rejected`,
		// SSR <noscript> path renders the verdict reasoning excerpt.
		`AC#3 missing`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline body missing marker %q", want)
		}
	}
}

func TestLedgerJSON_StoryIDCrossOwnerYieldsZero(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, users, sessions, projects, ledgerStore, stories := newTestPortal(t, &config.Config{Env: "dev"})
	alice := auth.User{ID: "u_alice", Email: "alice@local"}
	bob := auth.User{ID: "u_bob", Email: "bob@local"}
	users.Add(alice)
	users.Add(bob)

	now := time.Now().UTC()
	aliceProj, _ := projects.Create(ctx, alice.ID, "", "alpha", now)
	bobProj, _ := projects.Create(ctx, bob.ID, "", "bravo", now)
	bobStory := seedStory(t, stories, bobProj.ID, "bobs-secret", "x", now)
	bobStoryID := bobStory.ID
	_, _ = ledgerStore.Append(ctx, ledger.LedgerEntry{
		ProjectID:  bobProj.ID,
		StoryID:    &bobStoryID,
		Type:       ledger.TypeDecision,
		Tags:       []string{"kind:decision"},
		Content:    "bob-private",
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceAgent,
		Status:     ledger.StatusActive,
	}, now)

	// Alice queries her own project but supplies bob's story id.
	// Cross-project leakage would surface as a row rendered here.
	sess, _ := sessions.Create(alice.ID, auth.DefaultSessionTTL)
	rec, c := renderLedgerJSON(t, p, aliceProj.ID, sess.ID, "story_id="+bobStoryID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (alice on her own project)", rec.Code)
	}
	if len(c.Rows) != 0 {
		t.Fatalf("rows leaked across owners: got %d (want 0); rows=%+v", len(c.Rows), c.Rows)
	}

	// Bob requesting alice's project gets 404 (owner check on the
	// route's project_id; story_id never reaches the filter path).
	bobSess, _ := sessions.Create(bob.ID, auth.DefaultSessionTTL)
	rec2, _ := renderLedgerJSON(t, p, aliceProj.ID, bobSess.ID, "story_id="+bobStoryID)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("bob on alice's project: status = %d, want 404", rec2.Code)
	}
}

func TestLedgerRedirect_NoProjectSendsToList(t *testing.T) {
	t.Parallel()
	p, users, sessions, _, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/ledger", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if rec.Header().Get("Location") != "/projects" {
		t.Errorf("Location = %q, want /projects", rec.Header().Get("Location"))
	}
}

func TestLedgerRedirect_PicksFirstProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, users, sessions, projects, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)
	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	proj, _ := projects.Create(ctx, user.ID, "", "alpha", time.Now().UTC())
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)
	req := httptest.NewRequest(http.MethodGet, "/ledger", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	want := "/projects/" + proj.ID + "/ledger"
	if rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}

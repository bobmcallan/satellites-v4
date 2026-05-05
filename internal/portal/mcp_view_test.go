// Tests for the MCP catalogue page (sty_cd8b89c6). The page renders
// the boot-snapshot artifact 1:1 — name, description, per-parameter
// schema. The negative test below pins that the rendered HTML carries
// no fields beyond those, so future embellishment fails loudly.
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
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/mcpserver"
)

// mcpFixture builds a portal + signed-in alice for /mcp page tests.
type mcpFixture struct {
	t        *testing.T
	portal   *Portal
	docs     *document.MemoryStore
	sessions *auth.MemorySessionStore
	users    *auth.MemoryUserStore
	mux      *http.ServeMux
	user     auth.User
	sessID   string
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	p, users, sessions, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	return &mcpFixture{
		t:        t,
		portal:   p,
		docs:     docs,
		sessions: sessions,
		users:    users,
		mux:      mux,
		user:     user,
		sessID:   sess.ID,
	}
}

func (f *mcpFixture) request(t *testing.T, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: f.sessID})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *mcpFixture) seedCatalogue(t *testing.T, cat mcpserver.Catalogue) {
	t.Helper()
	body, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		t.Fatalf("marshal catalogue: %v", err)
	}
	if _, err := f.docs.Create(context.Background(), document.Document{
		Type:   document.TypeArtifact,
		Scope:  document.ScopeSystem,
		Name:   mcpserver.CatalogueArtifactName,
		Body:   string(body),
		Tags:   []string{mcpserver.CatalogueKindTag},
		Status: document.StatusActive,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}
}

func TestMCPPage_EmptyStateWhenNoSnapshot(t *testing.T) {
	t.Parallel()
	f := newMCPFixture(t)
	rec := f.request(t, "/mcp")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="mcp-page"`,
		`data-testid="mcp-empty"`,
		`no snapshot yet`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, mustNot := range []string{
		`data-testid="mcp-tool-row-`,
		`data-testid="mcp-group-`,
	} {
		if strings.Contains(body, mustNot) {
			t.Errorf("empty-state path should not contain %q", mustNot)
		}
	}
}

func TestMCPPage_RendersGroupedTools(t *testing.T) {
	t.Parallel()
	f := newMCPFixture(t)
	f.seedCatalogue(t, mcpserver.Catalogue{
		SnapshotAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		Tools: []mcpserver.CatalogueEntry{
			{
				Name:        "story_get",
				Description: "Return a story by id.",
				Parameters: []mcpserver.CatalogueParam{
					{Name: "id", Type: "string", Required: true, Description: "Story id."},
				},
			},
			{
				Name:        "task_walk",
				Description: "Return the task chain for a story.",
				Parameters: []mcpserver.CatalogueParam{
					{Name: "story_id", Type: "string", Required: true, Description: "Story whose walk should be returned."},
				},
			},
		},
	})
	rec := f.request(t, "/mcp")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="mcp-tool-count"`,
		`(2 tools)`,
		`2026-05-06T12:00:00Z`,
		`data-testid="mcp-group-story"`,
		`data-testid="mcp-group-task"`,
		`data-testid="mcp-tool-row-story_get"`,
		`data-testid="mcp-tool-row-task_walk"`,
		`data-testid="mcp-tool-name"`,
		`data-testid="mcp-tool-description"`,
		`data-testid="mcp-tool-parameters"`,
		`data-testid="mcp-param-id"`,
		`data-testid="mcp-param-story_id"`,
		`Return a story by id.`,
		`Return the task chain for a story.`,
		`Story id.`,
		`required`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestMCPPage_NoExtraFields locks in the AC's negative test: the
// rendered tool surface carries only name + description + per-param
// schema. If a future change adds a return-shape sidecar, an example,
// or any operator-only annotation, this test fails — drift between
// the page and what Claude reads is the thing to keep out.
func TestMCPPage_NoExtraFields(t *testing.T) {
	t.Parallel()
	f := newMCPFixture(t)
	f.seedCatalogue(t, mcpserver.Catalogue{
		SnapshotAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		Tools: []mcpserver.CatalogueEntry{
			{Name: "story_get", Description: "desc", Parameters: []mcpserver.CatalogueParam{{Name: "id", Type: "string", Required: true}}},
		},
	})
	rec := f.request(t, "/mcp")
	body := rec.Body.String()
	for _, mustNot := range []string{
		"return shape",
		"return-shape",
		"example",
		"sidecar",
		"output schema",
		"outputSchema",
		"annotation",
		`data-testid="mcp-tool-output`,
		`data-testid="mcp-tool-example`,
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(mustNot)) {
			t.Errorf("page surface contains forbidden field %q — only name/description/parameters belong on /mcp", mustNot)
		}
	}
}

// TestBuildMCPComposite_VisibleAcrossWorkspaces locks in the
// system-scope read semantics. The catalogue lives in the system
// workspace; an operator whose memberships do not include that
// workspace must still see it. Reproduces the production bug where
// /mcp rendered the empty-state copy after deploy because the
// handler was passing the operator's memberships into GetByName,
// filtering the system-scope artifact out.
func TestBuildMCPComposite_VisibleAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	cat := mcpserver.Catalogue{
		SnapshotAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		Tools:      []mcpserver.CatalogueEntry{{Name: "story_get", Description: "x", Parameters: []mcpserver.CatalogueParam{{Name: "id", Type: "string", Required: true}}}},
	}
	body, _ := json.Marshal(cat)
	if _, err := docs.Create(context.Background(), document.Document{
		WorkspaceID: "wksp_system",
		Type:        document.TypeArtifact,
		Scope:       document.ScopeSystem,
		Name:        mcpserver.CatalogueArtifactName,
		Body:        string(body),
		Tags:        []string{mcpserver.CatalogueKindTag},
		Status:      document.StatusActive,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}
	composite := buildMCPComposite(context.Background(), docs)
	if !composite.HasSnapshot {
		t.Fatalf("system-scope catalogue not visible — operator memberships should not gate this read")
	}
	if composite.ToolCount != 1 {
		t.Errorf("ToolCount = %d, want 1", composite.ToolCount)
	}
}

// TestNav_HasMCPLink covers the AC: desktop hamburger nav carries an
// MCP link routing to /mcp.
func TestNav_HasMCPLink(t *testing.T) {
	t.Parallel()
	f := newMCPFixture(t)
	rec := f.request(t, "/mcp")
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="nav-mcp-link"`) {
		t.Errorf("desktop nav missing MCP link")
	}
	if !strings.Contains(body, `href="/mcp"`) {
		t.Errorf("MCP link missing href=\"/mcp\"")
	}
}

// TestNav_HasMobileMCPLink covers mobile parity.
func TestNav_HasMobileMCPLink(t *testing.T) {
	t.Parallel()
	f := newMCPFixture(t)
	rec := f.request(t, "/mcp")
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="nav-mobile-link-mcp"`) {
		t.Errorf("mobile nav missing MCP link")
	}
}

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
)

// helpFixture builds a portal + signed-in alice for help-page tests.
type helpFixture struct {
	t        *testing.T
	portal   *Portal
	docs     *document.MemoryStore
	sessions *auth.MemorySessionStore
	users    *auth.MemoryUserStore
	mux      *http.ServeMux
	user     auth.User
	sessID   string
}

func newHelpFixture(t *testing.T) *helpFixture {
	t.Helper()
	p, users, sessions, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	return &helpFixture{
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

func (f *helpFixture) request(t *testing.T, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: f.sessID})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *helpFixture) seedHelp(slug, title string, order int, body string) {
	f.t.Helper()
	structured := []byte(`{"title":"` + title + `","slug":"` + slug + `","order":` + intToString(order) + `}`)
	if _, err := f.docs.Create(context.Background(), document.Document{
		Type:       document.TypeHelp,
		Scope:      document.ScopeSystem,
		Name:       slug,
		Body:       body,
		Structured: structured,
		Status:     document.StatusActive,
	}, time.Now().UTC()); err != nil {
		f.t.Fatalf("seed help %s: %v", slug, err)
	}
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// TestHelpIndex_RendersTitlesOrdered covers AC1: index lists titles
// ordered by frontmatter order ascending.
func TestHelpIndex_RendersTitlesOrdered(t *testing.T) {
	t.Parallel()
	f := newHelpFixture(t)
	f.seedHelp("agents", "Agents", 20, "# Agents\n\nbody")
	f.seedHelp("index", "Welcome", 0, "# Welcome\n\nbody")
	f.seedHelp("contracts", "Contracts", 30, "# Contracts\n\nbody")

	rec := f.request(t, "/help")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="help-panel"`,
		`data-testid="help-list"`,
		`data-testid="help-entry-index"`,
		`data-testid="help-entry-agents"`,
		`data-testid="help-entry-contracts"`,
		"Welcome",
		"Agents",
		"Contracts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	idxIndex := strings.Index(body, `data-testid="help-entry-index"`)
	idxAgents := strings.Index(body, `data-testid="help-entry-agents"`)
	idxContracts := strings.Index(body, `data-testid="help-entry-contracts"`)
	if !(idxIndex < idxAgents && idxAgents < idxContracts) {
		t.Errorf("entries out of order: index=%d agents=%d contracts=%d", idxIndex, idxAgents, idxContracts)
	}
}

// TestHelpIndex_EmptyState covers AC1: an empty store renders the
// "no help pages yet" message instead of a 500.
func TestHelpIndex_EmptyState(t *testing.T) {
	t.Parallel()
	f := newHelpFixture(t)
	rec := f.request(t, "/help")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-testid="help-empty"`) {
		t.Errorf("empty state missing")
	}
}

// TestHelpDetail_RendersHeading covers AC2: detail page renders the
// rendered markdown body — heading present.
func TestHelpDetail_RendersHeading(t *testing.T) {
	t.Parallel()
	f := newHelpFixture(t)
	f.seedHelp("agents", "Agents", 20, "# Agents\n\nan agent is a bundle of permissions.\n")

	rec := f.request(t, "/help/agents")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="help-detail-panel"`,
		`data-testid="help-detail-title">Agents`,
		`<h1>Agents</h1>`,
		`an agent is a bundle of permissions.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestHelpDetail_404OnUnknownSlug covers AC2: unknown slug → 404.
func TestHelpDetail_404OnUnknownSlug(t *testing.T) {
	t.Parallel()
	f := newHelpFixture(t)
	rec := f.request(t, "/help/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestNav_HasHelpLink covers AC3: nav contains the HELP link.
func TestNav_HasHelpLink(t *testing.T) {
	t.Parallel()
	f := newHelpFixture(t)
	rec := f.request(t, "/help")
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="nav-help-link"`) {
		t.Errorf("nav missing HELP link")
	}
}

// sty_5b92165a — desktop nav carries the LEDGER link routing to
// /ledger (which redirects to the active project's ledger page).
// Mobile nav already has the equivalent entry; this covers the
// missing top-bar parity.
func TestNav_HasLedgerLink(t *testing.T) {
	t.Parallel()
	f := newHelpFixture(t)
	rec := f.request(t, "/help")
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="nav-ledger-link"`) {
		t.Errorf("desktop nav missing LEDGER link (sty_5b92165a)")
	}
	if !strings.Contains(body, `href="/ledger"`) {
		t.Errorf("LEDGER link missing href=\"/ledger\"")
	}
}

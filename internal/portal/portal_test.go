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
)

func newTestPortal(t *testing.T, cfg *config.Config) (*Portal, *auth.MemoryUserStore, *auth.MemorySessionStore) {
	t.Helper()
	users := auth.NewMemoryUserStore()
	sessions := auth.NewMemorySessionStore()
	p, err := New(cfg, satarbor.New("info"), sessions, users, time.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, users, sessions
}

func TestLanding_UnauthRedirects(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPortal(t, &config.Config{Env: "dev"})
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
	p, users, sessions := newTestPortal(t, &config.Config{Env: "dev"})
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
	p, _, _ := newTestPortal(t, cfg)
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
	p, _, _ := newTestPortal(t, cfg)
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
	p, _, _ := newTestPortal(t, cfg)
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

func TestHead_HasBlockingThemeScript(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPortal(t, &config.Config{Env: "dev", DevMode: true})
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

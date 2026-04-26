package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/config"
)

func newTestHandlers(t *testing.T, cfg *config.Config) (*Handlers, *MemoryUserStore, *MemorySessionStore) {
	t.Helper()
	users := NewMemoryUserStore()
	sessions := NewMemorySessionStore()
	h := &Handlers{
		Users:    users,
		Sessions: sessions,
		Logger:   satarbor.New("info"),
		Cfg:      cfg,
	}
	return h, users, sessions
}

func seedUser(t *testing.T, users *MemoryUserStore, email, password string) User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u := User{ID: "u_" + email, Email: email, DisplayName: email, Provider: "local", HashedPassword: hash}
	users.Add(u)
	return u
}

func postLogin(t *testing.T, h *Handlers, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	return rec
}

func TestLogin_Happy(t *testing.T) {
	t.Parallel()
	h, users, _ := newTestHandlers(t, &config.Config{Env: "dev"})
	seedUser(t, users, "alice@local", "s3cr3t")

	rec := postLogin(t, h, "alice@local", "s3cr3t")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie to be set")
	}
	if sessionCookie.Value == "" {
		t.Fatal("session cookie value is empty")
	}
}

func TestLogin_BadPassword(t *testing.T) {
	t.Parallel()
	h, users, _ := newTestHandlers(t, &config.Config{Env: "dev"})
	seedUser(t, users, "alice@local", "s3cr3t")

	rec := postLogin(t, h, "alice@local", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestLogin_MissingUser(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHandlers(t, &config.Config{Env: "dev"})
	rec := postLogin(t, h, "nope@local", "anything")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// stubLimiter denies on Allow once threshold requests have been served.
type stubLimiter struct {
	allowed int
	max     int
}

func (s *stubLimiter) Allow(_ string) bool {
	if s.allowed >= s.max {
		return false
	}
	s.allowed++
	return true
}

// TestLogin_RateLimited429 covers AC3 of story_d5652302: requests in
// excess of the per-IP threshold must return 429 before the credential
// path runs.
func TestLogin_RateLimited429(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev"}
	h, users, _ := newTestHandlers(t, cfg)
	seedUser(t, users, "alice@local", "s3cr3t")
	h.LoginLimiter = &stubLimiter{max: 3}

	for i := 0; i < 3; i++ {
		rec := postLogin(t, h, "alice@local", "wrong")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d throttled too soon (under threshold)", i)
		}
	}
	rec := postLogin(t, h, "alice@local", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-threshold status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too many") {
		t.Errorf("body = %q, want 'too many requests' message", rec.Body.String())
	}
}

func TestLogin_DevMode_DevEnvAllowed(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true, DevUsername: "dev@local", DevPassword: "devpass"}
	h, _, _ := newTestHandlers(t, cfg)
	rec := postLogin(t, h, "dev@local", "devpass")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("DevMode dev status = %d, want 303", rec.Code)
	}
}

func TestLogin_DevMode_ProdRejected(t *testing.T) {
	t.Parallel()
	// DEV_MODE true but ENV prod — DevMode must be disabled.
	cfg := &config.Config{Env: "prod", DevMode: true, DevUsername: "dev@local", DevPassword: "devpass", DBDSN: "x"}
	h, _, _ := newTestHandlers(t, cfg)
	rec := postLogin(t, h, "dev@local", "devpass")
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("DevMode accepted in prod; must be rejected")
	}
}

func TestLogin_SessionExpiry(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionStore()
	store.clock = func() time.Time { return time.Unix(0, 0) }
	sess, err := store.Create("u_x", 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Advance the clock past expiry.
	store.clock = func() time.Time { return time.Unix(10, 0) }
	if _, err := store.Get(sess.ID); err != ErrSessionNotFound {
		t.Fatalf("Get expired session = %v, want ErrSessionNotFound", err)
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	t.Parallel()
	h, users, sessions := newTestHandlers(t, &config.Config{Env: "dev"})
	u := seedUser(t, users, "alice@local", "s3cr3t")
	sess, _ := sessions.Create(u.ID, DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	h.Logout(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("logout Location = %q, want /login", loc)
	}
	// Session row removed.
	if _, err := sessions.Get(sess.ID); err == nil {
		t.Error("logout did not delete session row")
	}
	// Response clears cookie (MaxAge -1).
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not set a cookie-clearing header")
	}
}

func TestRequireSession_RedirectsWhenUnauth(t *testing.T) {
	t.Parallel()
	users := NewMemoryUserStore()
	sessions := NewMemorySessionStore()
	mw := RequireSession(sessions, users, CookieOptions{})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unauth should not reach the wrapped handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/secure?x=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "/login?next=" + url.QueryEscape("/secure?x=1")
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}

func TestRequireSession_AcceptsValidSession(t *testing.T) {
	t.Parallel()
	users := NewMemoryUserStore()
	sessions := NewMemorySessionStore()
	u := User{ID: "u_1", Email: "alice@local"}
	users.Add(u)
	sess, _ := sessions.Create(u.ID, DefaultSessionTTL)

	mw := RequireSession(sessions, users, CookieOptions{})
	var seen User
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/secure", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen.ID != u.ID {
		t.Errorf("user on ctx = %+v, want id=%q", seen, u.ID)
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	t.Parallel()
	hash, err := HashPassword("correct")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(hash, "correct"); err != nil {
		t.Errorf("VerifyPassword(correct) = %v", err)
	}
	if err := VerifyPassword(hash, "wrong"); err != ErrPasswordMismatch {
		t.Errorf("VerifyPassword(wrong) = %v, want ErrPasswordMismatch", err)
	}
}

package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
)

func newAuthTestDeps() AuthDeps {
	return AuthDeps{
		Sessions: auth.NewMemorySessionStore(),
		Users:    auth.NewMemoryUserStore(),
		APIKeys:  []string{"key_valid"},
		Logger:   satarbor.New("info"),
	}
}

func TestAuth_UnauthRejected(t *testing.T) {
	t.Parallel()
	mw := AuthMiddleware(newAuthTestDeps())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler without auth")
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header")
	}
}

func TestAuth_APIKeyAccepted(t *testing.T) {
	t.Parallel()
	mw := AuthMiddleware(newAuthTestDeps())
	var seen CallerIdentity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer key_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen.Source != "apikey" {
		t.Errorf("source = %q, want apikey", seen.Source)
	}
}

func TestAuth_APIKeyWrongRejected(t *testing.T) {
	t.Parallel()
	mw := AuthMiddleware(newAuthTestDeps())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler with wrong key")
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer key_wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_SessionCookieAccepted(t *testing.T) {
	t.Parallel()
	deps := newAuthTestDeps()
	users := deps.Users.(*auth.MemoryUserStore)
	sessions := deps.Sessions.(*auth.MemorySessionStore)
	user := auth.User{ID: "u_1", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	mw := AuthMiddleware(deps)
	var seen CallerIdentity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen.Email != "alice@local" {
		t.Errorf("email = %q, want alice@local", seen.Email)
	}
	if seen.Source != "session" {
		t.Errorf("source = %q, want session", seen.Source)
	}
}

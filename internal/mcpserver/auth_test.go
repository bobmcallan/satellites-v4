package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// TestAuth_OAuthBearerAccepted (story_512cc5cd AC1+AC2) — when an
// OAuthValidator is wired and an Authorization: Bearer is presented that
// neither matches an API key nor a session cookie, the validator gets
// asked. A successful provider validation populates CallerIdentity with
// source="oauth:<provider>".
func TestAuth_OAuthBearerAccepted(t *testing.T) {
	t.Parallel()
	stub, _, _ := newAuthStub(t)
	defer stub.Close()

	deps := newAuthTestDeps()
	deps.OAuthValidator = auth.NewBearerValidator(auth.BearerValidatorConfig{
		CacheTTL:          time.Minute,
		GoogleUserinfoURL: stub.URL + "/google",
		GithubUserURL:     stub.URL + "/github",
	})

	mw := AuthMiddleware(deps)
	var seen CallerIdentity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))

	for _, tc := range []struct {
		token        string
		wantProvider string
	}{
		{"google-good", "google"},
		{"github-good", "github"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
			// 200 if handler ran, 404 if no body — either way the auth path
			// passed. 401 means auth rejected.
			t.Errorf("token %q: status = %d, want pass-through", tc.token, rec.Code)
		}
		if seen.Source != "oauth:"+tc.wantProvider {
			t.Errorf("token %q: source = %q, want oauth:%s", tc.token, seen.Source, tc.wantProvider)
		}
	}
}

// TestAuth_OAuthBearerInvalid_401 — invalid bearer + no validator path
// or all paths fail → 401.
func TestAuth_OAuthBearerInvalid_401(t *testing.T) {
	t.Parallel()
	stub, _, _ := newAuthStub(t)
	defer stub.Close()

	deps := newAuthTestDeps()
	deps.OAuthValidator = auth.NewBearerValidator(auth.BearerValidatorConfig{
		CacheTTL:          time.Minute,
		GoogleUserinfoURL: stub.URL + "/google",
		GithubUserURL:     stub.URL + "/github",
	})
	mw := AuthMiddleware(deps)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run for invalid bearer")
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer revoked-or-wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestAuth_SatelliteBearerAccepted — bearers minted via
// IssueSatelliteBearer (the /auth/token/exchange path) authenticate /mcp.
// TestAuth_BearerStorePath_Owner covers AC5: a Bearer matching an
// active row in the substrate-managed APIKeyStore resolves as the
// owner identity with Source = "apikey:<id>". This is the new
// store-backed path; the env-var keyset path stays untouched.
func TestAuth_BearerStorePath_Owner(t *testing.T) {
	t.Parallel()
	deps := newAuthTestDeps()
	users := deps.Users.(*auth.MemoryUserStore)
	owner := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(owner)

	store := auth.NewMemoryAgentAPIKeyStore()
	cleartext, salt, _ := auth.GenerateAPIKey()
	row := auth.APIKey{
		ID:          "apk_test01",
		WorkspaceID: "wksp_test",
		OwnerUserID: owner.ID,
		Name:        "alice's-laptop",
		Prefix:      auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:     auth.HashAPIKey(salt, cleartext),
		KeySalt:     salt,
		Status:      auth.APIKeyStatusActive,
		CreatedAt:   time.Now().UTC(),
	}
	_ = store.Create(context.Background(), row)
	deps.APIKeyStore = store

	mw := AuthMiddleware(deps)
	var seen CallerIdentity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+cleartext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen.Source != "apikey:apk_test01" {
		t.Errorf("Source = %q, want apikey:apk_test01", seen.Source)
	}
	if seen.Email != "alice@local" {
		t.Errorf("Email = %q, want alice@local", seen.Email)
	}
	if seen.UserID != "u_alice" {
		t.Errorf("UserID = %q, want u_alice", seen.UserID)
	}
}

// TestAuth_BearerStorePath_ExpiredKey covers AC5: an expired row
// MUST NOT authenticate. The middleware rejects with 401 and the
// downstream handler is never invoked.
func TestAuth_BearerStorePath_ExpiredKey(t *testing.T) {
	t.Parallel()
	deps := newAuthTestDeps()
	store := auth.NewMemoryAgentAPIKeyStore()
	cleartext, salt, _ := auth.GenerateAPIKey()
	past := time.Now().UTC().Add(-time.Hour)
	row := auth.APIKey{
		ID:          "apk_expired1",
		OwnerUserID: "u_alice",
		Prefix:      auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:     auth.HashAPIKey(salt, cleartext),
		KeySalt:     salt,
		Status:      auth.APIKeyStatusActive,
		ExpiresAt:   &past,
		CreatedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}
	_ = store.Create(context.Background(), row)
	deps.APIKeyStore = store

	mw := AuthMiddleware(deps)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler with expired key")
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+cleartext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestAuth_BearerStorePath_ArchivedKey covers AC5: an archived row
// MUST NOT authenticate. Soft-deleted keys cannot be revived by the
// caller still possessing the cleartext.
func TestAuth_BearerStorePath_ArchivedKey(t *testing.T) {
	t.Parallel()
	deps := newAuthTestDeps()
	store := auth.NewMemoryAgentAPIKeyStore()
	cleartext, salt, _ := auth.GenerateAPIKey()
	row := auth.APIKey{
		ID:          "apk_archived1",
		OwnerUserID: "u_alice",
		Prefix:      auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:     auth.HashAPIKey(salt, cleartext),
		KeySalt:     salt,
		Status:      auth.APIKeyStatusArchived,
		CreatedAt:   time.Now().UTC(),
	}
	_ = store.Create(context.Background(), row)
	deps.APIKeyStore = store

	mw := AuthMiddleware(deps)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler with archived key")
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+cleartext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestAuth_BearerStorePath_TouchFireAndForget covers AC6: the
// AuthMiddleware Touch call MUST NOT block the request. The test
// wires a Touch implementation that always errors, fires a request,
// asserts the inner handler runs and the response succeeds.
func TestAuth_BearerStorePath_TouchFireAndForget(t *testing.T) {
	t.Parallel()
	deps := newAuthTestDeps()
	users := deps.Users.(*auth.MemoryUserStore)
	users.Add(auth.User{ID: "u_alice", Email: "alice@local"})
	store := &touchErrAPIKeyStore{
		MemoryAgentAPIKeyStore: auth.NewMemoryAgentAPIKeyStore(),
	}
	cleartext, salt, _ := auth.GenerateAPIKey()
	row := auth.APIKey{
		ID:          "apk_touchfail1",
		OwnerUserID: "u_alice",
		Prefix:      auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:     auth.HashAPIKey(salt, cleartext),
		KeySalt:     salt,
		Status:      auth.APIKeyStatusActive,
		CreatedAt:   time.Now().UTC(),
	}
	_ = store.Create(context.Background(), row)
	deps.APIKeyStore = store

	mw := AuthMiddleware(deps)
	reached := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+cleartext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !reached {
		t.Error("Touch error blocked the request — must be fire-and-forget")
	}
}

// touchErrAPIKeyStore wraps MemoryAgentAPIKeyStore so the Touch
// method always errors. Used to assert AuthMiddleware does not
// surface Touch failures.
type touchErrAPIKeyStore struct {
	*auth.MemoryAgentAPIKeyStore
}

func (s *touchErrAPIKeyStore) Touch(ctx context.Context, id string, t time.Time) error {
	return errTouchUnreachable
}

var errTouchUnreachable = errors.New("auth: touch unreachable")

func TestAuth_SatelliteBearerAccepted(t *testing.T) {
	t.Parallel()
	deps := newAuthTestDeps()
	deps.OAuthValidator = auth.NewBearerValidator(auth.BearerValidatorConfig{CacheTTL: time.Minute})
	tok, err := deps.OAuthValidator.IssueSatelliteBearer(auth.BearerInfo{
		UserID: "u_alice", Email: "alice@local", Provider: "satellites",
	}, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	mw := AuthMiddleware(deps)
	var seen CallerIdentity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen.UserID != "u_alice" {
		t.Errorf("UserID = %q, want u_alice", seen.UserID)
	}
	if seen.Source != "oauth:satellites" {
		t.Errorf("Source = %q, want oauth:satellites", seen.Source)
	}
}

func newAuthStub(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	gCalls := &atomic.Int64{}
	hCalls := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/google":
			gCalls.Add(1)
			if token != "google-good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": "1", "email": "alice@example.com"})
		case "/github":
			hCalls.Add(1)
			if token != "github-good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "bob", "email": "bob@example.com"})
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, gCalls, hCalls
}

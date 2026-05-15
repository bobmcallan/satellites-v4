package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// memOAuthStore is the test double for OAuthStore. Concurrent-safe.
type memOAuthStore struct {
	mu       sync.Mutex
	clients  map[string]*OAuthClient
	sessions map[string]*OAuthSession
	codes    map[string]*OAuthCode
	refresh  map[string]*OAuthRefreshToken
}

func newMemOAuthStore() *memOAuthStore {
	return &memOAuthStore{
		clients:  map[string]*OAuthClient{},
		sessions: map[string]*OAuthSession{},
		codes:    map[string]*OAuthCode{},
		refresh:  map[string]*OAuthRefreshToken{},
	}
}

func (s *memOAuthStore) SaveClient(_ context.Context, c *OAuthClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.clients[c.ClientID] = &cp
	return nil
}
func (s *memOAuthStore) GetClient(_ context.Context, id string) (*OAuthClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		return nil, ErrOAuthNotFound
	}
	cp := *c
	return &cp, nil
}
func (s *memOAuthStore) SaveSession(_ context.Context, sess *OAuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.sessions[sess.SessionID] = &cp
	return nil
}
func (s *memOAuthStore) GetSession(_ context.Context, id string) (*OAuthSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, ErrOAuthNotFound
	}
	cp := *sess
	return &cp, nil
}
func (s *memOAuthStore) GetSessionByClientID(_ context.Context, clientID string) (*OAuthSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ClientID == clientID {
			cp := *sess
			return &cp, nil
		}
	}
	return nil, ErrOAuthNotFound
}
func (s *memOAuthStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}
func (s *memOAuthStore) SaveCode(_ context.Context, c *OAuthCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.codes[c.Code] = &cp
	return nil
}
func (s *memOAuthStore) GetCode(_ context.Context, code string) (*OAuthCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	if !ok || c.Used || c.ExpiresAt.Before(time.Now()) {
		return nil, ErrOAuthNotFound
	}
	cp := *c
	return &cp, nil
}
func (s *memOAuthStore) MarkCodeUsed(_ context.Context, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.codes[code]; ok {
		c.Used = true
	}
	return nil
}
func (s *memOAuthStore) SaveRefreshToken(_ context.Context, t *OAuthRefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.refresh[t.Token] = &cp
	return nil
}
func (s *memOAuthStore) GetRefreshToken(_ context.Context, tok string) (*OAuthRefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.refresh[tok]
	if !ok || t.ExpiresAt.Before(time.Now()) {
		return nil, ErrOAuthNotFound
	}
	cp := *t
	return &cp, nil
}
func (s *memOAuthStore) DeleteRefreshToken(_ context.Context, tok string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refresh, tok)
	return nil
}

func newTestServer(t *testing.T) *OAuthServer {
	t.Helper()
	return NewOAuthServer(OAuthServerConfig{
		JWTSecret:       "test-secret-32-bytes-or-more-for-hmac",
		AccessTokenTTL:  1 * time.Hour,
		RefreshTokenTTL: 24 * time.Hour,
		CodeTTL:         10 * time.Minute,
		Store:           newMemOAuthStore(),
		DevMode:         true,
	})
}

func TestOAuth_DiscoveryAS(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("GET", "https://test/.well-known/oauth-authorization-server", nil)
	r.Host = "test"
	w := httptest.NewRecorder()
	srv.HandleAuthorizationServer(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	for _, k := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		if _, ok := body[k]; !ok {
			t.Errorf("metadata missing %q", k)
		}
	}
	pkce, _ := body["code_challenge_methods_supported"].([]any)
	if len(pkce) == 0 || pkce[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want [S256]", pkce)
	}
}

func TestOAuth_DiscoveryProtectedResource(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("GET", "https://test/.well-known/oauth-protected-resource", nil)
	r.Host = "test"
	w := httptest.NewRecorder()
	srv.HandleProtectedResource(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, ok := body["resource"]; !ok {
		t.Errorf("metadata missing resource")
	}
	srvs, _ := body["authorization_servers"].([]any)
	if len(srvs) == 0 {
		t.Errorf("authorization_servers empty")
	}
}

func TestOAuth_DCR(t *testing.T) {
	srv := newTestServer(t)
	body := strings.NewReader(`{"client_name":"test","redirect_uris":["http://localhost:9999/cb"]}`)
	r := httptest.NewRequest("POST", "/oauth/register", body)
	w := httptest.NewRecorder()
	srv.HandleRegister(w, r)
	if w.Code != 201 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got OAuthClient
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ClientID == "" {
		t.Errorf("client_id empty")
	}
	if got.TokenEndpointAuthMethod != "none" {
		t.Errorf("auth_method = %q, want none", got.TokenEndpointAuthMethod)
	}
}

func TestOAuth_FullAuthCodeChain_PKCE(t *testing.T) {
	store := newMemOAuthStore()
	srv := NewOAuthServer(OAuthServerConfig{
		JWTSecret:       "test-secret-32-bytes-or-more-for-hmac",
		AccessTokenTTL:  1 * time.Hour,
		RefreshTokenTTL: 24 * time.Hour,
		CodeTTL:         10 * time.Minute,
		Store:           store,
		DevMode:         true,
	})

	// Pre-register a client.
	clientID := "client-abc"
	redirectURI := "http://localhost:9999/cb"
	_ = store.SaveClient(context.Background(), &OAuthClient{
		ClientID:                clientID,
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	})

	// PKCE pair.
	verifier := "test-verifier-with-enough-entropy-for-pkce-validation"
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	// 1. /oauth/authorize (POST so we can grab the session id directly).
	form := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
	}
	r := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.HandleAuthorize(w, r)
	if w.Code != 200 {
		t.Fatalf("authorize status = %d body=%s", w.Code, w.Body.String())
	}
	var sessResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &sessResp)
	sessionID := sessResp["session_id"]
	if sessionID == "" {
		t.Fatal("session_id empty")
	}

	// 2. Simulate user login → CompleteAuthorization.
	userID := "user_alice"
	redirectURL, err := srv.CompleteAuthorization(context.Background(), sessionID, userID)
	if err != nil {
		t.Fatalf("CompleteAuthorization: %v", err)
	}
	parsed, _ := url.Parse(redirectURL)
	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatal("redirect URL missing code")
	}
	if parsed.Query().Get("state") != "xyz" {
		t.Errorf("state not echoed: %q", parsed.Query().Get("state"))
	}

	// 3. /oauth/token grant_type=authorization_code.
	form = url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	r = httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	srv.HandleToken(w, r)
	if w.Code != 200 {
		t.Fatalf("token status = %d body=%s", w.Code, w.Body.String())
	}
	var tok map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &tok)
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("missing tokens: %+v", tok)
	}
	if !LooksLikeJWT(access) {
		t.Errorf("access_token does not look like JWT: %q", access)
	}

	// 4. Validate the access token.
	claims, err := ValidateJWT(access, srv.JWTSecretBytes())
	if err != nil {
		t.Fatalf("ValidateJWT: %v", err)
	}
	if claims.Sub != userID {
		t.Errorf("claims.Sub = %q, want %q", claims.Sub, userID)
	}

	// 5. Refresh grant rotates token.
	form = url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	}
	r = httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	srv.HandleToken(w, r)
	if w.Code != 200 {
		t.Fatalf("refresh status = %d body=%s", w.Code, w.Body.String())
	}
	var rotated map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	newRefresh, _ := rotated["refresh_token"].(string)
	if newRefresh == "" || newRefresh == refresh {
		t.Errorf("refresh token not rotated: old=%s new=%s", refresh, newRefresh)
	}

	// Old refresh token is gone.
	if _, err := store.GetRefreshToken(context.Background(), refresh); !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("old refresh still present, err=%v", err)
	}
}

// TestOAuth_AccessTokenCarriesIdentityClaims is the regression test for the
// sty_0be97c3e Email-in-JWT fix: when OAuthServerConfig.Users is wired, the
// access-token JWT minted by /oauth/token must carry Email + Name + Provider
// claims so the downstream BearerValidator restores caller.Email on every
// MCP / HTTP request. Without these claims, satellites_info returns
// user_email:"" and satellites_init falls back to auth_login.
func TestOAuth_AccessTokenCarriesIdentityClaims(t *testing.T) {
	store := newMemOAuthStore()
	users := NewMemoryUserStore()
	const userID = "u_google:bobmcallan@gmail.com"
	users.Add(User{ID: userID, Email: "google:bobmcallan@gmail.com", DisplayName: "Bob McAllan", Provider: "google"})
	srv := NewOAuthServer(OAuthServerConfig{
		JWTSecret:       "test-secret-32-bytes-or-more-for-hmac",
		AccessTokenTTL:  1 * time.Hour,
		RefreshTokenTTL: 24 * time.Hour,
		CodeTTL:         10 * time.Minute,
		Store:           store,
		Users:           users,
		DevMode:         true,
	})
	clientID, redirectURI := "client-abc", "http://localhost:9999/cb"
	_ = store.SaveClient(context.Background(), &OAuthClient{
		ClientID:                clientID,
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	})
	verifier := "test-verifier-with-enough-entropy-for-pkce-validation"
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	_ = store.SaveCode(context.Background(), &OAuthCode{
		Code: "C1", ClientID: clientID, UserID: userID, RedirectURI: redirectURI,
		CodeChallenge: challenge, ExpiresAt: time.Now().Add(time.Minute),
	})
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"C1"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.HandleToken(w, r)
	if w.Code != 200 {
		t.Fatalf("token status = %d body=%s", w.Code, w.Body.String())
	}
	var tok map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &tok)
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	claims, err := ValidateJWT(access, srv.JWTSecretBytes())
	if err != nil {
		t.Fatalf("ValidateJWT: %v", err)
	}
	if claims.Sub != userID {
		t.Errorf("claims.Sub = %q, want %q", claims.Sub, userID)
	}
	if claims.Email != "google:bobmcallan@gmail.com" {
		t.Errorf("claims.Email = %q, want %q (regression: empty Email breaks satellites_info.user_email)", claims.Email, "google:bobmcallan@gmail.com")
	}
	if claims.Name != "Bob McAllan" {
		t.Errorf("claims.Name = %q, want %q", claims.Name, "Bob McAllan")
	}
	if claims.Provider != "google" {
		t.Errorf("claims.Provider = %q, want %q", claims.Provider, "google")
	}
	// Refresh path must also carry the identity claims so long-lived
	// sessions don't lose Email on the first refresh.
	rfForm := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	}
	rr := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(rfForm.Encode()))
	rr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	srv.HandleToken(rw, rr)
	if rw.Code != 200 {
		t.Fatalf("refresh status = %d body=%s", rw.Code, rw.Body.String())
	}
	var rotated map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &rotated)
	rotatedAccess, _ := rotated["access_token"].(string)
	rotatedClaims, err := ValidateJWT(rotatedAccess, srv.JWTSecretBytes())
	if err != nil {
		t.Fatalf("ValidateJWT refresh: %v", err)
	}
	if rotatedClaims.Email != "google:bobmcallan@gmail.com" {
		t.Errorf("refresh claims.Email = %q, want preserved across refresh", rotatedClaims.Email)
	}
}

func TestOAuth_PKCEFailureRejected(t *testing.T) {
	store := newMemOAuthStore()
	srv := NewOAuthServer(OAuthServerConfig{
		JWTSecret:       "s",
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: time.Hour,
		CodeTTL:         time.Minute,
		Store:           store,
		DevMode:         true,
	})
	clientID, redirect := "c", "http://l/cb"
	_ = store.SaveClient(context.Background(), &OAuthClient{ClientID: clientID, RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none"})
	_ = store.SaveCode(context.Background(), &OAuthCode{Code: "C1", ClientID: clientID, UserID: "u", RedirectURI: redirect, CodeChallenge: "wrong-challenge", ExpiresAt: time.Now().Add(time.Minute)})

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"C1"},
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
		"code_verifier": {"some-verifier"},
	}
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.HandleToken(w, r)
	if w.Code != 400 {
		t.Fatalf("status = %d (want 400)", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "PKCE") {
		t.Errorf("expected PKCE error in body, got %s", body)
	}
}

func TestOAuth_RejectsNonS256(t *testing.T) {
	srv := newTestServer(t)
	q := url.Values{
		"client_id":             {"c"},
		"redirect_uri":          {"http://l/cb"},
		"response_type":         {"code"},
		"code_challenge":        {"x"},
		"code_challenge_method": {"plain"},
		"state":                 {"s"},
	}
	r := httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	srv.HandleAuthorize(w, r)
	// Expect a redirect with error params.
	if w.Code != 302 {
		t.Fatalf("status = %d (want 302)", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "S256") {
		t.Errorf("redirect %q missing S256 error description", loc)
	}
}

// _ keeps the http import live in test even when no test uses http.X
// directly (some compilers strip otherwise — defensive only).
var _ = http.StatusOK

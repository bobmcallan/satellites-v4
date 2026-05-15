// Package auth's oauth_server.go implements the MCP-spec OAuth 2.1
// Authorization Server (RFC 6749 + 7591 DCR + 7636 PKCE + 8414 AS
// metadata + 9728 protected-resource metadata). Ported from
// satellites-v3 (internal/auth/oauth_server.go) — see
// docs/mcp-oauth-server-port.md for the porting plan and rationale.
//
// The flow:
//
//  1. Client hits /mcp → 401 + WWW-Authenticate: Bearer resource_metadata=…
//  2. Client GETs /.well-known/oauth-protected-resource (HandleProtectedResource)
//  3. Client GETs /.well-known/oauth-authorization-server (HandleAuthorizationServer)
//  4. Client POSTs /oauth/register (HandleRegister) — RFC 7591 DCR
//  5. Client redirects user to GET /oauth/authorize?… (HandleAuthorize)
//  6. OAuthServer creates an OAuthSession, sets mcp_session_id cookie, redirects to /
//  7. User completes browser login (handled by auth.Handlers.Login)
//  8. Login handler detects mcp_session_id, calls CompleteAuthorization
//  9. Client POSTs /oauth/token with code+verifier (HandleToken) — auth-code grant
//
// 10. OAuthServer mints JWT access + opaque refresh, returns JSON
// 11. Client uses Bearer JWT on /mcp
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ternarybob/arbor"
)

const defaultScope = "satellites"

// OAuthServerConfig is what main.go hands NewOAuthServer at boot.
type OAuthServerConfig struct {
	// JWTSecret signs access tokens (HS256). Required; empty fails NewOAuthServer.
	JWTSecret string
	// Issuer is the absolute base URL announced in metadata + JWT iss claim.
	// Empty derives from each request's host + X-Forwarded-Proto.
	Issuer string
	// AccessTokenTTL bounds JWT lifetime. Required.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL bounds refresh token lifetime. Required.
	RefreshTokenTTL time.Duration
	// CodeTTL bounds authorization code lifetime. Required.
	CodeTTL time.Duration
	// Store persists clients, sessions, codes, refresh tokens.
	Store OAuthStore
	// Logger receives info/warn lifecycle events.
	Logger arbor.ILogger
	// DevMode disables Secure on the mcp_session_id cookie so local
	// http://localhost browser flows work.
	DevMode bool
	// ResolveSessionUser inspects an incoming /oauth/authorize request and
	// returns the already-logged-in user id (or "" when not). When non-nil
	// AND it returns a user id, /oauth/authorize short-circuits straight
	// into CompleteAuthorization, skipping the mcp_session_id cookie +
	// portal-bounce dance. Wired from main() with the V4 SessionStore +
	// UserStore — the V3 JWT-cookie shortcut doesn't apply here because
	// V4 session cookies are opaque UUIDs.
	ResolveSessionUser func(r *http.Request) string
	// Users is the lookup surface for stamping identity claims (email,
	// display name, provider) onto the access-token JWT at mint time.
	// Optional — when nil, JWTs carry only sub/scope/client_id and the
	// downstream bearer validator returns CallerIdentity with empty Email.
	// Wired in main() so /oauth/token mints carry the same identity
	// fields token_exchange already populates on sat_-prefixed tokens.
	Users UserStoreByID
}

// OAuthServer is the MCP-spec OAuth 2.1 Authorization Server. Long-lived;
// safe for concurrent use.
type OAuthServer struct {
	cfg OAuthServerConfig
}

// NewOAuthServer returns a configured server. Panics on missing required
// config — boot-time misconfiguration must be loud.
func NewOAuthServer(cfg OAuthServerConfig) *OAuthServer {
	if cfg.JWTSecret == "" {
		panic("auth: NewOAuthServer requires JWTSecret")
	}
	if cfg.Store == nil {
		panic("auth: NewOAuthServer requires Store")
	}
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = 1 * time.Hour
	}
	if cfg.RefreshTokenTTL <= 0 {
		cfg.RefreshTokenTTL = 7 * 24 * time.Hour
	}
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = 10 * time.Minute
	}
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	return &OAuthServer{cfg: cfg}
}

// JWTSecretBytes exposes the configured secret so the parent process can
// share it with BearerValidator (which validates JWTs on /mcp without
// needing a back-reference to OAuthServer).
func (s *OAuthServer) JWTSecretBytes() []byte {
	return []byte(s.cfg.JWTSecret)
}

// --- Discovery ---

// HandleAuthorizationServer serves GET /.well-known/oauth-authorization-server
// (RFC 8414). Cacheable for 1h to reduce client-side discovery chatter.
func (s *OAuthServer) HandleAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base := s.issuerForRequest(r)
	metadata := map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{defaultScope},
	}
	writeOAuthJSON(w, http.StatusOK, metadata, "public, max-age=3600")
}

// HandleProtectedResource serves GET /.well-known/oauth-protected-resource
// (RFC 9728). Points clients at this server as both the resource and the
// authorizing AS.
func (s *OAuthServer) HandleProtectedResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base := s.issuerForRequest(r)
	metadata := map[string]any{
		"resource":                   base + "/mcp",
		"authorization_servers":      []string{base},
		"scopes_supported":           []string{defaultScope},
		"bearer_methods_supported":   []string{"header"},
		"resource_documentation_uri": base + "/mcp",
	}
	writeOAuthJSON(w, http.StatusOK, metadata, "public, max-age=3600")
}

// --- Dynamic Client Registration (RFC 7591) ---

type dcrRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// HandleRegister handles POST /oauth/register. Public clients (PKCE +
// `token_endpoint_auth_method=none`) are the expected shape; we still
// generate a client_secret for clients that opt into confidential flows.
func (s *OAuthServer) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req dcrRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uris is required")
		return
	}

	clientID, err := generateUUID()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate client_id")
		return
	}
	clientSecret, err := generateRandomHex(32)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate client_secret")
		return
	}

	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	responseTypes := req.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}

	client := &OAuthClient{
		ClientID:                clientID,
		ClientSecret:            clientSecret,
		ClientName:              req.ClientName,
		RedirectURIs:            req.RedirectURIs,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		TokenEndpointAuthMethod: authMethod,
		CreatedAt:               time.Now().Unix(),
	}
	if err := s.cfg.Store.SaveClient(r.Context(), client); err != nil {
		s.logWarn(err, "save oauth client")
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to register client")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(client)
}

// --- Authorization ---

// HandleAuthorize handles GET and POST /oauth/authorize. GET initiates a
// browser flow with redirect; POST returns the session id directly (used
// by clients that prefer to drive the browser themselves).
func (s *OAuthServer) HandleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAuthorizeGET(w, r)
	case http.MethodPost:
		s.handleAuthorizePOST(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *OAuthServer) handleAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")

	// Resilience for clients that drop the redirect_uri (e.g. shells that
	// strip '&' on Windows). If we have a recent session for this client,
	// resume it.
	if redirectURI == "" && clientID != "" {
		if sess, err := s.cfg.Store.GetSessionByClientID(r.Context(), clientID); err == nil {
			s.setSessionCookieAndRedirect(w, r, sess.SessionID)
			return
		}
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}

	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Host == "" {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	responseType := q.Get("response_type")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	state := q.Get("state")
	scope := q.Get("scope")

	if clientID == "" || responseType == "" || codeChallenge == "" || codeChallengeMethod == "" || state == "" {
		oauthRedirectWithError(w, r, redirectURI, "invalid_request", "missing required parameters", state)
		return
	}
	if responseType != "code" {
		oauthRedirectWithError(w, r, redirectURI, "unsupported_response_type", "only code is supported", state)
		return
	}
	if codeChallengeMethod != "S256" {
		oauthRedirectWithError(w, r, redirectURI, "invalid_request", "only S256 code_challenge_method is supported", state)
		return
	}

	sessionID, err := s.createAuthSession(r.Context(), clientID, redirectURI, codeChallenge, codeChallengeMethod, state, scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.setSessionCookieAndRedirect(w, r, sessionID)
}

func (s *OAuthServer) handleAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	responseType := r.FormValue("response_type")
	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")
	state := r.FormValue("state")
	scope := r.FormValue("scope")

	if redirectURI == "" {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "missing redirect_uri"}, "")
		return
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Host == "" {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid redirect_uri"}, "")
		return
	}
	if clientID == "" || responseType == "" || codeChallenge == "" || codeChallengeMethod == "" || state == "" {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "missing required parameters"}, "")
		return
	}
	if responseType != "code" {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported response_type"}, "")
		return
	}
	if codeChallengeMethod != "S256" {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "only S256 code_challenge_method supported"}, "")
		return
	}

	sessionID, err := s.createAuthSession(r.Context(), clientID, redirectURI, codeChallenge, codeChallengeMethod, state, scope)
	if err != nil {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()}, "")
		return
	}
	writeOAuthJSON(w, http.StatusOK, map[string]string{"session_id": sessionID}, "")
}

func (s *OAuthServer) createAuthSession(ctx context.Context, clientID, redirectURI, codeChallenge, codeChallengeMethod, state, scope string) (string, error) {
	client, err := s.cfg.Store.GetClient(ctx, clientID)
	if err != nil {
		// Auto-register so unknown clients can still complete a flow.
		// Useful for `mcp-remote` style clients that don't go through DCR.
		client = &OAuthClient{
			ClientID:                clientID,
			ClientName:              "auto-registered",
			RedirectURIs:            []string{redirectURI},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
			CreatedAt:               time.Now().Unix(),
		}
		if err := s.cfg.Store.SaveClient(ctx, client); err != nil {
			return "", fmt.Errorf("auto-register client: %w", err)
		}
	}
	if !containsString(client.RedirectURIs, redirectURI) {
		return "", errors.New("redirect_uri does not match registered URI")
	}
	if scope == "" {
		scope = defaultScope
	}

	sessionID, err := generateRandomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	session := &OAuthSession{
		SessionID:     sessionID,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: codeChallenge,
		CodeMethod:    codeChallengeMethod,
		Scope:         scope,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.cfg.Store.SaveSession(ctx, session); err != nil {
		return "", fmt.Errorf("save oauth session: %w", err)
	}
	return sessionID, nil
}

// setSessionCookieAndRedirect drops the mcp_session_id cookie that
// auth.Handlers.Login reads after a successful login, then sends the
// browser to the login page. If the user is already logged in (carries
// a valid satellites_session cookie), short-circuit straight to
// CompleteAuthorization and redirect to the client redirect_uri.
func (s *OAuthServer) setSessionCookieAndRedirect(w http.ResponseWriter, r *http.Request, sessionID string) {
	// Already-logged-in shortcut.
	if userID := s.extractSessionUserID(r); userID != "" {
		redirectURL, err := s.CompleteAuthorization(r.Context(), sessionID, userID)
		if err == nil {
			s.logInfo("oauth: completed via existing browser session", "user_id", userID)
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}
		s.logWarn(err, "complete via existing session — falling back to login")
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "mcp_session_id",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.Redirect(w, r, "/?mcp_session="+sessionID, http.StatusFound)
}

// extractSessionUserID consults the operator-supplied ResolveSessionUser
// callback. Empty when the callback is unwired or returns "" (no
// already-logged-in user). Replaces the V3 JWT-cookie shortcut, which
// doesn't apply to V4's opaque-UUID session model.
func (s *OAuthServer) extractSessionUserID(r *http.Request) string {
	if s.cfg.ResolveSessionUser == nil {
		return ""
	}
	return s.cfg.ResolveSessionUser(r)
}

// CompleteAuthorization is called by auth.Handlers.Login when it detects
// the mcp_session_id bridge cookie on a successful login. Mints a
// single-use code, deletes the OAuthSession, returns the redirect URL
// (with ?code=…&state=…) the login handler should redirect the browser to.
func (s *OAuthServer) CompleteAuthorization(ctx context.Context, sessionID, userID string) (string, error) {
	sess, err := s.cfg.Store.GetSession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("session not found or expired")
	}
	code, err := generateRandomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate auth code: %w", err)
	}
	authCode := &OAuthCode{
		Code:          code,
		ClientID:      sess.ClientID,
		UserID:        userID,
		RedirectURI:   sess.RedirectURI,
		CodeChallenge: sess.CodeChallenge,
		Scope:         sess.Scope,
		ExpiresAt:     time.Now().Add(s.cfg.CodeTTL).UTC(),
		Used:          false,
	}
	if err := s.cfg.Store.SaveCode(ctx, authCode); err != nil {
		return "", fmt.Errorf("save auth code: %w", err)
	}
	if err := s.cfg.Store.DeleteSession(ctx, sessionID); err != nil {
		s.logWarn(err, "delete oauth session after completing authorization")
	}

	redirectURL, err := url.Parse(sess.RedirectURI)
	if err != nil {
		return "", fmt.Errorf("invalid redirect URI: %w", err)
	}
	q := redirectURL.Query()
	q.Set("code", code)
	q.Set("state", sess.State)
	redirectURL.RawQuery = q.Encode()
	return redirectURL.String(), nil
}

// --- Token ---

// HandleToken handles POST /oauth/token. Dispatches on grant_type.
func (s *OAuthServer) HandleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		s.handleAuthCodeGrant(w, r)
	case "refresh_token":
		s.handleRefreshTokenGrant(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *OAuthServer) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.FormValue("code")
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	codeVerifier := r.FormValue("code_verifier")

	if code == "" || clientID == "" || redirectURI == "" || codeVerifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing required parameters")
		return
	}

	authCode, err := s.cfg.Store.GetCode(ctx, code)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code not found, expired, or already used")
		return
	}
	if authCode.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if authCode.RedirectURI != redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if !VerifyPKCE(codeVerifier, authCode.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if err := s.cfg.Store.MarkCodeUsed(ctx, code); err != nil {
		s.logWarn(err, "mark oauth code used")
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to consume authorization code")
		return
	}

	issuer := s.issuerForRequest(r)
	accessToken, err := s.mintAccessToken(authCode.UserID, authCode.Scope, authCode.ClientID, issuer)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to mint access token")
		return
	}
	refreshToken, err := generateUUID()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate refresh token")
		return
	}
	rt := &OAuthRefreshToken{
		Token:     refreshToken,
		UserID:    authCode.UserID,
		ClientID:  authCode.ClientID,
		Scope:     authCode.Scope,
		ExpiresAt: time.Now().Add(s.cfg.RefreshTokenTTL).UTC(),
	}
	if err := s.cfg.Store.SaveRefreshToken(ctx, rt); err != nil {
		s.logWarn(err, "save refresh token")
	}

	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int64(s.cfg.AccessTokenTTL.Seconds()),
		"refresh_token": refreshToken,
		"scope":         authCode.Scope,
	}, "no-store")
}

func (s *OAuthServer) handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	oldToken := r.FormValue("refresh_token")
	clientID := r.FormValue("client_id")

	if oldToken == "" || clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing required parameters")
		return
	}

	rt, err := s.cfg.Store.GetRefreshToken(ctx, oldToken)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token not found or expired")
		return
	}
	if rt.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}

	// Rotate: delete old, issue new.
	if err := s.cfg.Store.DeleteRefreshToken(ctx, oldToken); err != nil {
		s.logWarn(err, "delete old refresh token")
	}

	issuer := s.issuerForRequest(r)
	accessToken, err := s.mintAccessToken(rt.UserID, rt.Scope, rt.ClientID, issuer)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to mint access token")
		return
	}
	newToken, err := generateUUID()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate refresh token")
		return
	}
	newRT := &OAuthRefreshToken{
		Token:     newToken,
		UserID:    rt.UserID,
		ClientID:  rt.ClientID,
		Scope:     rt.Scope,
		ExpiresAt: time.Now().Add(s.cfg.RefreshTokenTTL).UTC(),
	}
	if err := s.cfg.Store.SaveRefreshToken(ctx, newRT); err != nil {
		s.logWarn(err, "save new refresh token")
	}

	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int64(s.cfg.AccessTokenTTL.Seconds()),
		"refresh_token": newToken,
		"scope":         rt.Scope,
	}, "no-store")
}

// mintAccessToken builds a JWTClaims and signs via CreateJWT (HS256).
// When cfg.Users is non-nil, the user row is looked up and Email / Name /
// Provider claims are stamped — the bearer validator unpacks these back
// into CallerIdentity, restoring `caller.Email` for every downstream
// MCP / HTTP handler. Lookup failure is non-fatal: a JWT without the
// optional identity claims is still valid for sub-only auth paths.
func (s *OAuthServer) mintAccessToken(userID, scope, clientID, issuer string) (string, error) {
	now := time.Now()
	claims := &JWTClaims{
		Sub:      userID,
		Scope:    scope,
		ClientID: clientID,
		Iss:      issuer,
		Iat:      now.Unix(),
		Exp:      now.Add(s.cfg.AccessTokenTTL).Unix(),
	}
	if s.cfg.Users != nil {
		if u, err := s.cfg.Users.GetByID(userID); err == nil {
			claims.Email = u.Email
			claims.Name = u.DisplayName
			claims.Provider = u.Provider
		}
	}
	return CreateJWT(claims, s.JWTSecretBytes())
}

// --- Helpers ---

func (s *OAuthServer) issuerForRequest(r *http.Request) string {
	if s.cfg.Issuer != "" {
		return s.cfg.Issuer
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *OAuthServer) logWarn(err error, msg string) {
	if s.cfg.Logger == nil || err == nil {
		return
	}
	s.cfg.Logger.Warn().Str("error", err.Error()).Msg("oauth: " + msg)
}

func (s *OAuthServer) logInfo(msg string, keyValues ...string) {
	if s.cfg.Logger == nil {
		return
	}
	ev := s.cfg.Logger.Info()
	for i := 0; i+1 < len(keyValues); i += 2 {
		ev = ev.Str(keyValues[i], keyValues[i+1])
	}
	ev.Msg(msg)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": description,
	})
}

func writeOAuthJSON(w http.ResponseWriter, status int, body any, cacheControl string) {
	w.Header().Set("Content-Type", "application/json")
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func oauthRedirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, code, description, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func generateRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func containsString(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

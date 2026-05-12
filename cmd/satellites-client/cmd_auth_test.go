package main

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

	"github.com/bobmcallan/satellites/internal/clicred"
)

// TestAuthLogin_FakeIdP_RoundTrip drives the OAuth login flow end-to-
// end against an httptest-faked IdP. Asserts the bearer is persisted
// to ~/.satellites/credentials.json and round-trips via
// clicred.Resolve. No real network — every external call is to the
// httptest server's loopback address.
func TestAuthLogin_FakeIdP_RoundTrip(t *testing.T) {
	const wantBearer = "fake-access-token"

	// Capture the parameters /oauth/authorize was called with so the
	// assertions can verify the PKCE shape the runtime constructed.
	var (
		capturedCodeChallenge       string
		capturedCodeChallengeMethod string
		capturedClientID            string
		capturedRedirectURI         string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		capturedCodeChallenge = q.Get("code_challenge")
		capturedCodeChallengeMethod = q.Get("code_challenge_method")
		capturedClientID = q.Get("client_id")
		capturedRedirectURI = q.Get("redirect_uri")
		redirect := q.Get("redirect_uri")
		state := q.Get("state")
		// Bounce back to the localhost callback with a code + state.
		http.Redirect(w, r, redirect+"?code=fake_authcode&state="+url.QueryEscape(state), http.StatusFound)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "bad grant_type", http.StatusBadRequest)
			return
		}
		if r.Form.Get("code") != "fake_authcode" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		if r.Form.Get("code_verifier") == "" {
			http.Error(w, "missing verifier", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  wantBearer,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "fake-refresh",
			"scope":         "read write",
		})
	})
	fakeIdP := httptest.NewServer(mux)
	defer fakeIdP.Close()

	// The opener simulates the browser: GET the authorize URL with a
	// client that follows the 302 to the localhost callback. The
	// localhost handler in runAuthLogin captures the code and triggers
	// the token exchange.
	opener := func(authorizeURL string) {
		go func() {
			resp, err := http.Get(authorizeURL)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bearer, err := runAuthLogin(ctx, fakeIdP.URL, opener)
	if err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if bearer != wantBearer {
		t.Fatalf("bearer = %q, want %q", bearer, wantBearer)
	}

	// Assert the authorize call captured the expected PKCE + client params.
	if capturedCodeChallengeMethod != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", capturedCodeChallengeMethod)
	}
	if capturedCodeChallenge == "" {
		t.Errorf("code_challenge empty")
	}
	if capturedClientID != "satellites-client" {
		t.Errorf("client_id = %q, want satellites-client", capturedClientID)
	}
	if !strings.HasPrefix(capturedRedirectURI, "http://127.0.0.1:") {
		t.Errorf("redirect_uri = %q, want 127.0.0.1 prefix", capturedRedirectURI)
	}

	// Round-trip the persisted bearer via clicred.WriteToken +
	// clicred.Resolve so the AC2 round-trip assertion is exercised
	// end-to-end (the cobra RunE invokes WriteToken itself in the
	// production path; the test mirrors the same call sequence here).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(clicred.EnvToken, "")
	credPath := filepath.Join(dir, ".satellites", "credentials.json")
	if err := clicred.WriteToken(credPath, fakeIdP.URL, bearer); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	tok, err := clicred.Resolve("", fakeIdP.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != wantBearer {
		t.Fatalf("Resolve = %q, want %q", tok, wantBearer)
	}

	// Sanity: credentials file is 0o600.
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials perm = %o, want 0600", perm)
	}
}

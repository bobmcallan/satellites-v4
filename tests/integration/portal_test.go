package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLoginFlowSetsAndClearsSession drives the full SSR loop against the
// container: GET / → 303 /login → GET /login (assert buttons/form) → POST
// DevMode creds to /auth/login → follow to / and assert authenticated
// content (email + version chip).
func TestLoginFlowSetsAndClearsSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	baseURL, stop := startServerContainerWithEnv(t, ctx, map[string]string{
		"DEV_USERNAME": "dev@local",
		"DEV_PASSWORD": "letmein",
	})
	defer stop()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 1. GET / unauth → landing on /login (client.Jar follows redirects).
	resp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "action=\"/auth/login\"") {
		t.Fatalf("expected redirect to login; body=%s", string(body))
	}
	for _, want := range []string{"sign in", "username", "password"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("login page missing %q", want)
		}
	}

	// 2. POST credentials.
	form := url.Values{"username": {"dev@local"}, "password": {"letmein"}}
	resp, err = client.PostForm(baseURL+"/auth/login", form)
	if err != nil {
		t.Fatalf("POST /auth/login: %v", err)
	}
	landing, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("landing status = %d; body=%s", resp.StatusCode, string(landing))
	}
	if !strings.Contains(string(landing), "Signed in as") {
		t.Errorf("landing missing authenticated marker; body=%s", string(landing))
	}
	if !strings.Contains(string(landing), "dev@local") {
		t.Errorf("landing missing user email; body=%s", string(landing))
	}
	if !strings.Contains(string(landing), "version-chip") {
		t.Errorf("landing missing version chip; body=%s", string(landing))
	}

	// 3. Logout via POST. Then a GET / must redirect back to /login.
	logoutResp, err := client.Post(baseURL+"/auth/logout", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /auth/logout: %v", err)
	}
	logoutBody, _ := io.ReadAll(logoutResp.Body)
	logoutResp.Body.Close()
	// Should have followed 303 to /login and rendered the login form again.
	if !strings.Contains(string(logoutBody), "action=\"/auth/login\"") {
		t.Errorf("post-logout body missing login form: %s", string(logoutBody))
	}
}

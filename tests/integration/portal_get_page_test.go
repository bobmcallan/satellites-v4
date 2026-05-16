package integration

// portal_get_page_test.go — sty_02ad5eb9 — restores the V3
// satellites_portal_get_page surface as the new portal_get_page MCP
// verb. End-to-end coverage of AC8 (a)–(e) against a containerised
// satellites server (dev-mode session auth, no DB required).
//
// The verb is read-only by design (pr_substrate_model): it issues a
// loopback HTTP GET against the local satellites server with a 30s
// minted session cookie, returns the body + status to the caller, and
// never writes substrate state.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPortalGetPage drives every AC8 case against a single booted
// satellites container so the test pays the boot cost once. Each
// subtest is independent and asserts a distinct branch of
// validatePortalPath / the loopback fetch.
func TestPortalGetPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	baseURL, stop := startServerContainerWithEnv(t, ctx, map[string]string{
		"SATELLITES_DEV_USERNAME": "dev@local",
		"SATELLITES_DEV_PASSWORD": "letmein",
	})
	defer stop()

	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: jar}

	// Authenticate via the SSR dev-mode quick-signin so the
	// portal_get_page caller resolves to a real user. The verb's
	// minted loopback session is scoped to that UserID, so the SSR
	// page handler can resolve a user and render the page (rather
	// than 303'ing to /login).
	form := url.Values{"username": {"dev@local"}, "password": {"letmein"}}
	loginResp, err := httpClient.PostForm(baseURL+"/auth/login", form)
	if err != nil {
		t.Fatalf("dev-mode login: %v", err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("dev-mode login status = %d", loginResp.StatusCode)
	}

	// AC8(a): happy path. GET /projects renders the dev user's
	// projects page. Markers chosen to be stable across DB-on and
	// DB-off boot modes (the bare dev container in this test boots
	// without a DB so the empty-state copy is the "Projects are
	// unavailable…" variant; both modes share the SSR title +
	// page-header markup).
	t.Run("happy_path_renders_html", func(t *testing.T) {
		out := callPortalGetPage(t, ctx, httpClient, baseURL, "/projects")
		if out.Status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", out.Status, out.Body)
		}
		if !strings.Contains(out.Body, "<title>SATELLITES") {
			t.Errorf("body missing SSR title marker; body=%s", out.Body)
		}
		if !strings.Contains(out.Body, "<h1>projects</h1>") {
			t.Errorf("body missing /projects header marker; body=%s", out.Body)
		}
	})

	// AC8(d): redirect-not-followed. /ledger 303s to /projects for a
	// dev user with no projects (internal/portal/portal.go
	// handleLedgerRedirect). The verb must return the 3xx status and
	// the redirect-response body, not the followed /projects page.
	t.Run("redirect_not_followed", func(t *testing.T) {
		out := callPortalGetPage(t, ctx, httpClient, baseURL, "/ledger")
		if out.Status != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303 SeeOther; body=%s", out.Status, out.Body)
		}
		// The empty-state copy from /projects must NOT appear; if it
		// does, the loopback client followed the redirect.
		if strings.Contains(out.Body, "You don't own any projects yet") {
			t.Errorf("body followed the redirect; expected the 303 response body, got the /projects page: %s", out.Body)
		}
	})

	// AC8(b) + (e): every blocked prefix returns a 400 with the
	// documented error string. /api/v1/... is the (e) explicit case;
	// the others are AC8(b).
	t.Run("blocked_prefixes", func(t *testing.T) {
		cases := []struct {
			path string
			want string
		}{
			{"/api/v1/anything", "prefix /api/ blocked"},
			{"/oauth/google", "prefix /oauth/ blocked"},
			{"/mcp", "prefix /mcp blocked"},
			{"/static/app.css", "prefix /static/ blocked"},
			{"/.well-known/oauth-protected-resource", "prefix /.well-known/ blocked"},
		}
		for _, tc := range cases {
			t.Run(tc.path, func(t *testing.T) {
				status, body := callPortalGetPageRaw(t, ctx, httpClient, baseURL, tc.path)
				if status != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body=%s", status, body)
				}
				var errEnv struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(body, &errEnv); err != nil {
					t.Fatalf("decode error envelope: %v; raw=%s", err, body)
				}
				if errEnv.Error != tc.want {
					t.Errorf("error = %q, want %q", errEnv.Error, tc.want)
				}
			})
		}
	})

	// AC8(c): anonymous (no cookie, no bearer) → the AuthMiddleware
	// surfaces the documented authentication-required error before
	// the typed method runs. The verb's typed-method auth gate uses
	// the same literal string so callers see one consistent error.
	t.Run("anonymous_auth_required", func(t *testing.T) {
		anon := &http.Client{} // no cookie jar, no bearer
		status, body := callPortalGetPageRaw(t, ctx, anon, baseURL, "/projects")
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", status, body)
		}
		var errEnv struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &errEnv); err != nil {
			t.Fatalf("decode error envelope: %v; raw=%s", err, body)
		}
		if !strings.Contains(errEnv.Error, "authentication required") {
			t.Errorf("error = %q, want it to mention authentication required", errEnv.Error)
		}
	})
}

// callPortalGetPage posts the verb and asserts a 200 + decodes the
// `{status, body}` envelope. For non-200 transport responses (e.g.
// the blocked-prefix 400s) callers use callPortalGetPageRaw.
func callPortalGetPage(t *testing.T, ctx context.Context, c *http.Client, baseURL, path string) portalGetPageEnvelope {
	t.Helper()
	status, raw := callPortalGetPageRaw(t, ctx, c, baseURL, path)
	if status != http.StatusOK {
		t.Fatalf("portal_get_page status=%d body=%s", status, raw)
	}
	var out portalGetPageEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode envelope: %v; raw=%s", err, raw)
	}
	return out
}

// callPortalGetPageRaw posts the verb and returns the transport status
// + raw body, leaving error-envelope decoding to the caller.
func callPortalGetPageRaw(t *testing.T, ctx context.Context, c *http.Client, baseURL, path string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"path": path})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/portal/get-page", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("post portal/get-page: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

type portalGetPageEnvelope struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

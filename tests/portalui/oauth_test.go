//go:build portalui

package portalui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"golang.org/x/oauth2"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestOAuth_LandingButtonsPresent (AC7 surface check) — when both
// providers are configured, the landing page renders buttons that link
// to the per-provider /start endpoint.
func TestOAuth_LandingButtonsPresent(t *testing.T) {
	h := startOAuthHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	var googleHref, githubHref string
	if err := chromedp.Run(browserCtx,
		network.ClearBrowserCookies(),
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`a[href="/auth/google/start"]`, chromedp.ByQuery),
		chromedp.AttributeValue(`a[href="/auth/google/start"]`, "href", &googleHref, nil),
		chromedp.AttributeValue(`a[href="/auth/github/start"]`, "href", &githubHref, nil),
	); err != nil {
		t.Fatalf("navigate /: %v", err)
	}
	if googleHref != "/auth/google/start" {
		t.Errorf("Google href = %q, want /auth/google/start", googleHref)
	}
	if githubHref != "/auth/github/start" {
		t.Errorf("GitHub href = %q, want /auth/github/start", githubHref)
	}
}

// TestOAuth_FullSigninFlow (AC7 full E2E) — chromedp clicks the Google
// button, follows the redirect to a stub provider, the stub immediately
// 302s back to /auth/google/callback with code+state, the callback
// completes the exchange, and the dashboard renders for the new user.
func TestOAuth_FullSigninFlow(t *testing.T) {
	h := startOAuthHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	var bodyText string
	if err := chromedp.Run(browserCtx,
		network.ClearBrowserCookies(),
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`a[href="/auth/google/start"]`, chromedp.ByQuery),
		chromedp.Click(`a[href="/auth/google/start"]`, chromedp.ByQuery),
		// Click → 303 to stub /auth → stub 302 → /auth/google/callback → 303
		// → /. Wait for the dashboard to render.
		chromedp.Sleep(600*time.Millisecond),
		chromedp.WaitVisible(`.version-chip`, chromedp.ByQuery),
		chromedp.Text("body", &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("oauth full flow: %v", err)
	}
	if !strings.Contains(bodyText, "Signed in as") {
		t.Errorf("expected dashboard after OAuth signin, got body=%s", bodyText)
	}
}

// startOAuthHarness builds a harness with Google OAuth wired to a local
// stub provider that auto-completes the consent step. Used by the
// OAuth-flow tests above. Owns the stub server's lifetime.
func startOAuthHarness(t *testing.T) *Harness {
	t.Helper()

	// Stub provider server — answers /auth (auto-redirects to client
	// callback), /token (issues a fixed token), /userinfo (returns Alice).
	var stub *httptest.Server
	stub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth":
			// Auto-consent: 302 back to redirect_uri with code + state.
			redirect := r.URL.Query().Get("redirect_uri")
			state := r.URL.Query().Get("state")
			http.Redirect(w, r, redirect+"?state="+state+"&code=stub-code", http.StatusFound)
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "stub-token", "token_type": "Bearer", "expires_in": 3600,
			})
		case "/userinfo":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"email": "alice@example.com", "name": "Alice OAuth",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.Close)

	h := StartHarness(t)

	// Inject the provider into the harness's live auth.Handlers. The mux
	// already registered authHandlers via Register() — RegisterOAuth was a
	// no-op at startup because Providers was nil. We rebuild the mux to
	// pick up the providers.
	h.AuthHandlers.Providers = &auth.ProviderSet{
		Google: &auth.Provider{
			Name: "google",
			OAuth2: &oauth2.Config{
				ClientID:     "harness-id",
				ClientSecret: "harness-secret",
				RedirectURL:  h.BaseURL + "/auth/google/callback",
				Scopes:       []string{"openid", "email", "profile"},
				Endpoint: oauth2.Endpoint{
					AuthURL:  stub.URL + "/auth",
					TokenURL: stub.URL + "/token",
				},
			},
			FetchInfo: func(ctx context.Context, token *oauth2.Token) (auth.ProviderUserInfo, error) {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, stub.URL+"/userinfo", nil)
				req.Header.Set("Authorization", "Bearer "+token.AccessToken)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return auth.ProviderUserInfo{}, err
				}
				defer resp.Body.Close()
				var body struct{ Email, Name string }
				_ = json.NewDecoder(resp.Body).Decode(&body)
				return auth.ProviderUserInfo{Email: body.Email, DisplayName: body.Name}, nil
			},
		},
		GitHub: &auth.Provider{
			Name:   "github",
			OAuth2: &oauth2.Config{ClientID: "ghid", ClientSecret: "ghs", RedirectURL: h.BaseURL + "/auth/github/callback"},
			FetchInfo: func(ctx context.Context, token *oauth2.Token) (auth.ProviderUserInfo, error) {
				return auth.ProviderUserInfo{Email: "bob@example.com", DisplayName: "Bob"}, nil
			},
		},
	}

	// Configure the cfg's GoogleEnabled gate by setting the cred fields on
	// the underlying config. The renderLanding handler reads these to
	// decide whether to render the buttons.
	h.AuthHandlers.Cfg.GoogleClientID = "harness-id"
	h.AuthHandlers.Cfg.GoogleClientSecret = "harness-secret"
	h.AuthHandlers.Cfg.GithubClientID = "ghid"
	h.AuthHandlers.Cfg.GithubClientSecret = "ghs"

	// The mux already exists. Register OAuth routes onto a NEW sub-mux and
	// route /auth/google/* + /auth/github/* through it via a delegating
	// handler installed on the existing server. Easiest path: replace the
	// underlying server's handler with one that routes OAuth paths first.
	oauthMux := http.NewServeMux()
	h.AuthHandlers.RegisterOAuth(oauthMux)
	prevHandler := h.Server.Config.Handler
	h.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/google/") || strings.HasPrefix(r.URL.Path, "/auth/github/") {
			oauthMux.ServeHTTP(w, r)
			return
		}
		prevHandler.ServeHTTP(w, r)
	})

	// Configure the provider's RedirectURL to use the actual harness URL
	// (was set above with h.BaseURL which is now known). Already done.
	_ = config.Config{} // silence unused import in some test variants
	return h
}

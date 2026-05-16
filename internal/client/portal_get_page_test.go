package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValidatePortalPath_AllBranches pins the per-branch error strings
// the integration test asserts against (AC2 + AC8(b)+(e)).
func TestValidatePortalPath_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means valid
	}{
		{"happy_root", "/projects", ""},
		{"happy_nested", "/projects/proj_abc12345", ""},
		{"empty_rejected", "", "path must start with '/'"},
		{"no_leading_slash", "projects", "path must start with '/'"},
		{"contains_dotdot", "/projects/../etc/passwd", "path must not contain '..'"},
		{"percent_encoded_dotdot", "/projects/%2e%2e/etc", "path must not contain '..'"},
		{"prefix_api", "/api/v1/anything", "prefix /api/ blocked"},
		{"prefix_oauth", "/oauth/google", "prefix /oauth/ blocked"},
		{"prefix_mcp_bare", "/mcp", "prefix /mcp blocked"},
		{"prefix_mcp_subpath", "/mcp/session", "prefix /mcp blocked"},
		{"prefix_static", "/static/app.css", "prefix /static/ blocked"},
		{"prefix_well_known", "/.well-known/oauth-protected-resource", "prefix /.well-known/ blocked"},
		{"percent_encoded_api_prefix", "/%61pi/v1", "prefix /api/ blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePortalPath(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validatePortalPath(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validatePortalPath(%q) = nil, want %q", tc.in, tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("validatePortalPath(%q) = %q, want %q", tc.in, err.Error(), tc.want)
			}
		})
	}
}

// TestResolvePortalLoopbackHost pins the host-substitution rule from
// AC4: "" and "0.0.0.0" both fall back to 127.0.0.1; any other value
// passes through.
func TestResolvePortalLoopbackHost(t *testing.T) {
	cases := map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"  ":        "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"localhost": "localhost",
		"192.0.2.1": "192.0.2.1",
	}
	for in, want := range cases {
		if got := ResolvePortalLoopbackHost(in); got != want {
			t.Errorf("ResolvePortalLoopbackHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPortalGetPage_Auth covers the early-return paths that don't
// require an http.Client at all.
func TestPortalGetPage_Auth(t *testing.T) {
	c := New(Deps{})
	_, err := c.PortalGetPage(context.Background(), Caller{}, PortalGetPageInput{Path: "/projects"})
	if err == nil || err.Error() != "authentication required" {
		t.Fatalf("anonymous call err = %v, want %q", err, "authentication required")
	}
}

// TestPortalGetPage_LoopbackNotConfigured covers the dep-gate when
// PortalLoopbackURL is nil — the verb must not panic and must surface
// the documented unavailable string so the wire layer can render it.
func TestPortalGetPage_LoopbackNotConfigured(t *testing.T) {
	c := New(Deps{})
	_, err := c.PortalGetPage(context.Background(), Caller{UserID: "u_dev"}, PortalGetPageInput{Path: "/projects"})
	if err == nil || !strings.Contains(err.Error(), "loopback not configured") {
		t.Fatalf("err = %v, want loopback-not-configured", err)
	}
}

// TestPortalGetPage_RedirectNotFollowed proves AC4's redirect rule via
// an httptest server: 303 from the SSR target must surface to the
// caller as Status=303 with the redirect-response body, not the
// followed page.
func TestPortalGetPage_RedirectNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ledger":
			http.Redirect(w, r, "/projects", http.StatusSeeOther)
		case "/projects":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "FOLLOWED")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(Deps{
		PortalLoopbackURL: func() string { return srv.URL },
		PortalSessionMint: func(_ string, _ time.Duration) (string, error) { return "session-id", nil },
	})
	out, err := c.PortalGetPage(context.Background(), Caller{UserID: "u_dev"}, PortalGetPageInput{Path: "/ledger"})
	if err != nil {
		t.Fatalf("PortalGetPage: %v", err)
	}
	if out.Status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", out.Status)
	}
	if strings.Contains(out.Body, "FOLLOWED") {
		t.Errorf("body followed the redirect: %q", out.Body)
	}
}

// TestPortalGetPage_HappyPath proves the loopback round-trip returns
// the raw response body + status verbatim and threads the minted
// session cookie through to the target.
func TestPortalGetPage_HappyPath(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("satellites_session"); err == nil {
			gotCookie = c.Value
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html>OK "+r.URL.Path+"</html>")
	}))
	defer srv.Close()

	const wantSession = "mint-test-id"
	c := New(Deps{
		PortalLoopbackURL: func() string { return srv.URL },
		PortalSessionMint: func(_ string, _ time.Duration) (string, error) { return wantSession, nil },
	})
	out, err := c.PortalGetPage(context.Background(), Caller{UserID: "u_dev"}, PortalGetPageInput{Path: "/projects"})
	if err != nil {
		t.Fatalf("PortalGetPage: %v", err)
	}
	if out.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", out.Status)
	}
	if !strings.Contains(out.Body, "<html>OK /projects</html>") {
		t.Errorf("body = %q, want loopback-rendered payload", out.Body)
	}
	if gotCookie != wantSession {
		t.Errorf("loopback request cookie = %q, want %q (minted session must flow through)", gotCookie, wantSession)
	}
}

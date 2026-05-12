package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/clicred"
	"github.com/bobmcallan/satellites/internal/cliexit"
)

// authClientID is the static public-client identifier the
// satellites-client uses with the server's OAuth endpoints. Pre-
// registered by the server; PKCE-only public client per RFC 8252.
const authClientID = "satellites-client"

func registerAuthNoun(root *cobra.Command) {
	noun := &cobra.Command{
		Use:   "auth",
		Short: "Auth — operator login + credential bootstrap.",
	}
	noun.AddCommand(newAuthLoginCmd())
	root.AddCommand(noun)
}

func newAuthLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "OAuth 2.1 + PKCE localhost flow; persists the bearer to ~/.satellites/credentials.json.",
		RunE: func(cmd *cobra.Command, args []string) error {
			server := effectiveServer()
			path, err := clicred.DefaultCredentialsPath()
			if err != nil {
				return cliexit.Wrap(cliexit.Auth, fmt.Errorf("resolve credentials path: %w", err))
			}
			bearer, err := runAuthLogin(cmd.Context(), server, defaultOpenAuthorizeURL)
			if err != nil {
				return cliexit.Wrap(cliexit.Auth, err)
			}
			if err := clicred.WriteToken(path, server, bearer); err != nil {
				return cliexit.Wrap(cliexit.Auth, fmt.Errorf("write token: %w", err))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "authenticated; bearer written to %s for server %s\n", path, server)
			return nil
		},
	}
}

// defaultOpenAuthorizeURL prints the URL to stdout and best-effort
// launches the operator's default browser. This is the production
// path; cmd_auth_test.go injects a different opener that drives the
// redirect chain with an http.Client.
func defaultOpenAuthorizeURL(authorizeURL string) {
	fmt.Fprintf(os.Stdout, "Open this URL in your browser to authenticate:\n  %s\n", authorizeURL)
	tryOpenBrowser(authorizeURL)
}

// runAuthLogin executes the OAuth 2.1 (PKCE, S256) authorization-code
// flow against `<server>/oauth/authorize` + `<server>/oauth/token`,
// using a fresh localhost listener for the redirect_uri. Returns the
// access_token on success. The opener is invoked with the authorize
// URL once the localhost listener is accepting — the test path uses
// this hook to drive the flow without a real browser.
func runAuthLogin(ctx context.Context, server string, opener func(authorizeURL string)) (string, error) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		return "", fmt.Errorf("generate code verifier: %w", err)
	}
	challenge := codeChallengeS256(verifier)
	state, err := randomState()
	if err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("bind localhost: %w", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return "", fmt.Errorf("listener returned non-TCP addr: %T", listener.Addr())
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("oauth callback state mismatch")
			return
		}
		if oErr := q.Get("error"); oErr != "" {
			http.Error(w, oErr, http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth callback error: %s — %s", oErr, q.Get("error_description"))
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("oauth callback missing code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><p>satellites-client: authenticated. You can close this tab.</p></body></html>"))
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authorizeURL := buildAuthorizeURL(server, redirectURI, challenge, state)
	if opener != nil {
		opener(authorizeURL)
	}

	select {
	case code := <-codeCh:
		return exchangeCodeForToken(ctx, server, code, verifier, redirectURI)
	case e := <-errCh:
		return "", e
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// buildAuthorizeURL composes the /oauth/authorize URL with the
// standard public-client + S256 PKCE parameters.
func buildAuthorizeURL(server, redirectURI, challenge, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", authClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("scope", "read write")
	return strings.TrimRight(server, "/") + "/oauth/authorize?" + q.Encode()
}

// exchangeCodeForToken POSTs the authorization code + verifier to
// /oauth/token and returns the access_token field from the response.
func exchangeCodeForToken(ctx context.Context, server, code, verifier, redirectURI string) (string, error) {
	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("code", code)
	body.Set("client_id", authClientID)
	body.Set("redirect_uri", redirectURI)
	body.Set("code_verifier", verifier)

	tokenURL := strings.TrimRight(server, "/") + "/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", tokenURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var envelope struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if envelope.AccessToken == "" {
		return "", errors.New("token response missing access_token")
	}
	return envelope.AccessToken, nil
}

// generateCodeVerifier emits a 43-character base64url-encoded random
// string suitable for use as a PKCE code_verifier (RFC 7636).
func generateCodeVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// codeChallengeS256 returns the base64url-encoded SHA-256 digest of
// the verifier, matching the server's VerifyPKCE.
func codeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomState returns a 16-byte base64url-encoded nonce for CSRF
// protection of the authorize→callback round-trip.
func randomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// tryOpenBrowser best-effort opens the URL using the platform's
// default browser. Failure is non-fatal — the URL is always printed
// to stdout by the caller.
func tryOpenBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
}

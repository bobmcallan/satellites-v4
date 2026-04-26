package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
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
	if !strings.Contains(string(landing), `<footer class="footer">`) {
		t.Errorf("landing missing footer (story_1340913b moved version metadata to the footer); body=%s", string(landing))
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

// TestPortalProjectsPages boots satellites + SurrealDB, creates a project
// via the MCP surface with an API key, then drives the portal /projects
// pages as a dev-mode session user. Covers the end-to-end hand-off between
// MCP writes and SSR reads for the project primitive.
//
// Note: the project is created under the "apikey" synthetic owner (so the
// dev-mode session user sees the empty-state for their own owner id) and
// the detail page for that project correctly returns 404 to the dev user.
// This verifies cross-owner isolation holds over the HTTP surface.
func TestPortalProjectsPages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "surrealdb/surrealdb:v3.0.0",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {"surrealdb"},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	docsHost := filepath.Join(repoRoot(t), "docs")
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"DB_DSN":              "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": "key_portal",
			"DOCS_DIR":            "/app/docs",
			"DEV_USERNAME":        "dev@local",
			"DEV_PASSWORD":        "letmein",
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	defer stop()

	// 1. Create a project via MCP as the API key caller (owner = "apikey").
	mcpURL := baseURL + "/mcp"
	rpcCall(t, ctx, mcpURL, "key_portal", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	})
	created := rpcCall(t, ctx, mcpURL, "key_portal", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "project_create",
			"arguments": map[string]any{"name": "portal-smoke"},
		},
	})
	var proj map[string]any
	if err := json.Unmarshal([]byte(extractToolText(t, created)), &proj); err != nil {
		t.Fatalf("decode project_create: %v", err)
	}
	apikeyProjID, _ := proj["id"].(string)
	if apikeyProjID == "" {
		t.Fatal("created project missing id")
	}

	// 2. Log in as a dev-mode session user (different owner than apikey).
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	form := url.Values{"username": {"dev@local"}, "password": {"letmein"}}
	loginResp, err := client.PostForm(baseURL+"/auth/login", form)
	if err != nil {
		t.Fatalf("session login: %v", err)
	}
	loginResp.Body.Close()

	// 3. Dev user has no owned projects yet → empty state.
	listResp, err := client.Get(baseURL + "/projects")
	if err != nil {
		t.Fatalf("GET /projects: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("/projects status = %d; body=%s", listResp.StatusCode, string(listBody))
	}
	if !strings.Contains(string(listBody), "You don't own any projects yet") {
		t.Errorf("/projects missing empty-state copy; body=%s", string(listBody))
	}
	if strings.Contains(string(listBody), "portal-smoke") {
		t.Errorf("/projects must not leak apikey-owned project; body=%s", string(listBody))
	}

	// 4. Detail page for the apikey-owned project must 404 for the dev user.
	detailResp, err := client.Get(baseURL + "/projects/" + apikeyProjID)
	if err != nil {
		t.Fatalf("GET /projects/{id}: %v", err)
	}
	detailBody, _ := io.ReadAll(detailResp.Body)
	detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner detail status = %d, want 404; body=%s", detailResp.StatusCode, string(detailBody))
	}
	if strings.Contains(string(detailBody), "portal-smoke") {
		t.Errorf("404 body leaked project name: %s", string(detailBody))
	}

	// 5. /projects/{id}/ledger for the apikey-owned project must also 404
	//    for the dev session user.
	ledgerResp, err := client.Get(baseURL + "/projects/" + apikeyProjID + "/ledger")
	if err != nil {
		t.Fatalf("GET /projects/{id}/ledger: %v", err)
	}
	ledgerBody, _ := io.ReadAll(ledgerResp.Body)
	ledgerResp.Body.Close()
	if ledgerResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner ledger status = %d, want 404; body=%s", ledgerResp.StatusCode, string(ledgerBody))
	}
	if strings.Contains(string(ledgerBody), "portal-smoke") {
		t.Errorf("ledger 404 body leaked project name: %s", string(ledgerBody))
	}

	// 6. /projects/{id}/stories and /projects/{id}/stories/anything must
	//    also 404 for cross-owner access.
	storiesResp, err := client.Get(baseURL + "/projects/" + apikeyProjID + "/stories")
	if err != nil {
		t.Fatalf("GET /projects/{id}/stories: %v", err)
	}
	storiesBody, _ := io.ReadAll(storiesResp.Body)
	storiesResp.Body.Close()
	if storiesResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner stories list status = %d, want 404; body=%s", storiesResp.StatusCode, string(storiesBody))
	}

	storyDetResp, err := client.Get(baseURL + "/projects/" + apikeyProjID + "/stories/sty_abc12345")
	if err != nil {
		t.Fatalf("GET /projects/{id}/stories/{story_id}: %v", err)
	}
	storyDetBody, _ := io.ReadAll(storyDetResp.Body)
	storyDetResp.Body.Close()
	if storyDetResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner story detail status = %d, want 404; body=%s", storyDetResp.StatusCode, string(storyDetBody))
	}
}

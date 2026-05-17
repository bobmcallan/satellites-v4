// satellites_init_test.go — sty_796b8fe1 end-to-end coverage.
//
// Boots the real satellites-server image via the testcontainers
// harness, points its SATELLITES_MANIFEST_URL at a host-side
// httptest.Server, and asserts the satellites_init verb returns the
// canonical install/refresh payload across all three state-machine
// cases via both the typed /api/v1 surface and the legacy /mcp tools/
// call envelope. Honours pr_local_iteration — the test runs against
// the full substrate boot, not a stub.
//
// Also enforces the seed-sweep-clean predicate (AC5) as a sub-test so
// regressions surface on every CI run, not only at the develop
// commit.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestSatellitesInit is the AC8 evidence anchor.
func TestSatellitesInit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration harness requires a posix shell + docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping testcontainers-backed test")
	}

	// Fake manifest server. The container fetches this URL when the
	// CLI calls /api/v1/satellites/init or the MCP satellites_init.
	manifestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
		    "version":"0.0.300",
		    "build":"2026-05-13-15-00-00",
		    "commit":"abc12345",
		    "artifacts":[
		      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"aaaa","download_url":"https://github.com/example/satellites/releases/download/v0.0.300/satellites-client-linux-amd64"},
		      {"os":"linux","arch":"arm64","filename":"satellites-client-linux-arm64","sha256":"bbbb","download_url":"https://github.com/example/satellites/releases/download/v0.0.300/satellites-client-linux-arm64"}
		    ]
		}`)
	}))
	t.Cleanup(manifestSrv.Close)
	manifestForContainer := strings.Replace(manifestSrv.URL, "127.0.0.1", "host.docker.internal", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const bearer = "key_init_test"
	baseURL, stop := startServerContainerWithEnv(t, ctx, map[string]string{
		"SATELLITES_MANIFEST_URL": manifestForContainer,
		"SATELLITES_API_KEYS":     bearer,
	})
	defer stop()

	for _, tc := range []struct {
		name           string
		currentVersion string
		wantState      string
		wantFilename   string
	}{
		{name: "install_required", currentVersion: "", wantState: "install_required", wantFilename: "satellites-client-linux-amd64"},
		{name: "update_available", currentVersion: "0.0.299", wantState: "update_available", wantFilename: "satellites-client-linux-amd64"},
		{name: "up_to_date", currentVersion: "0.0.300", wantState: "up_to_date", wantFilename: "satellites-client-linux-amd64"},
	} {
		tc := tc
		t.Run(tc.name+"/api_v1", func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"current_version": tc.currentVersion,
				"os":              "linux",
				"arch":            "amd64",
			})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/satellites/init", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
			}
			var got struct {
				State             string `json:"state"`
				TargetInstallPath string `json:"target_install_path"`
				TargetConfigPath  string `json:"target_config_path"`
				DefaultConfig     struct {
					WorktreeRoot   string `json:"worktree_root"`
					LogPath        string `json:"log_path"`
					BranchTemplate string `json:"branch_template"`
				} `json:"default_config"`
				Install struct {
					Version     string `json:"version"`
					Filename    string `json:"filename"`
					DownloadURL string `json:"download_url"`
					SHA256      string `json:"sha256"`
				} `json:"install"`
				AuthBootstrap struct {
					Kind string `json:"kind"`
				} `json:"auth_bootstrap"`
				CurrentVersion string `json:"current_version"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode: %v\n%s", err, string(raw))
			}
			if got.State != tc.wantState {
				t.Errorf("state=%q want=%q (body=%s)", got.State, tc.wantState, string(raw))
			}
			if got.TargetInstallPath != "./.satellites/satellites-client" {
				t.Errorf("target_install_path=%q", got.TargetInstallPath)
			}
			if got.TargetConfigPath != "./.satellites/satellites-client.toml" {
				t.Errorf("target_config_path=%q", got.TargetConfigPath)
			}
			if got.DefaultConfig.WorktreeRoot != "./.satellites/worktree" ||
				got.DefaultConfig.LogPath != "./.satellites/logs" ||
				got.DefaultConfig.BranchTemplate != "client-{task_id}-from-{base_sha}" {
				t.Errorf("default_config drift: %+v", got.DefaultConfig)
			}
			if got.Install.Filename != tc.wantFilename {
				t.Errorf("install.filename=%q want=%q", got.Install.Filename, tc.wantFilename)
			}
			// AC6: update_available carries both download_url and
			// sha256.
			if tc.wantState == "update_available" {
				if got.Install.DownloadURL == "" || got.Install.SHA256 == "" {
					t.Errorf("update_available payload missing download_url/sha256: %+v", got.Install)
				}
			}
			if got.AuthBootstrap.Kind != "auth_login" {
				t.Errorf("auth_bootstrap.kind=%q", got.AuthBootstrap.Kind)
			}
			if got.CurrentVersion != tc.currentVersion {
				t.Errorf("current_version=%q want=%q", got.CurrentVersion, tc.currentVersion)
			}
		})
	}

	// MCP-tier sub-case: drive the same verb through the legacy
	// /mcp tools/call envelope (one state is enough; the typed
	// surface is shared so parity is structural).
	t.Run("update_available/mcp", func(t *testing.T) {
		// MCP requires the initialize handshake before tools/call —
		// reuse the harness helper for parity.
		mcpURL := baseURL + "/mcp"
		rpcInit(t, ctx, mcpURL, bearer)
		out := callTool(t, ctx, mcpURL, bearer, "satellites_init", map[string]any{
			"current_version": "0.0.299",
			"os":              "linux",
			"arch":            "amd64",
		})
		state, _ := out["state"].(string)
		if state != "update_available" {
			t.Errorf("mcp state=%q want=update_available (raw=%+v)", state, out)
		}
		install, _ := out["install"].(map[string]any)
		if install["filename"] != "satellites-client-linux-amd64" {
			t.Errorf("mcp install.filename=%v", install["filename"])
		}
	})

	// AC6 idempotency: a second /api/v1 call with the same args is
	// byte-identical modulo fetched_at.
	t.Run("idempotency", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"current_version": "0.0.300",
			"os":              "linux",
			"arch":            "amd64",
		})
		fetch := func() map[string]any {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/satellites/init", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("decode: %v", err)
			}
			delete(m, "fetched_at")
			return m
		}
		a := fetch()
		b := fetch()
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("idempotency drift (modulo fetched_at):\n%s\n%s", string(ja), string(jb))
		}
	})

	t.Run("seed_sweep_clean", func(t *testing.T) {
		// AC5 — `git grep -nE '\.satellites-agents/|agent-\{?task_id|\./satellites-client/'`
		// returns zero hits across config/seed/** and
		// internal/agent/worker/* docstrings (the worker test files
		// are scoped out per the plan — test fixtures pin TOML
		// overrides, not docstrings).
		root := repoRoot(t)
		cmd := exec.CommandContext(ctx, "git", "grep", "-nE",
			`\.satellites-agents/|agent-\{?task_id|\./satellites-client/`,
			"--", "config/seed/", "internal/agent/worker/")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		// git grep exits 1 when no match (the desired state).
		hits := strings.TrimSpace(string(out))
		if hits == "" {
			return
		}
		// Filter test-file hits per the plan's scope.
		testFile := regexp.MustCompile(`_test\.go(?::|$)`)
		var leaked []string
		for _, line := range strings.Split(hits, "\n") {
			if testFile.MatchString(line) {
				continue
			}
			leaked = append(leaked, line)
		}
		if len(leaked) > 0 {
			t.Errorf("seed_sweep_clean: legacy path strings still present in config/seed or worker docstrings:\n%s\n(grep err=%v)", strings.Join(leaked, "\n"), err)
		}
	})

	// Sty_245a95bf: integration coverage for the project-bound session→mint
	// path. The upstream sub-cases boot a no-DB stack which has neither
	// sessionStore nor projStore, so callerActiveProjectID returns "" and
	// satellites_init falls back to auth_login. These two sub-cases boot a
	// second stack with Surreal + the docs bind-mount so the Mcp-Session-Id
	// header path through AuthMiddleware → callerActiveProjectID →
	// Sessions.Get is actually exercised, mirroring the live pprod flow that
	// the unit-level fix in 489e781 addressed.
	sessionStackCtx, sessionStackCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(sessionStackCancel)

	net, err := network.New(sessionStackCtx)
	if err != nil {
		t.Fatalf("session-bound stack: create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(context.Background()) })

	surreal, err := testcontainers.GenericContainer(sessionStackCtx, testcontainers.GenericContainerRequest{
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
		t.Fatalf("session-bound stack: start surrealdb: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(context.Background()) })

	const sessionBearer = "key_init_bound"
	docsHost := filepath.Join(repoRoot(t), "docs")
	sessionBaseURL, sessionStop := startServerContainerWithOptions(t, sessionStackCtx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"SATELLITES_DB_DSN":       "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS":     sessionBearer,
			"SATELLITES_DOCS_DIR":     "/app/docs",
			"SATELLITES_MANIFEST_URL": manifestForContainer,
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	t.Cleanup(sessionStop)
	sessionMcpURL := sessionBaseURL + "/mcp"

	// AC1 — project_set + satellites_init drive the same Mcp-Session-Id
	// header so callerActiveProjectID resolves the bound project and slice
	// B mints a fresh agent_api_key.
	t.Run("kind_ready_oauth_bearer_session_bound", func(t *testing.T) {
		sessionID := rpcInitGetSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer)

		repoURL := "https://example.invalid/sty_245a95bf-ac1"
		added := rpcCallWithSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer, sessionID, map[string]any{
			"jsonrpc": "2.0", "id": 10, "method": "tools/call",
			"params": map[string]any{
				"name": "project_add",
				"arguments": map[string]any{
					"name":     "sty_245a95bf_ac1",
					"repo_url": repoURL,
				},
			},
		})
		var addedPV map[string]any
		if err := json.Unmarshal([]byte(extractToolText(t, added)), &addedPV); err != nil {
			t.Fatalf("decode project_add: %v", err)
		}
		pid, _ := addedPV["id"].(string)
		if !strings.HasPrefix(pid, "proj_") {
			t.Fatalf("project_add returned no id; resp=%+v", addedPV)
		}

		bound := rpcCallWithSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer, sessionID, map[string]any{
			"jsonrpc": "2.0", "id": 11, "method": "tools/call",
			"params": map[string]any{
				"name":      "project_set",
				"arguments": map[string]any{"repo_url": repoURL},
			},
		})
		var boundPV map[string]any
		if err := json.Unmarshal([]byte(extractToolText(t, bound)), &boundPV); err != nil {
			t.Fatalf("decode project_set: %v", err)
		}
		if boundPV["status"] != "resolved" {
			t.Fatalf("project_set status=%v want resolved (resp=%+v)", boundPV["status"], boundPV)
		}
		if boundPV["project_id"] != pid {
			t.Fatalf("project_set project_id=%v want %s", boundPV["project_id"], pid)
		}

		init := rpcCallWithSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer, sessionID, map[string]any{
			"jsonrpc": "2.0", "id": 12, "method": "tools/call",
			"params": map[string]any{
				"name": "satellites_init",
				"arguments": map[string]any{
					"os":   "linux",
					"arch": "amd64",
				},
			},
		})
		initText := extractToolText(t, init)
		t.Logf("satellites_init payload (kind_ready_oauth_bearer_session_bound):\n%s", initText)
		var initPV map[string]any
		if err := json.Unmarshal([]byte(initText), &initPV); err != nil {
			t.Fatalf("decode satellites_init: %v", err)
		}
		ab, _ := initPV["auth_bootstrap"].(map[string]any)
		if ab == nil {
			t.Fatalf("satellites_init missing auth_bootstrap: %s", initText)
		}
		if got, _ := ab["kind"].(string); got != "ready" {
			t.Errorf("auth_bootstrap.kind=%q want %q (raw=%s)", got, "ready", initText)
		}
		if got, _ := ab["source"].(string); got != "minted_at_init" {
			t.Errorf("auth_bootstrap.source=%q want %q (raw=%s)", got, "minted_at_init", initText)
		}
		key, _ := initPV["agent_api_key"].(map[string]any)
		if key == nil {
			t.Fatalf("satellites_init missing agent_api_key: %s", initText)
		}
		if k, _ := key["key"].(string); k == "" {
			t.Errorf("agent_api_key.key empty on minted_at_init (raw=%s)", initText)
		}
		if src, _ := key["source"].(string); src != "minted_at_init" {
			t.Errorf("agent_api_key.source=%q want %q (raw=%s)", src, "minted_at_init", initText)
		}
	})

	// AC2 — second satellites_init with the same (Mcp-Session-Id, agent_name)
	// must return existing_key + empty cleartext (the unit-fix's idempotency
	// guarantee, now covered over HTTP).
	t.Run("kind_ready_idempotent_on_existing_key", func(t *testing.T) {
		sessionID := rpcInitGetSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer)

		repoURL := "https://example.invalid/sty_245a95bf-ac2"
		added := rpcCallWithSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer, sessionID, map[string]any{
			"jsonrpc": "2.0", "id": 20, "method": "tools/call",
			"params": map[string]any{
				"name": "project_add",
				"arguments": map[string]any{
					"name":     "sty_245a95bf_ac2",
					"repo_url": repoURL,
				},
			},
		})
		var addedPV map[string]any
		if err := json.Unmarshal([]byte(extractToolText(t, added)), &addedPV); err != nil {
			t.Fatalf("decode project_add: %v", err)
		}
		if pid, _ := addedPV["id"].(string); !strings.HasPrefix(pid, "proj_") {
			t.Fatalf("project_add returned no id; resp=%+v", addedPV)
		}

		bound := rpcCallWithSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer, sessionID, map[string]any{
			"jsonrpc": "2.0", "id": 21, "method": "tools/call",
			"params": map[string]any{
				"name":      "project_set",
				"arguments": map[string]any{"repo_url": repoURL},
			},
		})
		var boundPV map[string]any
		if err := json.Unmarshal([]byte(extractToolText(t, bound)), &boundPV); err != nil {
			t.Fatalf("decode project_set: %v", err)
		}
		if boundPV["status"] != "resolved" {
			t.Fatalf("project_set status=%v want resolved", boundPV["status"])
		}

		callInit := func(id int) map[string]any {
			resp := rpcCallWithSessionID(t, sessionStackCtx, sessionMcpURL, sessionBearer, sessionID, map[string]any{
				"jsonrpc": "2.0", "id": id, "method": "tools/call",
				"params": map[string]any{
					"name": "satellites_init",
					"arguments": map[string]any{
						"os":   "linux",
						"arch": "amd64",
					},
				},
			})
			text := extractToolText(t, resp)
			var out map[string]any
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatalf("decode satellites_init: %v", err)
			}
			return out
		}

		first := callInit(22)
		firstKey, _ := first["agent_api_key"].(map[string]any)
		if firstKey == nil {
			firstText, _ := json.Marshal(first)
			t.Fatalf("first satellites_init missing agent_api_key: %s", string(firstText))
		}
		if src, _ := firstKey["source"].(string); src != "minted_at_init" {
			firstText, _ := json.Marshal(first)
			t.Fatalf("first call agent_api_key.source=%q want minted_at_init (raw=%s)", src, string(firstText))
		}

		second := callInit(23)
		secondText, _ := json.Marshal(second)
		t.Logf("second satellites_init payload (kind_ready_idempotent_on_existing_key):\n%s", string(secondText))
		secondKey, _ := second["agent_api_key"].(map[string]any)
		if secondKey == nil {
			t.Fatalf("second satellites_init missing agent_api_key: %s", string(secondText))
		}
		if src, _ := secondKey["source"].(string); src != "existing_key" {
			t.Errorf("second call agent_api_key.source=%q want existing_key (raw=%s)", src, string(secondText))
		}
		if k, _ := secondKey["key"].(string); k != "" {
			t.Errorf("second call agent_api_key.key=%q want empty (cleartext unrecoverable on re-read) (raw=%s)", k, string(secondText))
		}
	})
}

// rpcInitGetSessionID drives the MCP initialize handshake and returns the
// Mcp-Session-Id the server minted (mcp-go's StatelessGeneratingSessionIdManager
// returns it via the response header per the 2025-03-26 spec). Subsequent
// calls echo that id on the Mcp-Session-Id request header so the server's
// callerActiveProjectID helper resolves the same session row across calls.
// sty_245a95bf.
func rpcInitGetSessionID(t *testing.T, ctx context.Context, mcpURL, apiKey string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatalf("initialize did not return Mcp-Session-Id header (status=%d)", resp.StatusCode)
	}
	return sessionID
}

// rpcCallWithSessionID mirrors rpcCall but echoes the Mcp-Session-Id
// header so AuthMiddleware → callerActiveProjectID → Sessions.Get resolves
// the same session row across adjacent calls. sty_245a95bf.
func rpcCallWithSessionID(t *testing.T, ctx context.Context, mcpURL, apiKey, sessionID string, body any) map[string]any {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("rpc status = %d; body=%s", resp.StatusCode, string(b))
	}
	raw, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				var out map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &out); err != nil {
					t.Fatalf("sse decode: %v; raw=%s", err, string(raw))
				}
				return out
			}
		}
		t.Fatalf("no data: line in SSE response; raw=%s", string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json decode: %v; raw=%s", err, string(raw))
	}
	return out
}

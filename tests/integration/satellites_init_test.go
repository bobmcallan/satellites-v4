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
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
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
}

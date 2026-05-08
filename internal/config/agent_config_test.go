package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCWD chdirs to dir for the duration of the test, restoring on
// cleanup. The test must register cleanup before any other failure
// path runs.
func withCWD(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// withEnv sets env var key=value and restores the prior value (or
// unsets) on cleanup.
func withEnv(t *testing.T, key, value string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	require.NoError(t, os.Setenv(key, value))
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// withHome overrides HOME so the XDG-style fallback resolves to a
// tmpdir under our control.
func withHome(t *testing.T, dir string) {
	t.Helper()
	withEnv(t, "HOME", dir)
}

func TestLoadAgent_DefaultsOnly(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withEnv(t, agentConfigPathEnv, "")
	withHome(t, tmp)

	cfg, warnings, err := LoadAgent("")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "http://localhost:8080/mcp", cfg.MCPURL)
	assert.Equal(t, 5*time.Second, cfg.IdleBackoff)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "ws://localhost:8080/ws", cfg.HubURL)
	assert.Equal(t, "", cfg.AuthToken)
	assert.Equal(t, "", cfg.LoadedTOMLPath())
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "no file resolved")
}

func TestLoadAgent_FlagOverridesEnv(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)

	flagFile := filepath.Join(tmp, "flag.toml")
	envFile := filepath.Join(tmp, "env.toml")
	require.NoError(t, os.WriteFile(flagFile, []byte(`mcp_url = "http://flag/mcp"`), 0o600))
	require.NoError(t, os.WriteFile(envFile, []byte(`mcp_url = "http://env/mcp"`), 0o600))
	withEnv(t, agentConfigPathEnv, envFile)

	cfg, _, err := LoadAgent(flagFile)
	require.NoError(t, err)
	assert.Equal(t, "http://flag/mcp", cfg.MCPURL)
	assert.Equal(t, flagFile, cfg.LoadedTOMLPath())
}

func TestLoadAgent_EnvOverridesCwd(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)

	envFile := filepath.Join(tmp, "env.toml")
	cwdFile := filepath.Join(tmp, agentDefaultConfigFile)
	require.NoError(t, os.WriteFile(envFile, []byte(`mcp_url = "http://env/mcp"`), 0o600))
	require.NoError(t, os.WriteFile(cwdFile, []byte(`mcp_url = "http://cwd/mcp"`), 0o600))
	withEnv(t, agentConfigPathEnv, envFile)

	cfg, _, err := LoadAgent("")
	require.NoError(t, err)
	assert.Equal(t, "http://env/mcp", cfg.MCPURL)
	assert.Equal(t, envFile, cfg.LoadedTOMLPath())
}

func TestLoadAgent_CwdOverridesXDG(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	cwdFile := filepath.Join(tmp, agentDefaultConfigFile)
	xdgDir := filepath.Join(tmp, ".config", "satellites-agent")
	require.NoError(t, os.MkdirAll(xdgDir, 0o700))
	xdgFile := filepath.Join(xdgDir, "config.toml")
	require.NoError(t, os.WriteFile(cwdFile, []byte(`mcp_url = "http://cwd/mcp"`), 0o600))
	require.NoError(t, os.WriteFile(xdgFile, []byte(`mcp_url = "http://xdg/mcp"`), 0o600))

	cfg, _, err := LoadAgent("")
	require.NoError(t, err)
	assert.Equal(t, "http://cwd/mcp", cfg.MCPURL)
}

func TestLoadAgent_XDGFallback(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	xdgDir := filepath.Join(tmp, ".config", "satellites-agent")
	require.NoError(t, os.MkdirAll(xdgDir, 0o700))
	xdgFile := filepath.Join(xdgDir, "config.toml")
	require.NoError(t, os.WriteFile(xdgFile, []byte(`mcp_url = "http://xdg/mcp"`), 0o600))

	cfg, _, err := LoadAgent("")
	require.NoError(t, err)
	assert.Equal(t, "http://xdg/mcp", cfg.MCPURL)
	assert.Equal(t, xdgFile, cfg.LoadedTOMLPath())
}

func TestLoadAgent_ExplicitMissingIsFatal(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	missing := filepath.Join(tmp, "no-such.toml")
	cfg, warnings, err := LoadAgent(missing)
	assert.Error(t, err)
	assert.Nil(t, cfg)
	assert.Nil(t, warnings)
}

func TestLoadAgent_ExplicitEnvMissingIsFatal(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, filepath.Join(tmp, "no-such.toml"))

	cfg, _, err := LoadAgent("")
	assert.Error(t, err)
	assert.Nil(t, cfg)
}

func TestLoadAgent_DefaultPathMissingIsSilent(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	cfg, warnings, err := LoadAgent("")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "no file resolved")
}

func TestLoadAgent_TOMLFieldRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	body := `
worker_id = "worker_x"
workspace_ids = ["wksp_a", "wksp_b"]
mcp_url = "http://srv:9000/mcp"
auth_token = "tok-secret"
idle_backoff = "7s"
heartbeat_interval = "30s"
execute_timeout = "5m"
repo_path = "/srv/repo"
branch_template = "agent-{task_id}"
worktree_root = "/tmp/worktrees/"
claude_binary_path = "/usr/local/bin/claude"
log_level = "debug"

hub_url = "wss://srv:9000/ws"
subscribe_workspace_ids = ["wksp_a"]
subscribe_since_id = "00000000000000000010"
ws_reconnect_min_backoff = "250ms"
ws_reconnect_max_backoff = "1m"
`
	path := filepath.Join(tmp, "agent.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, warnings, err := LoadAgent(path)
	require.NoError(t, err)
	require.Empty(t, warnings)

	assert.Equal(t, "worker_x", cfg.WorkerID)
	assert.Equal(t, []string{"wksp_a", "wksp_b"}, cfg.WorkspaceIDs)
	assert.Equal(t, "http://srv:9000/mcp", cfg.MCPURL)
	assert.Equal(t, "tok-secret", cfg.AuthToken)
	assert.Equal(t, 7*time.Second, cfg.IdleBackoff)
	assert.Equal(t, 30*time.Second, cfg.HeartbeatInterval)
	assert.Equal(t, 5*time.Minute, cfg.ExecuteTimeout)
	assert.Equal(t, "/srv/repo", cfg.RepoPath)
	assert.Equal(t, "agent-{task_id}", cfg.BranchTemplate)
	assert.Equal(t, "/tmp/worktrees/", cfg.WorktreeRoot)
	assert.Equal(t, "/usr/local/bin/claude", cfg.ClaudeBinaryPath)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "wss://srv:9000/ws", cfg.HubURL)
	assert.Equal(t, []string{"wksp_a"}, cfg.SubscribeWorkspaceIDs)
	assert.Equal(t, "00000000000000000010", cfg.SubscribeSinceID)
	assert.Equal(t, 250*time.Millisecond, cfg.WSReconnectMinBackoff)
	assert.Equal(t, time.Minute, cfg.WSReconnectMaxBackoff)
}

func TestLoadAgent_InvalidDurationDegrades(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	// "fortnight" is unparseable — go-toml/v2 reports a top-level parse
	// error which a non-fatal cwd/xdg path degrades to a warning. Use
	// the cwd source so the loader degrades; explicit flag is fatal.
	body := `idle_backoff = "fortnight"` + "\n"
	path := filepath.Join(tmp, agentDefaultConfigFile)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, warnings, err := LoadAgent("")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	// Defaults preserved when overlay parse fails.
	assert.Equal(t, 5*time.Second, cfg.IdleBackoff)
	require.NotEmpty(t, warnings)
	assert.True(t, strings.Contains(warnings[0], "parse failed") || strings.Contains(warnings[0], "fortnight"),
		"expected parse-failed warning, got %q", warnings[0])
}

func TestLoadAgent_InvalidLogLevelDegrades(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	withHome(t, tmp)
	withEnv(t, agentConfigPathEnv, "")

	body := `log_level = "shouty"` + "\n"
	path := filepath.Join(tmp, agentDefaultConfigFile)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, warnings, err := LoadAgent("")
	require.NoError(t, err)
	assert.Equal(t, "info", cfg.LogLevel)
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0], "log_level")
}

func TestLoadAgent_AuthTokenNeverLogged(t *testing.T) {
	cfg := &AgentConfig{AuthToken: "super-secret-deadbeef", MCPURL: "http://x/mcp"}
	rendered := cfg.String()
	assert.NotContains(t, rendered, "super-secret-deadbeef")
	assert.Contains(t, rendered, "auth_token=***")
}

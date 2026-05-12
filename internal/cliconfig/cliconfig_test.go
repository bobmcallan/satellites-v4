package cliconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/cliconfig"
)

// TestLoad_Defaults: no flag, no env, no on-disk file resolves → empty
// values for transport/auth and operational defaults for the runtime
// fields. No error, no warnings, LoadedTOMLPath="".
func TestLoad_Defaults(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	chdirToEmpty(t)

	cfg, warnings, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server != "" || cfg.Token != "" {
		t.Fatalf("expected empty auth defaults, got %s", cfg)
	}
	if cfg.RepoPath != "." {
		t.Fatalf("expected repo_path default '.', got %q", cfg.RepoPath)
	}
	if cfg.WorktreeRoot != ".satellites-agents/" {
		t.Fatalf("expected worktree_root default '.satellites-agents/', got %q", cfg.WorktreeRoot)
	}
	if cfg.BranchTemplate != "agent-{task_id}-from-{base_sha}" {
		t.Fatalf("expected branch_template default, got %q", cfg.BranchTemplate)
	}
	if cfg.ExecuteTimeout != 30*time.Minute {
		t.Fatalf("expected execute_timeout 30m, got %s", cfg.ExecuteTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("expected log_level 'info', got %q", cfg.LogLevel)
	}
	if cfg.LoadedTOMLPath() != "" {
		t.Fatalf("expected no loaded path, got %q", cfg.LoadedTOMLPath())
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

// TestLoad_BinDir: bin/satellites-client.toml is the implicit path the
// operator drops a TOML next to the binary at. Operational fields
// override defaults.
func TestLoad_BinDir(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	wd := chdirToEmpty(t)
	binDir := filepath.Join(wd, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "satellites-client.toml"),
		[]byte(`server = "https://test.example/mcp"
token = "bin-token"
oauth_enabled = true
repo_path = "/repo"
worktree_root = "/tmp/wt"
branch_template = "feature-{task_id}"
execute_timeout = "5m"
log_level = "debug"
log_path = "/tmp/logs"
`),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, warnings, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server != "https://test.example/mcp" {
		t.Fatalf("server: got %q", cfg.Server)
	}
	if cfg.Token != "bin-token" {
		t.Fatalf("token: got %q", cfg.Token)
	}
	if !cfg.OAuthEnabled {
		t.Fatalf("oauth_enabled: got false, want true")
	}
	if cfg.RepoPath != "/repo" {
		t.Fatalf("repo_path: got %q", cfg.RepoPath)
	}
	if cfg.WorktreeRoot != "/tmp/wt" {
		t.Fatalf("worktree_root: got %q", cfg.WorktreeRoot)
	}
	if cfg.BranchTemplate != "feature-{task_id}" {
		t.Fatalf("branch_template: got %q", cfg.BranchTemplate)
	}
	if cfg.ExecuteTimeout != 5*time.Minute {
		t.Fatalf("execute_timeout: got %s", cfg.ExecuteTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("log_level: got %q", cfg.LogLevel)
	}
	if cfg.LogPath != "/tmp/logs" {
		t.Fatalf("log_path: got %q", cfg.LogPath)
	}
	if cfg.LoadedTOMLPath() == "" {
		t.Fatalf("expected loaded path, got empty")
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

// TestLoad_WalkUp: from a nested subdirectory inside the repo, the
// loader walks up to find bin/satellites-client.toml. The walk halts
// at a .git directory (the repo root) so it can never resolve a TOML
// outside the current repo.
//
// Layout under <root>:
//   .git/
//   bin/satellites-client.toml
//   sub/deep/                  ← test chdirs here
//
// Expected: the loader returns <root>/bin/satellites-client.toml even
// though CWD is two levels below.
func TestLoad_WalkUp(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	tomlPath := filepath.Join(binDir, "satellites-client.toml")
	if err := os.WriteFile(tomlPath, []byte(`server = "https://walk.example/mcp"
token = "walked-up"
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	deep := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}

	prevWD, _ := os.Getwd()
	if err := os.Chdir(deep); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	cfg, warnings, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolveSymlinks := func(p string) string {
		out, err := filepath.EvalSymlinks(p)
		if err != nil {
			return p
		}
		return out
	}
	gotPath := resolveSymlinks(cfg.LoadedTOMLPath())
	wantPath := resolveSymlinks(tomlPath)
	if gotPath != wantPath {
		t.Fatalf("loaded path mismatch: got %q want %q", gotPath, wantPath)
	}
	if cfg.Server != "https://walk.example/mcp" {
		t.Fatalf("server: got %q", cfg.Server)
	}
	if cfg.Token != "walked-up" {
		t.Fatalf("token: got %q", cfg.Token)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
}

// TestLoad_WalkUpStopsAtRepoBoundary: the walk-up halts when it sees
// a .git directory whose bin/satellites-client.toml is absent. The
// outer repo's TOML must not resolve from inside an inner repo —
// otherwise a dispatched subprocess could accidentally pick up the
// wrong project's bearer.
//
// Layout under <outer>:
//   bin/satellites-client.toml ← outer config, must NOT resolve
//   inner/.git/
//   inner/sub/                  ← test chdirs here
func TestLoad_WalkUpStopsAtRepoBoundary(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	outer := t.TempDir()
	outerBin := filepath.Join(outer, "bin")
	if err := os.MkdirAll(outerBin, 0o755); err != nil {
		t.Fatalf("mkdir outer bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outerBin, "satellites-client.toml"),
		[]byte(`server = "outer"
token = "outer"
`), 0o600); err != nil {
		t.Fatalf("write outer toml: %v", err)
	}
	innerGit := filepath.Join(outer, "inner", ".git")
	if err := os.MkdirAll(innerGit, 0o755); err != nil {
		t.Fatalf("mkdir inner/.git: %v", err)
	}
	innerSub := filepath.Join(outer, "inner", "sub")
	if err := os.MkdirAll(innerSub, 0o755); err != nil {
		t.Fatalf("mkdir inner/sub: %v", err)
	}

	prevWD, _ := os.Getwd()
	if err := os.Chdir(innerSub); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	cfg, _, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LoadedTOMLPath() != "" {
		t.Fatalf("expected walk to halt at inner/.git/, got %q", cfg.LoadedTOMLPath())
	}
	if cfg.Server != "" || cfg.Token != "" {
		t.Fatalf("outer TOML leaked across repo boundary: server=%q token=%q", cfg.Server, cfg.Token)
	}
}

// TestLoad_XDG: ~/.config/satellites-client/config.toml is the
// fallback when bin/ has nothing.
func TestLoad_XDG(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	chdirToEmpty(t)

	xdgDir := filepath.Join(dir, ".config", "satellites-client")
	if err := os.MkdirAll(xdgDir, 0o755); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xdgDir, "config.toml"),
		[]byte("token = \"xdg-token\"\nrepo_path = \"/repo/xdg\"\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, _, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "xdg-token" {
		t.Fatalf("token: got %q", cfg.Token)
	}
	if cfg.RepoPath != "/repo/xdg" {
		t.Fatalf("repo_path: got %q", cfg.RepoPath)
	}
}

// TestLoad_ExplicitFlag: --config <path> wins over the implicit search
// chain.
func TestLoad_ExplicitFlag(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	chdirToEmpty(t)

	explicit := filepath.Join(dir, "explicit.toml")
	if err := os.WriteFile(explicit,
		[]byte("token = \"flag-token\"\nserver = \"https://flag.example/mcp\"\nrepo_path = \"/flag/repo\"\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, _, err := cliconfig.Load(explicit)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "flag-token" || cfg.Server != "https://flag.example/mcp" || cfg.RepoPath != "/flag/repo" {
		t.Fatalf("explicit flag values not picked up: %s", cfg)
	}
}

// TestLoad_ExplicitFlagMissing: an explicit --config pointed at a
// non-existent path is fatal.
func TestLoad_ExplicitFlagMissing(t *testing.T) {
	_, _, err := cliconfig.Load("/definitely/does/not/exist.toml")
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

// TestLoad_EnvPath: SATELLITES_CLIENT_CONFIG resolves like the flag
// (fatal-on-missing).
func TestLoad_EnvPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	chdirToEmpty(t)

	envPath := filepath.Join(dir, "env.toml")
	if err := os.WriteFile(envPath,
		[]byte("token = \"env-path-token\"\nexecute_timeout = \"45m\"\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("SATELLITES_CLIENT_CONFIG", envPath)

	cfg, _, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "env-path-token" {
		t.Fatalf("token: got %q", cfg.Token)
	}
	if cfg.ExecuteTimeout != 45*time.Minute {
		t.Fatalf("execute_timeout: got %s", cfg.ExecuteTimeout)
	}
}

// TestLoad_StringMasksToken: Config.String never leaks the token
// verbatim.
func TestLoad_StringMasksToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")

	wd := chdirToEmpty(t)
	binDir := filepath.Join(wd, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "satellites-client.toml"),
		[]byte("token = \"sat_topsecret\"\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, _, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered := cfg.String()
	if strings.Contains(rendered, "sat_topsecret") {
		t.Fatalf("String() leaked token: %s", rendered)
	}
	if !strings.Contains(rendered, "***") {
		t.Fatalf("String() did not mask token: %s", rendered)
	}
}

// TestLoad_MalformedImplicitDegrades: malformed bin/ file does NOT
// return an error — surfaces a warning + degrades to defaults.
func TestLoad_MalformedImplicitDegrades(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	wd := chdirToEmpty(t)
	binDir := filepath.Join(wd, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "satellites-client.toml"),
		[]byte("this is not valid = toml = at all"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, warnings, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.Token != "" {
		t.Fatalf("expected empty token on parse failure, got %q", cfg.Token)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected at least one warning")
	}
}

// TestLoad_MalformedExplicitFatal: malformed --config / env file
// returns an error.
func TestLoad_MalformedExplicitFatal(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("not = valid = toml"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := cliconfig.Load(bad)
	if err == nil {
		t.Fatal("expected fatal error for malformed explicit path")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error %q does not mention path %q", err, bad)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("misclassified as not-exist: %v", err)
	}
}

// TestLoad_InvalidLogLevel_DegradesWithWarning: an unknown log_level
// emits a warning and keeps the default — matches LoadAgent behaviour.
func TestLoad_InvalidLogLevel_DegradesWithWarning(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	wd := chdirToEmpty(t)
	binDir := filepath.Join(wd, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "satellites-client.toml"),
		[]byte("log_level = \"chatty\"\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, warnings, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("expected default log_level retained, got %q", cfg.LogLevel)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected warning for invalid log_level")
	}
}

// TestLoad_NegativeExecuteTimeout_DegradesWithWarning: a non-positive
// duration keeps the default. Matches AgentConfig overlay behaviour.
func TestLoad_NegativeExecuteTimeout_DegradesWithWarning(t *testing.T) {
	t.Setenv("SATELLITES_CLIENT_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	wd := chdirToEmpty(t)
	binDir := filepath.Join(wd, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "satellites-client.toml"),
		[]byte("execute_timeout = \"-5m\"\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, warnings, err := cliconfig.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ExecuteTimeout != 30*time.Minute {
		t.Fatalf("expected default execute_timeout retained, got %s", cfg.ExecuteTimeout)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected warning for non-positive timeout")
	}
}

// chdirToEmpty cd's the test into a fresh temp dir and restores cwd
// at the end. Returns the new working directory.
func chdirToEmpty(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	target := t.TempDir()
	if err := os.Chdir(target); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return target
}

// Package cliconfig is the satellites-client runtime configuration. The
// schema covers two surfaces:
//
//  1. Auth + transport (server URL, bearer token, OAuth flag) — what the
//     existing thin-CLI invocations need.
//  2. Operational fields (repo_path, worktree_root, branch_template,
//     execute_timeout, log_*) — the substrate Layer E of sty_3b3e4e66
//     consumes when satellites-client hosts in-process runners
//     (`satellites-client agent run <task_id>`).
//
// The package mirrors `internal/config.AgentConfig`'s resolution chain
// + service-always-boots philosophy. Load resolves config from four
// layered sources, highest first:
//
//  1. --config <path> CLI flag (caller passes the path explicitly).
//  2. SATELLITES_CLIENT_CONFIG env var.
//  3. ./bin/satellites-client.toml (alongside the built binary).
//  4. ~/.config/satellites-client/config.toml (XDG-style fallback).
//
// When no source resolves, defaults are returned with no warnings —
// flags / env / clicred.Resolve take over. Service degradation parity
// with LoadAgent: malformed or unreadable implicit sources emit
// warnings and degrade to defaults; explicit --config / env paths are
// fatal when unreadable/unparseable because the operator clearly meant
// to point at *that* file.

package cliconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the runtime configuration for the satellites-client binary.
type Config struct {
	// Server is the canonical satellites-server URL (full MCP path,
	// e.g. https://satellites-pprod.fly.dev/mcp). When empty, the
	// binary falls back to its compiled-in default.
	Server string `toml:"server"`

	// Token is the api-key bearer the CLI sends as `Authorization:
	// Bearer …`. When empty, the credential-resolution chain falls
	// through to ~/.satellites/credentials.json.
	Token string `toml:"token"`

	// OAuthEnabled is reserved for the order:07a-L3 main slice
	// (sty_068a6c46). Default false; the carve-out (this story) ships
	// the field so operators can populate it without a re-edit when
	// the auth subcommand lands.
	OAuthEnabled bool `toml:"oauth_enabled"`

	// RepoPath is the path to the operator's git working tree the
	// runtime drives (sty_3b3e4e66 Layer E first consumer). Default "."
	// resolves to the CLI's working directory.
	RepoPath string `toml:"repo_path"`

	// WorktreeRoot is the directory where per-task worktrees live.
	// Mirrors satellites-agent's worktree_root. Default
	// ".satellites-agents/" — matches the heavy-path convention.
	WorktreeRoot string `toml:"worktree_root"`

	// BranchTemplate names the branch convention the runtime stamps
	// on per-task worktrees. Substitutions: {task_id}, {base_sha}.
	// Default "agent-{task_id}-from-{base_sha}" matches the
	// satellites-agent default.
	BranchTemplate string `toml:"branch_template"`

	// ExecuteTimeout is the hard ceiling on in-process runner
	// invocations. Default 30m mirrors the agent's ceiling.
	ExecuteTimeout time.Duration `toml:"execute_timeout"`

	// LogLevel is the arbor log level for satellites-client.
	// Default "info". Accepted: trace, debug, info, warn, error.
	LogLevel string `toml:"log_level"`

	// LogPath names the directory for rolling log files. Empty
	// disables the file writer — the CLI runs stdout-only.
	LogPath string `toml:"log_path"`

	// loadedTOMLPath records the path that was actually read; "" when
	// the loader fell back to defaults.
	loadedTOMLPath string
}

// configPathEnv is the env var operators set to point at an explicit
// TOML file when not using --config.
const configPathEnv = "SATELLITES_CLIENT_CONFIG"

// defaultBinFile is the repo-relative file the loader checks when no
// flag or env var resolves. Sits alongside the built binary at
// bin/satellites-client (created by scripts/build-client.sh) so the
// operator can drop a TOML next to the binary without rebuilding.
const defaultBinFile = "bin/satellites-client.toml"

// LoadedTOMLPath returns the path Load actually read, or "" when
// defaults supplied the whole config.
func (c *Config) LoadedTOMLPath() string {
	return c.loadedTOMLPath
}

// String renders the Config with the Token masked. Safe for arbor
// logging.
func (c Config) String() string {
	mask := ""
	if c.Token != "" {
		mask = "***"
	}
	return fmt.Sprintf(
		"Config{server=%q token=%s oauth_enabled=%t repo_path=%q worktree_root=%q "+
			"branch_template=%q execute_timeout=%s log_level=%q log_path=%q}",
		c.Server, mask, c.OAuthEnabled,
		c.RepoPath, c.WorktreeRoot, c.BranchTemplate,
		c.ExecuteTimeout, c.LogLevel, c.LogPath,
	)
}

// defaults builds the in-code default Config. Lowest precedence layer;
// TOML overrides it.
func defaults() *Config {
	// LogPath defaults to <exe_dir>/logs/ — matches satellites-agent's
	// default in internal/config/agent_config.go::defaultsAgent. With the
	// binary at bin/satellites-client, this resolves to bin/logs/.
	// sty_ef4eedaa.
	logDir := ""
	if len(os.Args) > 0 && os.Args[0] != "" {
		logDir = filepath.Join(filepath.Dir(os.Args[0]), "logs")
	}
	return &Config{
		Server:       "",
		Token:        "",
		OAuthEnabled: false,
		RepoPath:     ".",
		// WorktreeRoot + BranchTemplate use a "client-" prefix so
		// satellites-client's per-task worktrees + branches don't
		// collide with satellites-agent's (which use ".satellites-agents/"
		// and "agent-…"). sty_b1345841.
		WorktreeRoot:   ".satellites-clients/",
		BranchTemplate: "client-{task_id}-from-{base_sha}",
		ExecuteTimeout: 30 * time.Minute,
		LogLevel:       "info",
		LogPath:        logDir,
	}
}

// tomlOverlay is the TOML-decoding shadow of Config. Pointer fields
// let the loader distinguish "TOML set this" (non-nil) from "TOML
// didn't mention it" (nil) so the overlay overrides defaults
// selectively. Durations decode via the local duration wrapper so
// operators can write "30m", "5s" instead of nanoseconds.
type tomlOverlay struct {
	Server         *string   `toml:"server"`
	Token          *string   `toml:"token"`
	OAuthEnabled   *bool     `toml:"oauth_enabled"`
	RepoPath       *string   `toml:"repo_path"`
	WorktreeRoot   *string   `toml:"worktree_root"`
	BranchTemplate *string   `toml:"branch_template"`
	ExecuteTimeout *duration `toml:"execute_timeout"`
	LogLevel       *string   `toml:"log_level"`
	LogPath        *string   `toml:"log_path"`
}

// duration wraps time.Duration with a TextUnmarshaler so go-toml/v2 can
// decode Go-duration strings ("5m", "30s", "1h") into the timeout field.
type duration time.Duration

// UnmarshalText parses a Go duration string into the wrapper.
func (d *duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(parsed)
	return nil
}

// validLogLevels mirrors the level strings accepted by internal/arbor.
var validLogLevels = map[string]struct{}{
	"trace": {}, "debug": {}, "info": {}, "warn": {}, "error": {},
}

// Load resolves Config from the precedence chain and returns the
// resulting struct alongside operator-facing warnings. An error is
// returned only when an explicit path (CLI flag or env var) is
// supplied but the file is unreadable or unparseable.
func Load(explicitPath string) (*Config, []string, error) {
	cfg := defaults()
	var warnings []string

	path, source, err := resolveConfigPath(explicitPath)
	if err != nil {
		return nil, nil, err
	}
	if path == "" {
		return cfg, warnings, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if source == sourceFlag || source == sourceEnv {
			return nil, nil, fmt.Errorf("client config: %s path %q unreadable: %w", source, path, err)
		}
		warnings = append(warnings, fmt.Sprintf("client config: %s file %q unreadable: %v — falling back to defaults", source, path, err))
		return cfg, warnings, nil
	}

	var overlay tomlOverlay
	if err := toml.Unmarshal(raw, &overlay); err != nil {
		if source == sourceFlag || source == sourceEnv {
			return nil, nil, fmt.Errorf("client config: %s file %q parse failed: %w", source, path, err)
		}
		warnings = append(warnings, fmt.Sprintf("client config: %s file %q parse failed: %v — falling back to defaults", source, path, err))
		return cfg, warnings, nil
	}

	cfg.loadedTOMLPath = path
	overlay.applyTo(cfg, &warnings)
	return cfg, warnings, nil
}

// config path source markers used in warnings + error messages.
const (
	sourceFlag = "flag"
	sourceEnv  = "env"
	sourceBin  = "bin"
	sourceXDG  = "xdg"
)

// resolveConfigPath walks the precedence chain and returns the first
// file that resolves. An empty path means no source resolved. An error
// is returned only when an explicit source (flag, env) supplied a
// value but no file exists at that path.
func resolveConfigPath(explicit string) (string, string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", sourceFlag, fmt.Errorf("client config: --config path %q not found: %w", p, err)
		}
		return p, sourceFlag, nil
	}
	if p := strings.TrimSpace(os.Getenv(configPathEnv)); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", sourceEnv, fmt.Errorf("client config: %s=%q not found: %w", configPathEnv, p, err)
		}
		return p, sourceEnv, nil
	}
	// Walk-up: search CWD and its ancestors for bin/satellites-client.toml.
	// Stops at the filesystem root or a .git directory (repo boundary), so
	// a dispatched subprocess whose CWD is <repo>/.satellites-agents/<task_id>/
	// finds the operator's repo-root TOML without leaking out of the repo.
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for {
			candidate := filepath.Join(dir, defaultBinFile)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, sourceBin, nil
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		xdg := filepath.Join(home, ".config", "satellites-client", "config.toml")
		if _, err := os.Stat(xdg); err == nil {
			return xdg, sourceXDG, nil
		}
	}
	return "", "", nil
}

// applyTo copies overlay values into cfg for every non-nil field.
// Invalid values append to warnings and leave the field at its prior
// value (parity with LoadAgent).
func (o tomlOverlay) applyTo(cfg *Config, warnings *[]string) {
	if o.Server != nil {
		cfg.Server = strings.TrimSpace(*o.Server)
	}
	if o.Token != nil {
		cfg.Token = strings.TrimSpace(*o.Token)
	}
	if o.OAuthEnabled != nil {
		cfg.OAuthEnabled = *o.OAuthEnabled
	}
	if o.RepoPath != nil {
		cfg.RepoPath = *o.RepoPath
	}
	if o.WorktreeRoot != nil {
		cfg.WorktreeRoot = *o.WorktreeRoot
	}
	if o.BranchTemplate != nil {
		cfg.BranchTemplate = *o.BranchTemplate
	}
	if o.ExecuteTimeout != nil {
		d := time.Duration(*o.ExecuteTimeout)
		if d <= 0 {
			*warnings = append(*warnings, fmt.Sprintf("client config: execute_timeout=%s must be > 0 — keeping %s", d, cfg.ExecuteTimeout))
		} else {
			cfg.ExecuteTimeout = d
		}
	}
	if o.LogLevel != nil {
		lvl := strings.ToLower(*o.LogLevel)
		if _, ok := validLogLevels[lvl]; !ok {
			*warnings = append(*warnings, fmt.Sprintf("client config: log_level=%q invalid — keeping %q", *o.LogLevel, cfg.LogLevel))
		} else {
			cfg.LogLevel = lvl
		}
	}
	if o.LogPath != nil {
		cfg.LogPath = *o.LogPath
	}
}

// AgentConfig is the runtime configuration for the satellites-agent
// binary. The schema covers polling-fallback fields (worker_id,
// workspace_ids, spawn_mcp_url, idle_backoff, heartbeat_interval,
// execute_timeout, repo_path, branch_template, worktree_root,
// claude_binary_path, log_level, auth_token) plus the WS-subscriber
// fields (hub_url, subscribe_workspace_ids, subscribe_since_id,
// ws_reconnect_min_backoff, ws_reconnect_max_backoff).
//
// LoadAgent resolves config from four layered sources, highest first:
//
//  1. --config <path> CLI flag (caller passes the path explicitly).
//  2. SATELLITES_AGENT_CONFIG env var.
//  3. bin/satellites-agent.toml (alongside the built binary).
//  4. ~/.config/satellites-agent/config.toml (XDG-style fallback).
//
// When no source resolves, defaults are returned alongside one
// warning. Service ALWAYS boots: malformed values, parse errors, and
// unparseable durations all degrade to warnings and the affected
// field falls back to its default. The single fatal case is "explicit
// --config <path> supplied but the file is unreadable or unparseable"
// — the operator clearly meant to point at *that* file.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// AgentConfig is the runtime configuration for the satellites-agent
// binary.
type AgentConfig struct {
	WorkerID     string   `toml:"worker_id"`
	WorkspaceIDs []string `toml:"workspace_ids"`
	// SpawnMCPURL is the MCP endpoint URL written into the dispatched
	// claude subprocess's per-worktree MCP config. The satellites-client
	// CLI itself talks to /api/v1 via internal/cliremote.Client and does
	// not consume this field — it flows only to the spawned subprocess
	// (sty_74e67353 work#1: renamed from MCPURL to make the spawn-side
	// role explicit and to retire the legacy MCP-rooted Go transport).
	SpawnMCPURL       string        `toml:"spawn_mcp_url"`
	AuthToken         string        `toml:"auth_token"`
	IdleBackoff       time.Duration `toml:"idle_backoff"`
	HeartbeatInterval time.Duration `toml:"heartbeat_interval"`
	ExecuteTimeout    time.Duration `toml:"execute_timeout"`
	RepoPath          string        `toml:"repo_path"`
	BranchTemplate    string        `toml:"branch_template"`
	WorktreeRoot      string        `toml:"worktree_root"`
	ClaudeBinaryPath  string        `toml:"claude_binary_path"`
	LogLevel          string        `toml:"log_level"`

	HubURL                string        `toml:"hub_url"`
	SubscribeWorkspaceIDs []string      `toml:"subscribe_workspace_ids"`
	SubscribeSinceID      string        `toml:"subscribe_since_id"`
	WSReconnectMinBackoff time.Duration `toml:"ws_reconnect_min_backoff"`
	WSReconnectMaxBackoff time.Duration `toml:"ws_reconnect_max_backoff"`

	// CLIBinaryPath is the resolved path to bin/satellites-client used
	// by the cli transport. Empty falls back to looking up
	// "satellites-client" on PATH at runtime. Per cli-primary order:06.
	// Post sty_4db0e025 slice E1 the cli transport is the sole agent
	// transport — the legacy MCP-rooted transport + the `transport`
	// config knob were retired together with the placeholderClient
	// shim.
	CLIBinaryPath string `toml:"cli_binary_path"`

	// LogPath is the directory where the agent writes its rolling log
	// file (filename: satellites-agent.log; rotation handled by arbor's
	// phuslu/log backend). Empty disables the file writer — the agent
	// runs console-only. Default: <exe>/logs/ resolved at boot in
	// defaultsAgent. sty_92bfd9e6.
	LogPath string `toml:"log_path"`

	// ClientConfigPath is the absolute path to the operator-resolved
	// satellites-client TOML. The orchestrator-side dispatcher (in
	// cmd/satellites-client) populates this from
	// cliconfig.Config.LoadedTOMLPath() before invoking RunDispatched,
	// so the dispatched subprocess can locate the same config via
	// SATELLITES_CLIENT_CONFIG. Empty when no TOML resolved (loader
	// fell back to defaults) or on the satellites-agent daemon path
	// (daemon uses its own TOML, not the client's). Populated by the
	// caller; never read from TOML. sty_ef4eedaa.
	ClientConfigPath string `toml:"-"`

	// AllowedVerbs is the resolved per-task verb allowlist the
	// dispatcher writes into the worktree `.mcp.json` server entry's
	// `allowedTools` array. Empty/nil → field omitted (un-narrowed
	// caller). Set by cmd/satellites-client/task_run.go after a
	// successful `agent_apikey_create(task_id=…)`; never read from
	// TOML. Defence-in-depth: server enforcement (AC2) is canonical,
	// the client narrowing is the artefact a reviewer greps.
	// sty_056b68f6.
	AllowedVerbs []string `toml:"-"`

	// loadedTOMLPath records the path that was actually read; "" when
	// the loader fell back to defaults.
	loadedTOMLPath string
}

// agentConfigPathEnv is the env var operators set to point at an
// explicit TOML file when not using --config.
const agentConfigPathEnv = "SATELLITES_AGENT_CONFIG"

// agentDefaultConfigFile is the repo-relative file the loader checks
// when no flag or env var resolves. Sits alongside the built binary
// at bin/satellites-agent (mirrors cliconfig's bin-tier convention
// shipped in sty_5da2ed4d).
const agentDefaultConfigFile = "bin/satellites-agent.toml"

// LoadedTOMLPath returns the path that LoadAgent actually read, or
// "" when defaults supplied the whole config.
func (c *AgentConfig) LoadedTOMLPath() string {
	return c.loadedTOMLPath
}

// String renders the AgentConfig with the AuthToken masked. Used by
// arbor logging to ensure secrets do not leak into log output.
func (c AgentConfig) String() string {
	mask := ""
	if c.AuthToken != "" {
		mask = "***"
	}
	return fmt.Sprintf(
		"AgentConfig{worker_id=%q workspace_ids=%v spawn_mcp_url=%q auth_token=%s "+
			"idle_backoff=%s heartbeat_interval=%s execute_timeout=%s "+
			"repo_path=%q branch_template=%q worktree_root=%q "+
			"claude_binary_path=%q log_level=%q log_path=%q hub_url=%q "+
			"subscribe_workspace_ids=%v subscribe_since_id=%q "+
			"ws_reconnect_min_backoff=%s ws_reconnect_max_backoff=%s}",
		c.WorkerID, c.WorkspaceIDs, c.SpawnMCPURL, mask,
		c.IdleBackoff, c.HeartbeatInterval, c.ExecuteTimeout,
		c.RepoPath, c.BranchTemplate, c.WorktreeRoot,
		c.ClaudeBinaryPath, c.LogLevel, c.LogPath, c.HubURL,
		c.SubscribeWorkspaceIDs, c.SubscribeSinceID,
		c.WSReconnectMinBackoff, c.WSReconnectMaxBackoff,
	)
}

// defaultsAgent builds the in-code default AgentConfig. Lowest
// precedence layer; TOML overrides it. LogPath defaults to
// <exe>/logs/ resolved from os.Args[0]; tests typically override via
// the toml fixture so this default doesn't pollute test workspaces.
func defaultsAgent() *AgentConfig {
	logDir := ""
	if len(os.Args) > 0 && os.Args[0] != "" {
		logDir = filepath.Join(filepath.Dir(os.Args[0]), "logs")
	}
	return &AgentConfig{
		WorkerID:                     "",
		WorkspaceIDs:                 nil,
		SpawnMCPURL:                  "http://localhost:8080/mcp",
		AuthToken:                    "",
		IdleBackoff:                  5 * time.Second,
		HeartbeatInterval:            60 * time.Second,
		ExecuteTimeout:               10 * time.Minute,
		RepoPath:                     ".",
		BranchTemplate:               "agent-{task_id}-from-{base_sha}",
		WorktreeRoot:                 ".satellites-agents/",
		ClaudeBinaryPath:             "claude",
		CLIBinaryPath:                "",
		LogLevel:                     "info",
		LogPath:                      logDir,
		HubURL:                       "ws://localhost:8080/ws",
		SubscribeWorkspaceIDs:        nil,
		SubscribeSinceID:             "",
		WSReconnectMinBackoff: 500 * time.Millisecond,
		WSReconnectMaxBackoff: 30 * time.Second,
	}
}

// agentTOMLOverlay is the TOML-decoding shadow of AgentConfig.
// Pointer fields let the loader distinguish "TOML set this" (non-nil)
// from "TOML didn't mention it" (nil) so the overlay overrides
// defaults selectively. Durations decode via the existing duration
// wrapper in this package.
type agentTOMLOverlay struct {
	WorkerID          *string   `toml:"worker_id"`
	WorkspaceIDs      []string  `toml:"workspace_ids"`
	SpawnMCPURL       *string   `toml:"spawn_mcp_url"`
	AuthToken         *string   `toml:"auth_token"`
	IdleBackoff       *duration `toml:"idle_backoff"`
	HeartbeatInterval *duration `toml:"heartbeat_interval"`
	ExecuteTimeout    *duration `toml:"execute_timeout"`
	RepoPath          *string   `toml:"repo_path"`
	BranchTemplate    *string   `toml:"branch_template"`
	WorktreeRoot      *string   `toml:"worktree_root"`
	ClaudeBinaryPath  *string   `toml:"claude_binary_path"`
	CLIBinaryPath     *string   `toml:"cli_binary_path"`
	LogLevel          *string   `toml:"log_level"`

	HubURL                *string   `toml:"hub_url"`
	SubscribeWorkspaceIDs []string  `toml:"subscribe_workspace_ids"`
	SubscribeSinceID      *string   `toml:"subscribe_since_id"`
	WSReconnectMinBackoff *duration `toml:"ws_reconnect_min_backoff"`
	WSReconnectMaxBackoff *duration `toml:"ws_reconnect_max_backoff"`

	LogPath *string `toml:"log_path"`
}

// LoadAgent resolves AgentConfig from the precedence chain and
// returns the resulting struct alongside operator-facing warnings. An
// error is returned only when an explicit path (CLI flag or env var)
// is supplied but the file is unreadable or unparseable.
func LoadAgent(explicitPath string) (*AgentConfig, []string, error) {
	cfg := defaultsAgent()
	var warnings []string

	path, source, err := resolveAgentConfigPath(explicitPath)
	if err != nil {
		return nil, nil, err
	}
	if path == "" {
		warnings = append(warnings, "agent config: no file resolved (flag/env/bin/xdg) — using defaults")
		return cfg, warnings, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if source == agentSourceFlag || source == agentSourceEnv {
			return nil, nil, fmt.Errorf("agent config: %s path %q unreadable: %w", source, path, err)
		}
		warnings = append(warnings, fmt.Sprintf("agent config: %s file %q unreadable: %v — falling back to defaults", source, path, err))
		return cfg, warnings, nil
	}

	var overlay agentTOMLOverlay
	if err := toml.Unmarshal(raw, &overlay); err != nil {
		if source == agentSourceFlag || source == agentSourceEnv {
			return nil, nil, fmt.Errorf("agent config: %s file %q parse failed: %w", source, path, err)
		}
		warnings = append(warnings, fmt.Sprintf("agent config: %s file %q parse failed: %v — falling back to defaults", source, path, err))
		return cfg, warnings, nil
	}

	cfg.loadedTOMLPath = path
	overlay.applyTo(cfg, &warnings)
	return cfg, warnings, nil
}

// agent config path source markers used in warnings + error messages.
const (
	agentSourceFlag = "flag"
	agentSourceEnv  = "env"
	agentSourceBin  = "bin"
	agentSourceXDG  = "xdg"
)

// resolveAgentConfigPath walks the precedence chain and returns the
// first file that resolves. An empty path means no source resolved.
// An error is returned only when an explicit source (flag, env)
// supplied a value but no file exists at that path; the loader
// surfaces that to the caller as fatal because the operator clearly
// meant to point at *that* file.
func resolveAgentConfigPath(explicit string) (string, string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", agentSourceFlag, fmt.Errorf("agent config: --config path %q not found: %w", p, err)
		}
		return p, agentSourceFlag, nil
	}
	if p := strings.TrimSpace(os.Getenv(agentConfigPathEnv)); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", agentSourceEnv, fmt.Errorf("agent config: %s=%q not found: %w", agentConfigPathEnv, p, err)
		}
		return p, agentSourceEnv, nil
	}
	if _, err := os.Stat(agentDefaultConfigFile); err == nil {
		return agentDefaultConfigFile, agentSourceBin, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		xdg := filepath.Join(home, ".config", "satellites-agent", "config.toml")
		if _, err := os.Stat(xdg); err == nil {
			return xdg, agentSourceXDG, nil
		}
	}
	return "", "", nil
}

// applyTo copies overlay values into cfg for every non-nil field.
// Invalid TOML values append to warnings and leave the field at its
// prior value.
func (o agentTOMLOverlay) applyTo(cfg *AgentConfig, warnings *[]string) {
	if o.WorkerID != nil {
		cfg.WorkerID = *o.WorkerID
	}
	if o.WorkspaceIDs != nil {
		cfg.WorkspaceIDs = append([]string(nil), o.WorkspaceIDs...)
	}
	if o.SpawnMCPURL != nil {
		cfg.SpawnMCPURL = *o.SpawnMCPURL
	}
	if o.AuthToken != nil {
		cfg.AuthToken = *o.AuthToken
	}
	if o.IdleBackoff != nil {
		d := time.Duration(*o.IdleBackoff)
		if d <= 0 {
			*warnings = append(*warnings, fmt.Sprintf("agent config: idle_backoff=%s must be > 0 — keeping %s", d, cfg.IdleBackoff))
		} else {
			cfg.IdleBackoff = d
		}
	}
	if o.HeartbeatInterval != nil {
		d := time.Duration(*o.HeartbeatInterval)
		if d <= 0 {
			*warnings = append(*warnings, fmt.Sprintf("agent config: heartbeat_interval=%s must be > 0 — keeping %s", d, cfg.HeartbeatInterval))
		} else {
			cfg.HeartbeatInterval = d
		}
	}
	if o.ExecuteTimeout != nil {
		d := time.Duration(*o.ExecuteTimeout)
		if d <= 0 {
			*warnings = append(*warnings, fmt.Sprintf("agent config: execute_timeout=%s must be > 0 — keeping %s", d, cfg.ExecuteTimeout))
		} else {
			cfg.ExecuteTimeout = d
		}
	}
	if o.RepoPath != nil {
		cfg.RepoPath = *o.RepoPath
	}
	if o.BranchTemplate != nil {
		cfg.BranchTemplate = *o.BranchTemplate
	}
	if o.WorktreeRoot != nil {
		cfg.WorktreeRoot = *o.WorktreeRoot
	}
	if o.ClaudeBinaryPath != nil {
		cfg.ClaudeBinaryPath = *o.ClaudeBinaryPath
	}
	if o.CLIBinaryPath != nil {
		cfg.CLIBinaryPath = *o.CLIBinaryPath
	}
	if o.LogLevel != nil {
		lvl := strings.ToLower(*o.LogLevel)
		if _, ok := validLogLevels[lvl]; !ok {
			*warnings = append(*warnings, fmt.Sprintf("agent config: log_level=%q invalid — keeping %q", *o.LogLevel, cfg.LogLevel))
		} else {
			cfg.LogLevel = lvl
		}
	}
	if o.HubURL != nil {
		cfg.HubURL = *o.HubURL
	}
	if o.SubscribeWorkspaceIDs != nil {
		cfg.SubscribeWorkspaceIDs = append([]string(nil), o.SubscribeWorkspaceIDs...)
	}
	if o.SubscribeSinceID != nil {
		cfg.SubscribeSinceID = *o.SubscribeSinceID
	}
	if o.WSReconnectMinBackoff != nil {
		d := time.Duration(*o.WSReconnectMinBackoff)
		if d <= 0 {
			*warnings = append(*warnings, fmt.Sprintf("agent config: ws_reconnect_min_backoff=%s must be > 0 — keeping %s", d, cfg.WSReconnectMinBackoff))
		} else {
			cfg.WSReconnectMinBackoff = d
		}
	}
	if o.WSReconnectMaxBackoff != nil {
		d := time.Duration(*o.WSReconnectMaxBackoff)
		if d <= 0 {
			*warnings = append(*warnings, fmt.Sprintf("agent config: ws_reconnect_max_backoff=%s must be > 0 — keeping %s", d, cfg.WSReconnectMaxBackoff))
		} else {
			cfg.WSReconnectMaxBackoff = d
		}
	}
	if o.LogPath != nil {
		cfg.LogPath = *o.LogPath
	}
}

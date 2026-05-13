// Package client — `satellites_init` typed method (sty_796b8fe1).
//
// Read-shape verb. Returns a structured install/refresh payload the
// orchestrator (or an operator script) consumes to install or refresh
// the satellites-client binary on a consumer project. The verb itself
// performs no disk writes; install + refresh are the caller's job.
//
// Payload state machine (per the plan):
//
//	install_required — no installed binary detected or version stamp unreadable.
//	update_available — installed stamp lags the manifest's version.
//	up_to_date       — installed stamp matches the manifest's version exactly.
//
// Manifest fetch reuses the SystemVersion typed surface — the same
// HTTP indirection point (`systemVersionHTTPDo`) and the same 60s
// in-memory cache — so MCP / HTTP / CLI calls to either verb share
// the round-trip cost. Per pr_mcp_cli_shared_path the wire adapters
// (mcpserver, httpserver, cmd/satellites-client) delegate here and
// hold no payload-assembly logic of their own.

package client

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// SatellitesInitInput is the caller-supplied input for SatellitesInit.
// All fields are optional: zero-value runs the server-resolved defaults
// (OS/arch from runtime.{GOOS,GOARCH}; empty CurrentVersion drives the
// `install_required` state).
type SatellitesInitInput struct {
	// CurrentVersion is the operator-supplied stamp of the binary
	// already installed on the consumer host, when one exists. The
	// caller resolves this (the CLI subcommand reads `config.Version`)
	// and threads it into the payload. Empty resolves to
	// `install_required` so a never-installed host gets the install
	// instructions without an extra round-trip.
	CurrentVersion string `json:"current_version,omitempty"`

	// OS / Arch override the server-side runtime defaults for cross-
	// host orchestration. Zero-value falls back to runtime.GOOS /
	// runtime.GOARCH.
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
}

// SatellitesInitDefaultConfig mirrors the canonical satellites-client.toml
// shape the orchestrator materialises during install.
type SatellitesInitDefaultConfig struct {
	RepoPath       string `json:"repo_path"`
	WorktreeRoot   string `json:"worktree_root"`
	LogPath        string `json:"log_path"`
	BranchTemplate string `json:"branch_template"`
}

// SatellitesInitInstall pins the artifact details the caller needs to
// fetch + verify the binary. Mirrors the per-artifact entry on the
// manifest but stamped down to the resolved OS/arch only.
type SatellitesInitInstall struct {
	Version     string `json:"version"`
	Build       string `json:"build"`
	Commit      string `json:"commit"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Filename    string `json:"filename"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

// SatellitesInitAuthBootstrap describes the auth step the operator runs
// after the binary lands. Kept as a typed struct so future bootstrap
// kinds (`oauth_pkce`, `device_code`, …) can be added without breaking
// the wire shape.
type SatellitesInitAuthBootstrap struct {
	Kind    string `json:"kind"`
	Command string `json:"command"`
	EnvHint string `json:"env_hint"`
}

// SatellitesInitOutput is the wire payload for satellites_init.
type SatellitesInitOutput struct {
	State             string                      `json:"state"`
	TargetInstallPath string                      `json:"target_install_path"`
	TargetConfigPath  string                      `json:"target_config_path"`
	DefaultConfig     SatellitesInitDefaultConfig `json:"default_config"`
	Install           SatellitesInitInstall       `json:"install"`
	AuthBootstrap     SatellitesInitAuthBootstrap `json:"auth_bootstrap"`
	CurrentVersion    string                      `json:"current_version,omitempty"`
	FetchedAt         time.Time                   `json:"fetched_at"`
}

// State string constants returned in SatellitesInitOutput.State.
const (
	SatellitesInitStateInstallRequired = "install_required"
	SatellitesInitStateUpdateAvailable = "update_available"
	SatellitesInitStateUpToDate        = "up_to_date"
)

// SatellitesInit assembles the install/refresh payload. Delegates the
// manifest fetch to SystemVersion (so the 60s cache and TTL apply
// uniformly across both verbs), picks the artifact for the requested
// OS/arch, and computes the state from the caller-supplied
// CurrentVersion.
func (c *Client) SatellitesInit(ctx context.Context, caller Caller, in SatellitesInitInput) (SatellitesInitOutput, error) {
	manifest, err := c.SystemVersion(ctx, caller, SystemVersionInput{})
	if err != nil {
		return SatellitesInitOutput{}, err
	}

	osID := strings.TrimSpace(in.OS)
	if osID == "" {
		osID = runtime.GOOS
	}
	archID := strings.TrimSpace(in.Arch)
	if archID == "" {
		archID = runtime.GOARCH
	}

	artifact, ok := pickSatellitesInitArtifact(manifest.Artifacts, osID, archID)
	if !ok {
		return SatellitesInitOutput{}, fmt.Errorf("satellites_init: no artifact for os=%s arch=%s in manifest", osID, archID)
	}

	state := SatellitesInitStateInstallRequired
	current := strings.TrimSpace(in.CurrentVersion)
	switch {
	case current == "":
		state = SatellitesInitStateInstallRequired
	case current == manifest.Version:
		state = SatellitesInitStateUpToDate
	default:
		state = SatellitesInitStateUpdateAvailable
	}

	return SatellitesInitOutput{
		State:             state,
		TargetInstallPath: "./satellites/satellites-client",
		TargetConfigPath:  "./satellites/satellites-client.toml",
		DefaultConfig: SatellitesInitDefaultConfig{
			RepoPath:       ".",
			WorktreeRoot:   "./satellites/worktree",
			LogPath:        "./satellites/logs",
			BranchTemplate: "client-{task_id}-from-{base_sha}",
		},
		Install: SatellitesInitInstall{
			Version:     manifest.Version,
			Build:       manifest.Build,
			Commit:      manifest.Commit,
			OS:          artifact.OS,
			Arch:        artifact.Arch,
			Filename:    artifact.Filename,
			DownloadURL: artifact.DownloadURL,
			SHA256:      artifact.SHA256,
		},
		AuthBootstrap: SatellitesInitAuthBootstrap{
			Kind:    "auth_login",
			Command: "satellites-client auth login",
			EnvHint: "SATELLITES_TOKEN",
		},
		CurrentVersion: current,
		FetchedAt:      manifest.FetchedAt,
	}, nil
}

// pickSatellitesInitArtifact picks the manifest entry matching the
// requested os/arch tuple. Exact match only — falling back to a
// different arch would hand the operator a binary that segfaults at
// boot.
func pickSatellitesInitArtifact(artifacts []SystemVersionArtifact, osID, archID string) (SystemVersionArtifact, bool) {
	for _, a := range artifacts {
		if a.OS == osID && a.Arch == archID {
			return a, true
		}
	}
	return SystemVersionArtifact{}, false
}

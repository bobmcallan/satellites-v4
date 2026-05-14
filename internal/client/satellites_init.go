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

	"github.com/bobmcallan/satellites/internal/audit/chaincoverage"
	"github.com/bobmcallan/satellites/internal/document"
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

	// WorkspaceID + ResolvedProjectID are pre-resolved by the wire
	// layer when the caller binds a workspace/project context. Both
	// drive the orientation surface (workspace_overrides +
	// chain_coverage); empty disables the resolution and stamps
	// scope=system on the overrides (best-effort fallback).
	WorkspaceID       string
	ResolvedProjectID string
	Memberships       []string
}

// SatellitesInitOverride is one orientation pointer surfaced on the
// init payload: which scope the workspace currently consumes the doc
// from + the resolved doc id + a recommendation describing the
// customization point. The verb does NOT mutate the substrate —
// recommendations are informational only.
type SatellitesInitOverride struct {
	Scope          string `json:"scope"`
	DocID          string `json:"doc_id,omitempty"`
	Recommendation string `json:"recommendation"`
}

// SatellitesInitWorkspaceOverrides bundles the per-doc-kind override
// pointers. Today we surface orchestrator + workflow.
type SatellitesInitWorkspaceOverrides struct {
	Orchestrator SatellitesInitOverride `json:"orchestrator"`
	Workflow     SatellitesInitOverride `json:"workflow"`
}

// SatellitesInitContractEntry mirrors chaincoverage.ContractEntry on
// the wire — the orientation payload carries it directly so callers
// do not need to import internal/audit.
type SatellitesInitContractEntry struct {
	Name    string `json:"name"`
	FoundAt string `json:"found_at,omitempty"`
	DocID   string `json:"doc_id,omitempty"`
}

// SatellitesInitChainCoverage is the chain-coverage report stamped on
// the init payload. WorkflowSource names the tier the workspace
// resolves the `default` workflow doc from; Contracts enumerates the
// canonical chain with each contract's tier + doc id; MissingContracts
// collects names that did not resolve.
type SatellitesInitChainCoverage struct {
	WorkflowSource   string                        `json:"workflow_source"`
	WorkflowDocID    string                        `json:"workflow_doc_id,omitempty"`
	Contracts        []SatellitesInitContractEntry `json:"contracts"`
	MissingContracts []string                      `json:"missing_contracts"`
	Recommendation   string                        `json:"recommendation"`
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
	State              string                           `json:"state"`
	TargetInstallPath  string                           `json:"target_install_path"`
	TargetConfigPath   string                           `json:"target_config_path"`
	DefaultConfig      SatellitesInitDefaultConfig      `json:"default_config"`
	Install            SatellitesInitInstall            `json:"install"`
	AuthBootstrap      SatellitesInitAuthBootstrap      `json:"auth_bootstrap"`
	CurrentVersion     string                           `json:"current_version,omitempty"`
	FetchedAt          time.Time                        `json:"fetched_at"`
	WorkspaceOverrides SatellitesInitWorkspaceOverrides `json:"workspace_overrides"`
	ChainCoverage      SatellitesInitChainCoverage      `json:"chain_coverage"`
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

	overrides := c.resolveWorkspaceOverrides(ctx, in)
	coverage := c.resolveChainCoverage(ctx, in)

	return SatellitesInitOutput{
		State:             state,
		TargetInstallPath: "./.satellites/satellites-client",
		TargetConfigPath:  "./.satellites/satellites-client.toml",
		DefaultConfig: SatellitesInitDefaultConfig{
			RepoPath:       ".",
			WorktreeRoot:   "./.satellites/worktree",
			LogPath:        "./.satellites/logs",
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
		CurrentVersion:     current,
		FetchedAt:          manifest.FetchedAt,
		WorkspaceOverrides: overrides,
		ChainCoverage:      coverage,
	}, nil
}

// recommendationOrchestratorWorkspace is the customization-point
// prose surfaced when the workspace consumes the system-tier
// orchestrator (no workspace override).
const recommendationOrchestratorWorkspace = "This workspace has no claude_orchestrator override; consuming the universal default. If your project's contracts or process flow differ (e.g., a different release primitive than merge_to_main; an additional review step), author a workspace-scope override at config/seed/<workspace>/agents/claude_orchestrator.md and seed it. Otherwise, the default applies cleanly."

// recommendationWorkflowWorkspace mirrors the orchestrator hint for
// the `default` workflow.
const recommendationWorkflowWorkspace = "This workspace has no `default` workflow override; consuming the universal default. Override at config/seed/<workspace>/workflows/default.md if your project's chain shape differs."

// recommendationUsingOverride is the prose stamped when the
// workspace has authored its own override (no recommendation
// needed).
const recommendationUsingOverride = "using your workspace override"

// resolveWorkspaceOverrides resolves the orchestrator + workflow
// docs through the document store's tier ladder and stamps the
// appropriate recommendation prose. Returns scope=system entries
// with the workspace-side recommendation when the docs store is
// not configured or no workspace was supplied — the operator still
// sees the customization point.
func (c *Client) resolveWorkspaceOverrides(ctx context.Context, in SatellitesInitInput) SatellitesInitWorkspaceOverrides {
	overrides := SatellitesInitWorkspaceOverrides{
		Orchestrator: SatellitesInitOverride{Scope: document.ScopeSystem, Recommendation: recommendationOrchestratorWorkspace},
		Workflow:     SatellitesInitOverride{Scope: document.ScopeSystem, Recommendation: recommendationWorkflowWorkspace},
	}
	if c.deps.Documents == nil {
		return overrides
	}
	if doc, err := c.deps.Documents.ResolveByName(ctx, document.TypeAgent, "claude_orchestrator", in.WorkspaceID, in.ResolvedProjectID, in.Memberships); err == nil {
		overrides.Orchestrator.Scope = doc.Scope
		overrides.Orchestrator.DocID = doc.ID
		if doc.Scope == document.ScopeWorkspace {
			overrides.Orchestrator.Recommendation = recommendationUsingOverride
		}
	}
	if doc, err := c.deps.Documents.ResolveByName(ctx, document.TypeWorkflow, "default", in.WorkspaceID, in.ResolvedProjectID, in.Memberships); err == nil {
		overrides.Workflow.Scope = doc.Scope
		overrides.Workflow.DocID = doc.ID
		if doc.Scope == document.ScopeWorkspace {
			overrides.Workflow.Recommendation = recommendationUsingOverride
		}
	}
	return overrides
}

// resolveChainCoverage delegates to the shared chaincoverage package
// and re-projects the Result into the wire-payload type. The
// chaincoverage package is the same primitive sty_2f0db922's audit
// verb will consume — locating the loop there avoids a second
// extraction.
func (c *Client) resolveChainCoverage(ctx context.Context, in SatellitesInitInput) SatellitesInitChainCoverage {
	coverage := SatellitesInitChainCoverage{
		Contracts:        []SatellitesInitContractEntry{},
		MissingContracts: []string{},
	}
	if c.deps.Documents == nil {
		coverage.Recommendation = "document store not configured; chain_coverage unavailable"
		return coverage
	}
	res, err := chaincoverage.Resolve(ctx, c.deps.Documents, in.WorkspaceID, in.ResolvedProjectID, in.Memberships, nil)
	if err != nil {
		coverage.Recommendation = "chain_coverage error: " + err.Error()
		return coverage
	}
	coverage.WorkflowSource = res.WorkflowSource
	coverage.WorkflowDocID = res.WorkflowDocID
	for _, e := range res.Contracts {
		coverage.Contracts = append(coverage.Contracts, SatellitesInitContractEntry{
			Name:    e.Name,
			FoundAt: e.FoundAt,
			DocID:   e.DocID,
		})
	}
	coverage.MissingContracts = res.MissingContracts
	coverage.Recommendation = chaincoverage.RecommendationText(in.WorkspaceID, res)
	return coverage
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

// Package satellitesinit resolves the install schema artifact that
// drives the `satellites_init` MCP/HTTP/CLI verb. The schema —
// target_install_path, target_config_path, default_config (repo_path,
// worktree_root, log_path, branch_template), auth_bootstrap (kind,
// command, env_hint) — was carved out of Go literals into a system-scope
// `type=artifact` row seeded from
// `config/seed/system/artifacts/satellites_client_install.md`
// (sty_193a5185).
//
// Sourcing the schema from a seeded artifact rather than Go constants
// enforces the substrate's configuration-over-code mandate
// (pr_substrate_model): future operators flip an install default by
// editing the markdown and re-seeding — no recompile.
package satellitesinit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bobmcallan/satellites/internal/document"
)

// SystemDefaultName is the name under which the canonical install
// schema artifact is seeded into the system tier.
const SystemDefaultName = "satellites_client_install"

// KindTag is the canonical tag every install-schema artifact carries.
// Mirrors the `kind:<purpose>` convention default_agent_process uses.
const KindTag = "kind:install-schema"

// InstallSchema is the typed shape the artifact's Structured payload
// decodes into. Field tags match the wire shape the satellites_init
// verb emits — keeping the JSON shape stable lets the resolver decode
// directly into the same struct the seed loader serialises.
type InstallSchema struct {
	TargetInstallPath string                     `json:"target_install_path"`
	TargetConfigPath  string                     `json:"target_config_path"`
	DefaultConfig     InstallSchemaDefaultConfig `json:"default_config"`
	AuthBootstrap     InstallSchemaAuthBootstrap `json:"auth_bootstrap"`
}

// InstallSchemaDefaultConfig mirrors the canonical satellites-client.toml
// defaults the orchestrator materialises during install.
type InstallSchemaDefaultConfig struct {
	RepoPath       string `json:"repo_path"`
	WorktreeRoot   string `json:"worktree_root"`
	LogPath        string `json:"log_path"`
	BranchTemplate string `json:"branch_template"`
}

// InstallSchemaAuthBootstrap describes the auth bootstrap step the
// operator runs after the binary lands. Kept as a typed struct so
// future bootstrap kinds can be added without breaking the wire shape.
type InstallSchemaAuthBootstrap struct {
	Kind    string `json:"kind"`
	Command string `json:"command"`
	EnvHint string `json:"env_hint"`
}

// ResolverDocs is the read-only subset of document.Store the resolver
// needs. Defined here (rather than imported wholesale) so tests can
// inject a stub without standing up a full document store.
type ResolverDocs interface {
	ResolveByName(ctx context.Context, docType, name, workspaceID, projectID string, memberships []string) (document.Document, error)
}

// Resolve fetches the install schema from the seed-loaded artifact via
// the tier ladder project → workspace → system. workspaceID / projectID
// scope the lookup the same way every other document read does;
// memberships gate workspace + project tiers per the doc store's
// membership predicate. The system tier is always considered.
//
// Returns a typed error when the artifact is missing, inactive, has no
// structured payload, or carries an unparseable payload — by design,
// the resolver refuses to serve baked defaults so seed-edit drift
// surfaces loudly. Per pr_substrate_model the substrate's prose is the
// source of truth; an empty prose state is an authoring error, not a
// fallback condition.
func Resolve(ctx context.Context, docs ResolverDocs, workspaceID, projectID string, memberships []string) (InstallSchema, error) {
	if docs == nil {
		return InstallSchema{}, fmt.Errorf("satellites_init: install schema not seeded (document store not configured)")
	}
	d, err := docs.ResolveByName(ctx, document.TypeArtifact, SystemDefaultName, workspaceID, projectID, memberships)
	if err != nil {
		return InstallSchema{}, fmt.Errorf("satellites_init: install schema not seeded: %w", err)
	}
	if d.Status != document.StatusActive {
		return InstallSchema{}, fmt.Errorf("satellites_init: install schema artifact inactive")
	}
	if len(d.Structured) == 0 {
		return InstallSchema{}, fmt.Errorf("satellites_init: install schema artifact has no structured payload")
	}
	var schema InstallSchema
	if err := json.Unmarshal(d.Structured, &schema); err != nil {
		return InstallSchema{}, fmt.Errorf("satellites_init: install schema structured payload: %w", err)
	}
	if schema.TargetInstallPath == "" {
		return InstallSchema{}, fmt.Errorf("satellites_init: install schema missing target_install_path")
	}
	return schema, nil
}

// MarshalSchema encodes the typed schema into the canonical Structured
// payload bytes the seed loader (artifactToInput in
// internal/configseed/parsers.go) emits. Exposed for tests that need to
// seed a fixture schema directly via document.Store.Create without
// going through configseed.
func MarshalSchema(s InstallSchema) ([]byte, error) {
	return json.Marshal(s)
}

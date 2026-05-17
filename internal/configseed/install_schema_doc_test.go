package configseed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestSeedLoad_InstallSchemaArtifact — sty_193a5185 AC1.
//
// Asserts the real `config/seed/system/artifacts/satellites_client_install.md`
// artifact loads through configseed.Run as a system-scope, active,
// type=artifact row carrying the install-schema tag AND a Structured
// payload that decodes into the canonical InstallSchema shape with the
// byte-identical default values today's verb returns. AC3
// (byte-identical pre/post wire shape) gets a downstream verification
// against the live pprod deploy; this test is the substrate-tier load
// invariant — a seed file rename, frontmatter typo, or parser
// regression surfaces here before it would reach pprod.
//
// The test deliberately decodes Structured with a local mirror of the
// resolver's typed struct so that no test-private byte alignment with
// the resolver is required: the JSON field tags ARE the contract and
// the test asserts that contract directly.
func TestSeedLoad_InstallSchemaArtifact(t *testing.T) {
	t.Parallel()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const ws = "wksp_install_schema"

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	if _, err := Run(ctx, docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("configseed.Run: %v", err)
	}

	doc, err := docs.GetByName(ctx, "", "satellites_client_install", nil)
	if err != nil {
		t.Fatalf("docs.GetByName(satellites_client_install): %v", err)
	}
	if doc.Type != document.TypeArtifact {
		t.Errorf("doc.Type = %q, want %q", doc.Type, document.TypeArtifact)
	}
	if doc.Scope != document.ScopeSystem {
		t.Errorf("doc.Scope = %q, want %q", doc.Scope, document.ScopeSystem)
	}
	if doc.Status != document.StatusActive {
		t.Errorf("doc.Status = %q, want %q", doc.Status, document.StatusActive)
	}
	if !hasTag(doc.Tags, "kind:install-schema") {
		t.Errorf("doc.Tags missing kind:install-schema (got %v)", doc.Tags)
	}
	if len(doc.Structured) == 0 {
		t.Fatalf("doc.Structured empty — parser did not encode frontmatter schema keys")
	}

	type schemaDefaults struct {
		RepoPath       string `json:"repo_path"`
		WorktreeRoot   string `json:"worktree_root"`
		LogPath        string `json:"log_path"`
		BranchTemplate string `json:"branch_template"`
	}
	type schemaAuth struct {
		Kind    string `json:"kind"`
		Command string `json:"command"`
		EnvHint string `json:"env_hint"`
	}
	type schema struct {
		TargetInstallPath string         `json:"target_install_path"`
		TargetConfigPath  string         `json:"target_config_path"`
		DefaultConfig     schemaDefaults `json:"default_config"`
		AuthBootstrap     schemaAuth     `json:"auth_bootstrap"`
	}
	var got schema
	if err := json.Unmarshal(doc.Structured, &got); err != nil {
		t.Fatalf("unmarshal Structured: %v\nstructured=%s", err, string(doc.Structured))
	}

	want := schema{
		TargetInstallPath: "./.satellites/satellites-client",
		TargetConfigPath:  "./.satellites/satellites-client.toml",
		DefaultConfig: schemaDefaults{
			RepoPath:       ".",
			WorktreeRoot:   "./.satellites/worktree",
			LogPath:        "./.satellites/logs",
			BranchTemplate: "client-{task_id}-from-{base_sha}",
		},
		AuthBootstrap: schemaAuth{
			Kind:    "auth_login",
			Command: "satellites-client auth login",
			EnvHint: "SATELLITES_TOKEN",
		},
	}
	if got != want {
		t.Errorf("seeded install schema drift:\n got = %+v\nwant = %+v", got, want)
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

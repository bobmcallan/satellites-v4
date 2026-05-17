// Package satellitesinit_test — install-schema carve-out regression
// tests (sty_193a5185). The external _test package avoids the import
// cycle that would arise if these tests lived in `package
// satellitesinit` (the regression test needs `internal/client` to drive
// the full verb, and `internal/client` imports the resolver).
package satellitesinit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
)

// installSchemaManifest is the manifest fixture the regression test
// fans against. Stable across re-seeds because the carve-out test only
// asserts on schema-driven fields; the manifest-derived install fields
// (version/build/...) are scope-out per AC3.
const installSchemaManifest = `{
    "version":"0.0.300",
    "build":"2026-05-13-15-00-00",
    "commit":"abc12345",
    "artifacts":[
      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"aaaa","download_url":"https://example.invalid/linux-amd64"}
    ]
}`

// writeInstallSchemaSeed writes a minimal install-schema artifact file
// into <tmpDir>/system/artifacts/satellites_client_install.md and
// returns the tmpDir root expected by configseed.Run.
func writeInstallSchemaSeed(t *testing.T, tmpDir, targetInstallPath string) {
	t.Helper()
	dir := filepath.Join(tmpDir, "system", "artifacts")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	body := `---
name: satellites_client_install
tags: [kind:install-schema, v1]
target_install_path: ` + targetInstallPath + `
target_config_path: ./.satellites/satellites-client.toml
default_config:
  repo_path: .
  worktree_root: ./.satellites/worktree
  log_path: ./.satellites/logs
  branch_template: client-{task_id}-from-{base_sha}
auth_bootstrap:
  kind: auth_login
  command: satellites-client auth login
  env_hint: SATELLITES_TOKEN
---
# install schema (regression test fixture)
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "satellites_client_install.md"), []byte(body), 0o644))
}

// TestSatellitesInit_ArtifactDrivesTargetInstallPath is the AC5
// substrate-tier regression test for sty_193a5185. The test shape, per
// the review criteria:
//
//  1. Two configseed.Run invocations against the same MemoryStore.
//  2. Between the two invocations the on-disk artifact body is
//     rewritten to carry a different target_install_path value.
//  3. Two Client.SatellitesInit(...) calls — one after each seed.
//  4. Assertion 1: out.TargetInstallPath matches the first body's value.
//  5. Assertion 2: out.TargetInstallPath matches the second body's value.
//  6. The Go binary is the same compiled artifact across both
//     assertions — no subprocess, no recompile. The only delta between
//     the two assertions is the markdown file on disk.
//
// If this test passes, the carve-out is structurally correct: future
// operators can flip the install target by editing prose + re-seeding,
// with no Go recompile. If it fails, the literal strings have leaked
// back into Go (or the resolver is reading stale state). Either
// failure mode is exactly the regression AC5 exists to catch.
func TestSatellitesInit_ArtifactDrivesTargetInstallPath(t *testing.T) {
	client.ResetSystemVersionCacheForTest()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	manifestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(installSchemaManifest))
	}))
	t.Cleanup(manifestSrv.Close)

	docs := document.NewMemoryStore()
	c := client.New(client.Deps{ManifestURL: manifestSrv.URL, Documents: docs})

	tmpDir := t.TempDir()
	const wsID = "wksp_carveout_test"

	// Seed A — current canonical path.
	writeInstallSchemaSeed(t, tmpDir, "./.satellites/satellites-client")
	if _, err := configseed.Run(ctx, docs, tmpDir, wsID, "test", now); err != nil {
		t.Fatalf("configseed.Run (seed A): %v", err)
	}
	outA, err := c.SatellitesInit(ctx, client.Caller{}, client.SatellitesInitInput{
		OS: "linux", Arch: "amd64",
	})
	require.NoError(t, err)
	assert.Equal(t, "./.satellites/satellites-client", outA.TargetInstallPath,
		"seed A: TargetInstallPath should reflect the seeded value")

	// Seed B — rewrite the same file with a different
	// target_install_path. The Go binary is unchanged across this
	// boundary; the only delta is the markdown file on disk.
	writeInstallSchemaSeed(t, tmpDir, "./carveout-regression/satellites-client")
	if _, err := configseed.Run(ctx, docs, tmpDir, wsID, "test", now.Add(time.Hour)); err != nil {
		t.Fatalf("configseed.Run (seed B): %v", err)
	}
	outB, err := c.SatellitesInit(ctx, client.Caller{}, client.SatellitesInitInput{
		OS: "linux", Arch: "amd64",
	})
	require.NoError(t, err)
	assert.Equal(t, "./carveout-regression/satellites-client", outB.TargetInstallPath,
		"seed B: TargetInstallPath should reflect the re-seeded value (configuration-over-code invariant)")
}

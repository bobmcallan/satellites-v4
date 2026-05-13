package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// satellitesInitManifest returns a fixture manifest with linux/amd64,
// linux/arm64 and darwin/arm64 entries covering the runtime tuples the
// integration test bench fans across.
func satellitesInitManifest() string {
	return `{
	    "version":"0.0.300",
	    "build":"2026-05-13-15-00-00",
	    "commit":"abc12345",
	    "artifacts":[
	      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"aaaa","download_url":"https://example.invalid/linux-amd64"},
	      {"os":"linux","arch":"arm64","filename":"satellites-client-linux-arm64","sha256":"bbbb","download_url":"https://example.invalid/linux-arm64"},
	      {"os":"darwin","arch":"arm64","filename":"satellites-client-darwin-arm64","sha256":"cccc","download_url":"https://example.invalid/darwin-arm64"}
	    ]
	}`
}

// newSatellitesInitFixture stands up an httptest manifest server and
// returns a Client wired to it. Resets the shared system_version cache
// so each call exercises a fresh fetch path.
func newSatellitesInitFixture(t *testing.T) *Client {
	t.Helper()
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(satellitesInitManifest()))
	}))
	t.Cleanup(srv.Close)
	return New(Deps{ManifestURL: srv.URL})
}

// TestSatellitesInit_InstallRequired: empty CurrentVersion → state
// is install_required, payload carries the canonical install paths and
// the runtime-matching artifact.
func TestSatellitesInit_InstallRequired(t *testing.T) {
	c := newSatellitesInitFixture(t)
	out, err := c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{
		OS:   "linux",
		Arch: "amd64",
	})
	require.NoError(t, err)
	assert.Equal(t, SatellitesInitStateInstallRequired, out.State)
	assert.Equal(t, "./satellites/satellites-client", out.TargetInstallPath)
	assert.Equal(t, "./satellites/satellites-client.toml", out.TargetConfigPath)
	assert.Equal(t, "./satellites/worktree", out.DefaultConfig.WorktreeRoot)
	assert.Equal(t, "./satellites/logs", out.DefaultConfig.LogPath)
	assert.Equal(t, "client-{task_id}-from-{base_sha}", out.DefaultConfig.BranchTemplate)
	assert.Equal(t, "0.0.300", out.Install.Version)
	assert.Equal(t, "linux", out.Install.OS)
	assert.Equal(t, "amd64", out.Install.Arch)
	assert.Equal(t, "satellites-client-linux-amd64", out.Install.Filename)
	assert.Equal(t, "aaaa", out.Install.SHA256)
	assert.Equal(t, "https://example.invalid/linux-amd64", out.Install.DownloadURL)
	assert.Equal(t, "auth_login", out.AuthBootstrap.Kind)
	assert.Equal(t, "satellites-client auth login", out.AuthBootstrap.Command)
	assert.Equal(t, "SATELLITES_TOKEN", out.AuthBootstrap.EnvHint)
	assert.Empty(t, out.CurrentVersion)
	assert.False(t, out.FetchedAt.IsZero())
}

// TestSatellitesInit_UpdateAvailable: caller's stamp lags manifest.
func TestSatellitesInit_UpdateAvailable(t *testing.T) {
	c := newSatellitesInitFixture(t)
	out, err := c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{
		CurrentVersion: "0.0.299",
		OS:             "linux",
		Arch:           "arm64",
	})
	require.NoError(t, err)
	assert.Equal(t, SatellitesInitStateUpdateAvailable, out.State)
	assert.Equal(t, "0.0.299", out.CurrentVersion)
	assert.Equal(t, "arm64", out.Install.Arch)
	assert.Equal(t, "satellites-client-linux-arm64", out.Install.Filename)
	// AC1: download_url is the GitHub-style URL from the manifest, not
	// a `go build` fallback string.
	assert.NotContains(t, out.Install.DownloadURL, "go build")
}

// TestSatellitesInit_UpToDate: caller's stamp matches manifest exactly.
func TestSatellitesInit_UpToDate(t *testing.T) {
	c := newSatellitesInitFixture(t)
	out, err := c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{
		CurrentVersion: "0.0.300",
		OS:             "linux",
		Arch:           "amd64",
	})
	require.NoError(t, err)
	assert.Equal(t, SatellitesInitStateUpToDate, out.State)
	assert.Equal(t, "0.0.300", out.CurrentVersion)
}

// TestSatellitesInit_UnknownOSArch surfaces a typed error rather than
// returning a half-populated payload with an empty Install.
func TestSatellitesInit_UnknownOSArch(t *testing.T) {
	c := newSatellitesInitFixture(t)
	_, err := c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{
		OS:   "plan9",
		Arch: "mips",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no artifact for os=plan9 arch=mips")
}

// TestSatellitesInit_DefaultsToRuntime: zero-value OS/Arch fall back to
// runtime.GOOS / runtime.GOARCH. The manifest fixture carries
// linux/amd64, linux/arm64, darwin/arm64; assert only when the running
// host is one of those tuples (otherwise the verb correctly errors and
// the assertion would be brittle).
func TestSatellitesInit_DefaultsToRuntime(t *testing.T) {
	c := newSatellitesInitFixture(t)
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "darwin/arm64":
	default:
		t.Skipf("manifest fixture has no artifact for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	out, err := c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{})
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS, out.Install.OS)
	assert.Equal(t, runtime.GOARCH, out.Install.Arch)
}

// TestSatellitesInit_EmptyManifestURL propagates the SystemVersion
// error path — a server without ManifestURL configured cannot serve
// the verb.
func TestSatellitesInit_EmptyManifestURL(t *testing.T) {
	resetSystemVersionCache()
	c := New(Deps{})
	_, err := c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest_url")
}

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
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
	assert.Equal(t, "./.satellites/satellites-client", out.TargetInstallPath)
	assert.Equal(t, "./.satellites/satellites-client.toml", out.TargetConfigPath)
	assert.Equal(t, "./.satellites/worktree", out.DefaultConfig.WorktreeRoot)
	assert.Equal(t, "./.satellites/logs", out.DefaultConfig.LogPath)
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

// recommendationFixture stands up a satellites_init Client wired to
// a fake manifest server AND a memory-backed document store. The
// caller seeds the store with whatever orchestrator / workflow /
// contract docs the test case wants the verb to observe.
type recommendationFixture struct {
	t       *testing.T
	c       *Client
	docs    document.Store
	srv     *httptest.Server
	wsID    string
	projID  string
	memberships []string
}

func newRecommendationFixture(t *testing.T) *recommendationFixture {
	t.Helper()
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(satellitesInitManifest()))
	}))
	t.Cleanup(srv.Close)
	docs := document.NewMemoryStore()
	c := New(Deps{ManifestURL: srv.URL, Documents: docs})
	const wsID = "wksp_recommendation"
	return &recommendationFixture{
		t:           t,
		c:           c,
		docs:        docs,
		srv:         srv,
		wsID:        wsID,
		projID:      "proj_recommendation",
		memberships: []string{wsID},
	}
}

// seedSystemTier creates the system-tier orchestrator + workflow +
// the canonical contracts. SkipContracts opts a name out so tests
// can drive the missing_contracts surface.
func (f *recommendationFixture) seedSystemTier(skipContracts ...string) {
	f.t.Helper()
	skip := map[string]bool{}
	for _, n := range skipContracts {
		skip[n] = true
	}
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	create := func(d document.Document) string {
		out, err := f.docs.Create(context.Background(), d, now)
		require.NoError(f.t, err)
		return out.ID
	}
	create(document.Document{Type: document.TypeAgent, Scope: document.ScopeSystem, Name: "claude_orchestrator", Body: "system orchestrator", Status: document.StatusActive})
	create(document.Document{Type: document.TypeWorkflow, Scope: document.ScopeSystem, Name: "default", Body: "system default workflow", Status: document.StatusActive})
	for _, name := range []string{"plan", "develop", "commit", "merge_to_main", "story_review"} {
		if skip[name] {
			continue
		}
		create(document.Document{Type: document.TypeContract, Scope: document.ScopeSystem, Name: name, Body: "system " + name, Status: document.StatusActive})
	}
}

// seedWorkspaceOverride creates workspace-tier overrides for the
// supplied doc names + type tuples.
func (f *recommendationFixture) seedWorkspaceOverride(docType, name string) string {
	f.t.Helper()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	out, err := f.docs.Create(context.Background(), document.Document{
		Type:        docType,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: f.wsID,
		Name:        name,
		Body:        "workspace override " + name,
		Status:      document.StatusActive,
	}, now)
	require.NoError(f.t, err)
	return out.ID
}

func (f *recommendationFixture) call() SatellitesInitOutput {
	f.t.Helper()
	out, err := f.c.SatellitesInit(context.Background(), Caller{}, SatellitesInitInput{
		OS:                "linux",
		Arch:              "amd64",
		CurrentVersion:    "0.0.300",
		WorkspaceID:       f.wsID,
		ResolvedProjectID: f.projID,
		Memberships:       f.memberships,
	})
	require.NoError(f.t, err)
	return out
}

// TestSatellitesInit_RecommendsNoOverride — AC11. Workspace lacks
// both orchestrator + `default` workflow overrides; the payload
// surfaces scope=system with the customization-hint recommendation.
// The verb writes NO substrate rows (read-only assertion via
// document_list count pre/post).
func TestSatellitesInit_RecommendsNoOverride(t *testing.T) {
	f := newRecommendationFixture(t)
	f.seedSystemTier()
	pre, err := f.docs.List(context.Background(), document.ListOptions{}, nil)
	require.NoError(t, err)
	out := f.call()
	post, err := f.docs.List(context.Background(), document.ListOptions{}, nil)
	require.NoError(t, err)

	assert.Equal(t, len(pre), len(post), "satellites_init must NOT write substrate rows (pre=%d post=%d)", len(pre), len(post))

	assert.Equal(t, document.ScopeSystem, out.WorkspaceOverrides.Orchestrator.Scope)
	assert.Contains(t, out.WorkspaceOverrides.Orchestrator.Recommendation, "consuming the universal default")
	assert.NotEmpty(t, out.WorkspaceOverrides.Orchestrator.DocID)

	assert.Equal(t, document.ScopeSystem, out.WorkspaceOverrides.Workflow.Scope)
	assert.Contains(t, out.WorkspaceOverrides.Workflow.Recommendation, "consuming the universal default")
}

// TestSatellitesInit_RecommendsUsingOverride — AC11 flip case. With
// a workspace-tier override authored, the recommendation flips to
// the "using your workspace override" sentinel.
func TestSatellitesInit_RecommendsUsingOverride(t *testing.T) {
	f := newRecommendationFixture(t)
	f.seedSystemTier()
	wsOrchID := f.seedWorkspaceOverride(document.TypeAgent, "claude_orchestrator")
	wsWfID := f.seedWorkspaceOverride(document.TypeWorkflow, "default")

	out := f.call()

	assert.Equal(t, document.ScopeWorkspace, out.WorkspaceOverrides.Orchestrator.Scope)
	assert.Equal(t, wsOrchID, out.WorkspaceOverrides.Orchestrator.DocID)
	assert.Equal(t, "using your workspace override", out.WorkspaceOverrides.Orchestrator.Recommendation)

	assert.Equal(t, document.ScopeWorkspace, out.WorkspaceOverrides.Workflow.Scope)
	assert.Equal(t, wsWfID, out.WorkspaceOverrides.Workflow.DocID)
	assert.Equal(t, "using your workspace override", out.WorkspaceOverrides.Workflow.Recommendation)
}

// TestSatellitesInit_ChainCoverage — AC11b. Canonical chain
// contracts list enumerated with each contract's resolved tier; a
// missing contract surfaces on `missing_contracts` and the
// recommendation text names the missing contract.
func TestSatellitesInit_ChainCoverage(t *testing.T) {
	f := newRecommendationFixture(t)
	f.seedSystemTier("develop") // omit develop to drive the missing path
	out := f.call()
	assert.Equal(t, document.ScopeSystem, out.ChainCoverage.WorkflowSource)
	require.GreaterOrEqual(t, len(out.ChainCoverage.Contracts), 5,
		"chain_coverage.contracts must list all canonical contracts")
	names := map[string]string{}
	for _, e := range out.ChainCoverage.Contracts {
		names[e.Name] = e.FoundAt
	}
	assert.Equal(t, document.ScopeSystem, names["plan"])
	assert.Equal(t, document.ScopeSystem, names["commit"])
	assert.Equal(t, document.ScopeSystem, names["merge_to_main"])
	assert.Equal(t, "", names["develop"], "develop must NOT resolve when system tier omits it")
	assert.Contains(t, out.ChainCoverage.MissingContracts, "develop")
	assert.Contains(t, out.ChainCoverage.Recommendation, "develop")
}

// TestSatellitesInit_ChainCoverage_WorkspaceContractWins — workspace-
// tier contracts win over system. The chain_coverage report carries
// `found_at=workspace` for the overridden entries.
func TestSatellitesInit_ChainCoverage_WorkspaceContractWins(t *testing.T) {
	f := newRecommendationFixture(t)
	f.seedSystemTier()
	f.seedWorkspaceOverride(document.TypeContract, "commit")
	out := f.call()
	for _, e := range out.ChainCoverage.Contracts {
		if e.Name == "commit" {
			assert.Equal(t, document.ScopeWorkspace, e.FoundAt, "workspace-tier commit must win over system")
		}
	}
	assert.Contains(t, out.ChainCoverage.Recommendation, "All canonical chain contracts resolved")
}

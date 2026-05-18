package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/satellitesinit"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// seededInstallSchemaDocs returns a memory-backed document store with
// the canonical install schema artifact pre-seeded. The wire-adapter
// tests need the resolver to find the seeded row; production seeding
// goes through configseed.Run against the on-disk artifact file.
func seededInstallSchemaDocs(t *testing.T) document.Store {
	t.Helper()
	docs := document.NewMemoryStore()
	schema := satellitesinit.InstallSchema{
		TargetInstallPath: "./.satellites/satellites-client",
		TargetConfigPath:  "./.satellites/satellites-client.toml",
		DefaultConfig: satellitesinit.InstallSchemaDefaultConfig{
			RepoPath:       ".",
			WorktreeRoot:   "./.satellites/worktree",
			LogPath:        "./.satellites/logs",
			BranchTemplate: "client-{task_id}-from-{base_sha}",
		},
		AuthBootstrap: satellitesinit.InstallSchemaAuthBootstrap{
			Kind:    "auth_login",
			Command: "satellites-client auth login",
			EnvHint: "SATELLITES_TOKEN",
		},
	}
	structured, err := satellitesinit.MarshalSchema(schema)
	if err != nil {
		t.Fatalf("marshal install schema: %v", err)
	}
	_, err = docs.Create(context.Background(), document.Document{
		Type:       document.TypeArtifact,
		Scope:      document.ScopeSystem,
		Name:       satellitesinit.SystemDefaultName,
		Structured: structured,
		Tags:       []string{satellitesinit.KindTag, "seed", "configseed"},
		Status:     document.StatusActive,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed install schema: %v", err)
	}
	return docs
}

// fakeClientSession satisfies mark3labs/mcp-go's ClientSession interface
// for tests. SessionID() returns the injected id; the other methods are
// no-ops since handlers under test do not send notifications.
type fakeClientSession struct {
	id string
}

func (f *fakeClientSession) Initialize()       {}
func (f *fakeClientSession) Initialized() bool { return true }
func (f *fakeClientSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification {
	return make(chan mcpgo.JSONRPCNotification, 1)
}
func (f *fakeClientSession) SessionID() string { return f.id }

// satellitesInitFakeManifest stands up an httptest manifest server and
// returns the URL. Cleanup is registered with t.
func satellitesInitFakeManifest(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		    "version":"0.0.300",
		    "build":"2026-05-13-15-00-00",
		    "commit":"abc12345",
		    "artifacts":[
		      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"aaaa","download_url":"https://example.invalid/linux-amd64"},
		      {"os":"linux","arch":"arm64","filename":"satellites-client-linux-arm64","sha256":"bbbb","download_url":"https://example.invalid/linux-arm64"}
		    ]
		}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestHandleSatellitesInit_HappyPath drives the MCP wire adapter
// through a fake manifest and asserts the JSON wire shape carries the
// state machine + canonical install paths.
func TestHandleSatellitesInit_HappyPath(t *testing.T) {
	client.ResetSystemVersionCacheForTest()
	manifestURL := satellitesInitFakeManifest(t)

	s := &Server{
		startedAt: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC),
		deps:      client.Deps{ManifestURL: manifestURL, Documents: seededInstallSchemaDocs(t)},
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "satellites_init"
	req.Params.Arguments = map[string]any{
		"current_version": "0.0.299",
		"os":              "linux",
		"arch":            "amd64",
	}
	res, err := s.handleSatellitesInit(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSatellitesInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected ok result, got error: %+v", res)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	var payload struct {
		State             string `json:"state"`
		TargetInstallPath string `json:"target_install_path"`
		TargetConfigPath  string `json:"target_config_path"`
		DefaultConfig     struct {
			RepoPath       string `json:"repo_path"`
			WorktreeRoot   string `json:"worktree_root"`
			LogPath        string `json:"log_path"`
			BranchTemplate string `json:"branch_template"`
		} `json:"default_config"`
		Install struct {
			Version     string `json:"version"`
			OS          string `json:"os"`
			Arch        string `json:"arch"`
			Filename    string `json:"filename"`
			DownloadURL string `json:"download_url"`
			SHA256      string `json:"sha256"`
		} `json:"install"`
		AuthBootstrap struct {
			Kind    string `json:"kind"`
			Command string `json:"command"`
			EnvHint string `json:"env_hint"`
		} `json:"auth_bootstrap"`
		CurrentVersion     string `json:"current_version"`
		FetchedAt          string `json:"fetched_at"`
		WorkspaceOverrides struct {
			Orchestrator struct {
				Scope          string `json:"scope"`
				DocID          string `json:"doc_id"`
				Recommendation string `json:"recommendation"`
			} `json:"orchestrator"`
			Workflow struct {
				Scope          string `json:"scope"`
				DocID          string `json:"doc_id"`
				Recommendation string `json:"recommendation"`
			} `json:"workflow"`
		} `json:"workspace_overrides"`
		ChainCoverage struct {
			WorkflowSource string `json:"workflow_source"`
			WorkflowDocID  string `json:"workflow_doc_id"`
			Contracts      []struct {
				Name    string `json:"name"`
				FoundAt string `json:"found_at"`
				DocID   string `json:"doc_id"`
			} `json:"contracts"`
			MissingContracts []string `json:"missing_contracts"`
			Recommendation   string   `json:"recommendation"`
		} `json:"chain_coverage"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, text)
	}
	if payload.State != "update_available" {
		t.Errorf("state = %q, want update_available", payload.State)
	}
	if payload.TargetInstallPath != "./.satellites/satellites-client" {
		t.Errorf("target_install_path = %q", payload.TargetInstallPath)
	}
	if payload.TargetConfigPath != "./.satellites/satellites-client.toml" {
		t.Errorf("target_config_path = %q", payload.TargetConfigPath)
	}
	if payload.DefaultConfig.WorktreeRoot != "./.satellites/worktree" ||
		payload.DefaultConfig.LogPath != "./.satellites/logs" ||
		payload.DefaultConfig.BranchTemplate != "client-{task_id}-from-{base_sha}" {
		t.Errorf("default_config drift: %+v", payload.DefaultConfig)
	}
	if payload.Install.Version != "0.0.300" || payload.Install.Filename != "satellites-client-linux-amd64" {
		t.Errorf("install drift: %+v", payload.Install)
	}
	if payload.AuthBootstrap.Kind != "auth_login" {
		t.Errorf("auth_bootstrap.kind = %q", payload.AuthBootstrap.Kind)
	}
	if payload.CurrentVersion != "0.0.299" {
		t.Errorf("current_version = %q", payload.CurrentVersion)
	}
	if payload.FetchedAt == "" {
		t.Errorf("fetched_at empty")
	}
	// workspace_overrides and chain_coverage must round-trip through
	// the wire adapter — sty_a4c98504 regression: the hand-rolled
	// map[string]any in the old handler dropped both keys.
	if payload.WorkspaceOverrides.Orchestrator.Scope == "" {
		t.Errorf("workspace_overrides.orchestrator.scope empty (key likely missing from wire payload)")
	}
	if payload.WorkspaceOverrides.Orchestrator.Recommendation == "" {
		t.Errorf("workspace_overrides.orchestrator.recommendation empty")
	}
	if payload.WorkspaceOverrides.Workflow.Scope == "" {
		t.Errorf("workspace_overrides.workflow.scope empty (key likely missing from wire payload)")
	}
	if payload.WorkspaceOverrides.Workflow.Recommendation == "" {
		t.Errorf("workspace_overrides.workflow.recommendation empty")
	}
	if payload.ChainCoverage.Recommendation == "" {
		t.Errorf("chain_coverage.recommendation empty (key likely missing from wire payload)")
	}
}

// TestHandleSatellitesInit_ManifestURLMissing surfaces the typed error
// as an MCP tool error envelope rather than a panic.
func TestHandleSatellitesInit_ManifestURLMissing(t *testing.T) {
	client.ResetSystemVersionCacheForTest()
	s := &Server{
		startedAt: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC),
		deps:      client.Deps{ManifestURL: ""},
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "satellites_init"
	res, err := s.handleSatellitesInit(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSatellitesInit: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result, got: %+v", res)
	}
}

// TestHandleSatellitesInit_KindReadyOnBoundSession reproduces sty_245a95bf's
// AC1: when the caller's ctx carries (a) an authed CallerIdentity and (b)
// a ClientSession whose SessionID matches a pre-staged session row
// stamped with ActiveProjectID, the handler must return
// `auth_bootstrap.kind == "ready"` and `agent_api_key.key` non-empty.
//
// Pprod symptom under reproduction: `kind=auth_login` returns despite a
// valid project_set having stamped the session. Reproducing this in a
// unit test against the typed handler isolates whether the bug lives
// in callerActiveProjectID(ctx, ...) -> Sessions.Get OR somewhere
// further up the streamable transport.
func TestHandleSatellitesInit_KindReadyOnBoundSession(t *testing.T) {
	client.ResetSystemVersionCacheForTest()
	manifestURL := satellitesInitFakeManifest(t)

	const (
		userID    = "u_test_bound"
		userEmail = "bound@test.local"
		sessionID = "sess_test_bound"
		wsID      = "wksp_test_bound"
	)

	now := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	sessions := session.NewMemoryStore()
	projects := project.NewMemoryStore()
	workspaces := workspace.NewMemoryStore()
	ws, err := workspaces.Create(context.Background(), userID, "test-bound-ws", now)
	if err != nil {
		t.Fatalf("workspace.Create: %v", err)
	}
	_ = wsID // kept for documentation; the actual ws id is the store-allocated one
	proj, err := projects.Create(context.Background(), userID, ws.ID, "test-bound", now)
	if err != nil {
		t.Fatalf("project.Create: %v", err)
	}
	_, err = sessions.Register(context.Background(), userID, sessionID, session.SourceSessionStart, now)
	if err != nil {
		t.Fatalf("session.Register: %v", err)
	}
	_, err = sessions.SetActiveProject(context.Background(), userID, sessionID, proj.ID, now)
	if err != nil {
		t.Fatalf("session.SetActiveProject: %v", err)
	}

	s := &Server{
		startedAt: now,
		mcp:       mcpsrv.NewMCPServer("test", "0.0.0"),
		deps: client.Deps{
			ManifestURL: manifestURL,
			StartedAt:   now,
			Sessions:    sessions,
			APIKeys:     auth.NewMemoryAgentAPIKeyStore(),
			Projects:    projects,
			Workspaces:  workspaces,
			Documents:   seededInstallSchemaDocs(t),
		},
	}

	// Build ctx: authed caller + injected ClientSession whose SessionID
	// matches the pre-staged row. Note: caller.Memberships is set on the
	// CallerIdentity for completeness, but the bug fix routes memberships
	// through in.Memberships (set by the handler from
	// resolveCallerMemberships) and assigns them onto cc.Memberships
	// before calling SatellitesInit.
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: userID,
		Email:  userEmail,
		Source: "oauth:test",
	})
	ctx = s.mcp.WithContext(ctx, &fakeClientSession{id: sessionID})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "satellites_init"
	req.Params.Arguments = map[string]any{
		"os":   "linux",
		"arch": "amd64",
	}
	res, err := s.handleSatellitesInit(ctx, req)
	if err != nil {
		t.Fatalf("handleSatellitesInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected ok result, got error: %+v", res)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	var payload struct {
		AuthBootstrap struct {
			Kind   string `json:"kind"`
			Source string `json:"source"`
		} `json:"auth_bootstrap"`
		AgentAPIKey *struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"agent_api_key"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, text)
	}
	if payload.AuthBootstrap.Kind != "ready" {
		t.Errorf("auth_bootstrap.kind = %q, want %q (raw=%s)", payload.AuthBootstrap.Kind, "ready", text)
	}
	if payload.AuthBootstrap.Source != "minted_at_init" {
		t.Errorf("auth_bootstrap.source = %q, want %q", payload.AuthBootstrap.Source, "minted_at_init")
	}
	if payload.AgentAPIKey == nil {
		t.Fatalf("agent_api_key missing from payload (raw=%s)", text)
	}
	if payload.AgentAPIKey.Key == "" {
		t.Errorf("agent_api_key.key empty on fresh mint")
	}
	if payload.AgentAPIKey.Source != "minted_at_init" {
		t.Errorf("agent_api_key.source = %q, want minted_at_init", payload.AgentAPIKey.Source)
	}
	_ = wsID // workspace_id is informational on the payload; not asserted here
}

// TestHandleSatellitesInit_KindReadyIdempotent — AC2 of sty_245a95bf.
// Second call with same (caller, project, agent_name) returns
// source=existing_key + empty cleartext.
func TestHandleSatellitesInit_KindReadyIdempotent(t *testing.T) {
	client.ResetSystemVersionCacheForTest()
	manifestURL := satellitesInitFakeManifest(t)

	const (
		userID    = "u_test_idem"
		sessionID = "sess_test_idem"
	)

	now := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	sessions := session.NewMemoryStore()
	projects := project.NewMemoryStore()
	workspaces := workspace.NewMemoryStore()
	ws, _ := workspaces.Create(context.Background(), userID, "test-idem-ws", now)
	proj, _ := projects.Create(context.Background(), userID, ws.ID, "test-idem", now)
	_, _ = sessions.Register(context.Background(), userID, sessionID, session.SourceSessionStart, now)
	_, _ = sessions.SetActiveProject(context.Background(), userID, sessionID, proj.ID, now)

	s := &Server{
		startedAt: now,
		mcp:       mcpsrv.NewMCPServer("test", "0.0.0"),
		deps: client.Deps{
			ManifestURL: manifestURL,
			StartedAt:   now,
			Sessions:    sessions,
			APIKeys:     auth.NewMemoryAgentAPIKeyStore(),
			Projects:    projects,
			Workspaces:  workspaces,
			Documents:   seededInstallSchemaDocs(t),
		},
	}

	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{
		UserID: userID, Email: "idem@test.local", Source: "oauth:test",
	})
	ctx = s.mcp.WithContext(ctx, &fakeClientSession{id: sessionID})
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "satellites_init"
	req.Params.Arguments = map[string]any{"os": "linux", "arch": "amd64"}

	// First call mints.
	res1, _ := s.handleSatellitesInit(ctx, req)
	if res1.IsError {
		t.Fatalf("first call error: %+v", res1)
	}
	// Second call returns existing.
	res2, _ := s.handleSatellitesInit(ctx, req)
	if res2.IsError {
		t.Fatalf("second call error: %+v", res2)
	}
	text2 := res2.Content[0].(mcpgo.TextContent).Text
	var p2 struct {
		AgentAPIKey *struct {
			Key    string `json:"key"`
			Source string `json:"source"`
		} `json:"agent_api_key"`
	}
	_ = json.Unmarshal([]byte(text2), &p2)
	if p2.AgentAPIKey == nil {
		t.Fatalf("second call missing agent_api_key (raw=%s)", text2)
	}
	if p2.AgentAPIKey.Source != "existing_key" {
		t.Errorf("second-call source = %q, want existing_key", p2.AgentAPIKey.Source)
	}
	if p2.AgentAPIKey.Key != "" {
		t.Errorf("second-call key cleartext leaked: %q (should be empty on existing_key)", p2.AgentAPIKey.Key)
	}
}

// TestHandleSatellitesInit_UnknownOSArch returns an MCP error result
// rather than a half-populated payload when no artifact matches.
func TestHandleSatellitesInit_UnknownOSArch(t *testing.T) {
	client.ResetSystemVersionCacheForTest()
	manifestURL := satellitesInitFakeManifest(t)

	s := &Server{
		startedAt: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC),
		deps:      client.Deps{ManifestURL: manifestURL},
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "satellites_init"
	req.Params.Arguments = map[string]any{
		"os":   "plan9",
		"arch": "mips",
	}
	res, err := s.handleSatellitesInit(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSatellitesInit: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result, got: %+v", res)
	}
}

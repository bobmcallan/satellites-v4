package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/client"
)

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
		deps:      client.Deps{ManifestURL: manifestURL},
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
		CurrentVersion string `json:"current_version"`
		FetchedAt      string `json:"fetched_at"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, text)
	}
	if payload.State != "update_available" {
		t.Errorf("state = %q, want update_available", payload.State)
	}
	if payload.TargetInstallPath != "./satellites/satellites-client" {
		t.Errorf("target_install_path = %q", payload.TargetInstallPath)
	}
	if payload.TargetConfigPath != "./satellites/satellites-client.toml" {
		t.Errorf("target_config_path = %q", payload.TargetConfigPath)
	}
	if payload.DefaultConfig.WorktreeRoot != "./satellites/worktree" ||
		payload.DefaultConfig.LogPath != "./satellites/logs" ||
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

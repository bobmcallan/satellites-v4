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

// TestHandleSystemVersion_HappyPath drives the MCP wire adapter
// through a fake manifest httptest.Server and asserts the JSON wire
// shape (version/build/commit/artifacts/fetched_at).
func TestHandleSystemVersion_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		    "version":"0.0.269",
		    "build":"2026-05-13-12-00-00",
		    "commit":"abcd1234",
		    "artifacts":[
		      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"deadbeef","download_url":"https://example.invalid/a"}
		    ]
		}`))
	}))
	t.Cleanup(upstream.Close)

	s := &Server{
		startedAt: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC),
		deps:      client.Deps{ManifestURL: upstream.URL},
	}
	res, err := s.handleSystemVersion(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleSystemVersion: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected ok result, got error: %+v", res)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	var payload struct {
		Version   string                         `json:"version"`
		Build     string                         `json:"build"`
		Commit    string                         `json:"commit"`
		Artifacts []client.SystemVersionArtifact `json:"artifacts"`
		FetchedAt string                         `json:"fetched_at"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, text)
	}
	if payload.Version != "0.0.269" {
		t.Errorf("version = %q, want 0.0.269", payload.Version)
	}
	if len(payload.Artifacts) != 1 || payload.Artifacts[0].Filename != "satellites-client-linux-amd64" {
		t.Errorf("artifacts mismatch: %+v", payload.Artifacts)
	}
	if payload.FetchedAt == "" {
		t.Errorf("fetched_at empty")
	}
}

// TestHandleSystemVersion_ManifestURLMissing surfaces the typed error
// as an MCP tool error envelope rather than a panic.
func TestHandleSystemVersion_ManifestURLMissing(t *testing.T) {
	// Reset cache to ensure deterministic behaviour across runs.
	client.ResetSystemVersionCacheForTest()
	s := &Server{
		startedAt: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC),
		deps:      client.Deps{ManifestURL: ""},
	}
	res, err := s.handleSystemVersion(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleSystemVersion: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result, got: %+v", res)
	}
}

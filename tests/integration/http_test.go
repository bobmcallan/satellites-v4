package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestHealthzReturnsVersion boots the satellites server in a testcontainer
// (built from docker/Dockerfile), hits /healthz, and asserts the JSON shape
// and non-empty version/commit fields.
func TestHealthzReturnsVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	baseURL, stop := startServerContainer(t, ctx)
	defer stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("healthz request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(b))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Request-ID"); got == "" {
		t.Errorf("expected X-Request-ID header on response")
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"version", "build", "commit", "started_at", "uptime_seconds"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing key %q in healthz body: %+v", k, body)
		}
	}
	if v, _ := body["version"].(string); v == "" {
		t.Errorf("version is empty, want stamped value")
	}
	if c, _ := body["commit"].(string); c == "" {
		t.Errorf("commit is empty, want stamped value")
	}
}

//go:build pprod

// Package integration's PPROD smoke is deliberately hidden behind the
// `pprod` build tag: the default `go test ./tests/integration/...` run stays
// under the 60 s local budget, while operators can run this file explicitly
// after a deploy to verify the three things that matter.
//
//	SATELLITES_PPROD_URL=https://satellites-pprod.fly.dev \
//	SATELLITES_PPROD_API_KEY=<key> \
//	go test -tags=pprod ./tests/integration/... -run Pprod
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPprodSmoke(t *testing.T) {
	url := os.Getenv("SATELLITES_PPROD_URL")
	if url == "" {
		t.Fatal("SATELLITES_PPROD_URL is required for the PPROD smoke; set to the live host (e.g. https://satellites-pprod.fly.dev)")
	}
	apiKey := os.Getenv("SATELLITES_PPROD_API_KEY")
	if apiKey == "" {
		t.Fatal("SATELLITES_PPROD_API_KEY is required for the PPROD smoke; set to a Bearer token the server accepts")
	}
	url = strings.TrimRight(url, "/")

	expectedCommit := currentShortCommit(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Run("login_page", func(t *testing.T) {
		body := httpGet(t, ctx, url+"/login", nil)
		if !strings.Contains(body, `action="/auth/login"`) {
			t.Errorf("login page missing form action; got %d bytes", len(body))
		}
	})

	t.Run("healthz_commit_match", func(t *testing.T) {
		deadline := time.Now().Add(2 * time.Minute)
		var lastCommit string
		for {
			body := httpGet(t, ctx, url+"/healthz", nil)
			var payload map[string]any
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatalf("healthz JSON decode: %v; raw=%s", err, body)
			}
			if c, _ := payload["commit"].(string); c != "" {
				lastCommit = c
				if strings.HasPrefix(c, expectedCommit) || strings.HasPrefix(expectedCommit, c) {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("healthz commit = %q after 2 min; want prefix match with local HEAD %q", lastCommit, expectedCommit)
			}
			time.Sleep(5 * time.Second)
		}
	})

	t.Run("mcp_info", func(t *testing.T) {
		req := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "satellites_info",
				"arguments": map[string]any{},
			},
		}
		body := rpcPprod(t, ctx, url+"/mcp", apiKey, req)
		result, _ := body["result"].(map[string]any)
		content, _ := result["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("satellites_info returned no content: %+v", body)
		}
		first, _ := content[0].(map[string]any)
		text, _ := first["text"].(string)
		var payload map[string]any
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatalf("satellites_info text decode: %v; raw=%s", err, text)
		}
		if v, _ := payload["version"].(string); v == "" {
			t.Errorf("satellites_info version empty; payload=%+v", payload)
		}
	})
}

func currentShortCommit(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--short=8", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func httpGet(t *testing.T, ctx context.Context, url string, headers map[string]string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("GET %s status=%d body=%s", url, resp.StatusCode, string(b))
	}
	return string(b)
}

func rpcPprod(t *testing.T, ctx context.Context, mcpURL, apiKey string, body any) map[string]any {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pprod mcp rpc: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mcp rpc status=%d body=%s", resp.StatusCode, string(b))
	}
	raw, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				var out map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &out); err != nil {
					t.Fatalf("sse decode: %v; raw=%s", err, string(raw))
				}
				return out
			}
		}
		t.Fatalf("no data: line in SSE response; raw=%s", string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json decode: %v; raw=%s", err, string(raw))
	}
	return out
}

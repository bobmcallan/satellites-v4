package cliremote_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// stubServer wraps an httptest server that asserts the JSON-RPC
// shape and replies with the supplied body.
func stubServer(t *testing.T, status int, body string) (*httptest.Server, *cliremote.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			http.Error(w, "content-type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, cliremote.New(srv.URL, "test-bearer", nil)
}

func TestCall_HappyPath(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"id": "task_x", "kind": "work"})
	body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + jsonString(string(payload)) + `}],"isError":false}}`
	_, client := stubServer(t, http.StatusOK, body)

	var got struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if err := client.Call(context.Background(), "task_get", map[string]any{"id": "task_x"}, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.ID != "task_x" || got.Kind != "work" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestCall_NotFoundEnvelope(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"story not found"}],"isError":true}}`
	_, client := stubServer(t, http.StatusOK, body)
	err := client.Call(context.Background(), "story_get", map[string]any{"id": "sty_x"}, nil)
	if got := cliexit.Resolve(err); got != cliexit.NotFound {
		t.Fatalf("Resolve(NotFoundEnvelope) = %d, want %d", got, cliexit.NotFound)
	}
}

func TestCall_UnauthorizedHTTP(t *testing.T) {
	_, client := stubServer(t, http.StatusUnauthorized, "")
	err := client.Call(context.Background(), "session_whoami", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Auth {
		t.Fatalf("Resolve(401) = %d, want %d", got, cliexit.Auth)
	}
}

func TestCall_ForbiddenHTTP(t *testing.T) {
	_, client := stubServer(t, http.StatusForbidden, "")
	err := client.Call(context.Background(), "session_whoami", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Auth {
		t.Fatalf("Resolve(403) = %d, want %d", got, cliexit.Auth)
	}
}

func TestCall_NotFoundHTTP(t *testing.T) {
	_, client := stubServer(t, http.StatusNotFound, "")
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.NotFound {
		t.Fatalf("Resolve(404) = %d, want %d", got, cliexit.NotFound)
	}
}

func TestCall_ServerError(t *testing.T) {
	_, client := stubServer(t, http.StatusInternalServerError, "boom")
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("Resolve(500) = %d, want %d", got, cliexit.Server)
	}
}

func TestCall_AuthEnvelope(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"unauthorized: bearer expired"}],"isError":true}}`
	_, client := stubServer(t, http.StatusOK, body)
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Auth {
		t.Fatalf("Resolve(authEnvelope) = %d, want %d", got, cliexit.Auth)
	}
}

func TestCall_ParseFailure(t *testing.T) {
	_, client := stubServer(t, http.StatusOK, "not json")
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("Resolve(parseFail) = %d, want %d", got, cliexit.Server)
	}
}

func TestCall_NilClient(t *testing.T) {
	var c *cliremote.Client
	err := c.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("Resolve(nilClient) = %d, want %d", got, cliexit.Server)
	}
}

// jsonString quotes the supplied raw text for embedding into a
// json-rpc text envelope.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

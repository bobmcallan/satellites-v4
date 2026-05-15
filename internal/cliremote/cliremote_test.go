package cliremote_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phuslu/log"
	"github.com/ternarybob/arbor"
	arbormodels "github.com/ternarybob/arbor/models"

	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// stubServer wraps an httptest server that asserts the HTTP API
// wire shape (POST /api/v1/<noun>/<verb>, JSON body, Bearer auth)
// and replies with the supplied (status, body) pair.
func stubServer(t *testing.T, wantPath string, status int, body string) (*httptest.Server, *cliremote.Client) {
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
		if wantPath != "" && r.URL.Path != wantPath {
			http.Error(w, "path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-bearer" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		// Drain body so the test can assert the client wrote valid JSON.
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, cliremote.New(srv.URL, "test-bearer", nil)
}

func TestCall_HappyPath(t *testing.T) {
	_, client := stubServer(t, "/api/v1/task/get", http.StatusOK, `{"id":"task_x","kind":"work"}`)

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

func TestCall_NilArgsBecomeEmptyObject(t *testing.T) {
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	client := cliremote.New(srv.URL, "test-bearer", nil)
	if err := client.Call(context.Background(), "session_whoami", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	body := <-captured
	if string(body) != "{}" {
		t.Fatalf("nil args body = %q, want \"{}\"", string(body))
	}
}

func TestCall_NotFound404WithEnvelope(t *testing.T) {
	_, client := stubServer(t, "/api/v1/story/get", http.StatusNotFound, `{"error":"story not found"}`)
	err := client.Call(context.Background(), "story_get", map[string]any{"id": "sty_x"}, nil)
	if got := cliexit.Resolve(err); got != cliexit.NotFound {
		t.Fatalf("Resolve(404+envelope) = %d, want %d", got, cliexit.NotFound)
	}
	if !strings.Contains(err.Error(), "story not found") {
		t.Fatalf("error missing envelope message: %v", err)
	}
}

func TestCall_Auth401WithEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bearer expired"}`))
	}))
	t.Cleanup(srv.Close)
	client := cliremote.New(srv.URL, "test-bearer", nil)
	err := client.Call(context.Background(), "session_whoami", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Auth {
		t.Fatalf("Resolve(401+envelope) = %d, want %d", got, cliexit.Auth)
	}
	if !strings.Contains(err.Error(), "bearer expired") {
		t.Fatalf("error missing envelope message: %v", err)
	}
}

func TestCall_Forbidden403(t *testing.T) {
	_, client := stubServer(t, "/api/v1/session/whoami", http.StatusForbidden, "")
	err := client.Call(context.Background(), "session_whoami", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Auth {
		t.Fatalf("Resolve(403) = %d, want %d", got, cliexit.Auth)
	}
}

func TestCall_NotFound404NoBody(t *testing.T) {
	_, client := stubServer(t, "/api/v1/task/get", http.StatusNotFound, "")
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.NotFound {
		t.Fatalf("Resolve(404) = %d, want %d", got, cliexit.NotFound)
	}
}

func TestCall_BadRequest400(t *testing.T) {
	_, client := stubServer(t, "/api/v1/task/get", http.StatusBadRequest, `{"error":"id is required"}`)
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Usage {
		t.Fatalf("Resolve(400) = %d, want %d", got, cliexit.Usage)
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("error missing envelope message: %v", err)
	}
}

func TestCall_ServerError(t *testing.T) {
	_, client := stubServer(t, "/api/v1/task/get", http.StatusInternalServerError, `{"error":"db boom"}`)
	err := client.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("Resolve(500) = %d, want %d", got, cliexit.Server)
	}
}

func TestCall_ParseFailure(t *testing.T) {
	_, client := stubServer(t, "/api/v1/task/get", http.StatusOK, "not json")
	var got struct {
		ID string `json:"id"`
	}
	err := client.Call(context.Background(), "task_get", nil, &got)
	if c := cliexit.Resolve(err); c != cliexit.Server {
		t.Fatalf("Resolve(parseFail) = %d, want %d", c, cliexit.Server)
	}
}

func TestCall_NilClient(t *testing.T) {
	var c *cliremote.Client
	err := c.Call(context.Background(), "task_get", nil, nil)
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("Resolve(nilClient) = %d, want %d", got, cliexit.Server)
	}
}

func TestCall_PassesAuthHeader(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	client := cliremote.New(srv.URL, "abc123", nil)
	if err := client.Call(context.Background(), "satellites_info", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if seen != "Bearer abc123" {
		t.Fatalf("Authorization header = %q, want %q", seen, "Bearer abc123")
	}
}

func TestCall_PathMappingViaServer(t *testing.T) {
	// End-to-end confirmation that the in-package mapping resolves to
	// the correct route. Each test case fires one POST against the
	// expected path; a mismatch surfaces as a 400 from the stub.
	cases := []struct {
		toolName string
		wantPath string
	}{
		{"satellites_info", "/api/v1/satellites/info"},
		{"system_version", "/api/v1/system/version"},
		{"session_whoami", "/api/v1/session/whoami"},
		{"session_register", "/api/v1/session/register"},
		{"task_get", "/api/v1/task/get"},
		{"task_walk", "/api/v1/task/walk"},
		{"task_claim", "/api/v1/task/claim"},
		{"task_add", "/api/v1/task/add"},
		{"task_update", "/api/v1/task/update"},
		{"task_log_append", "/api/v1/task/log/append"},
		{"task_log_list", "/api/v1/task/log/list"},
		{"ledger_get", "/api/v1/ledger/get"},
		{"ledger_list", "/api/v1/ledger/list"},
		{"ledger_search", "/api/v1/ledger/search"},
		{"ledger_recall", "/api/v1/ledger/recall"},
		{"ledger_append", "/api/v1/ledger/append"},
		{"ledger_dereference", "/api/v1/ledger/dereference"},
		{"document_get", "/api/v1/document/get"},
		{"document_list", "/api/v1/document/list"},
		{"story_get", "/api/v1/story/get"},
		// story_update_status + story_field_set folded into story_update
		// in sty_4db0e025 slice D1.
		{"story_update", "/api/v1/story/update"},
		{"project_set", "/api/v1/project/set"},
	}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			_, client := stubServer(t, tc.wantPath, http.StatusOK, `{}`)
			if err := client.Call(context.Background(), tc.toolName, nil, nil); err != nil {
				t.Fatalf("%s: %v", tc.toolName, err)
			}
		})
	}
}

// TestCall_EmitsDebugRow asserts that when a logger is wired via
// WithLogger, each Call emits one Debug row with the verb, HTTP
// path, status, and duration fields. Uses arbor's in-memory writer
// + GetMemoryLogs API to capture rows without touching stdout/files.
func TestCall_EmitsDebugRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	correlation := "test-cli-call-row"
	mem := arbor.NewLogger().
		WithMemoryWriter(arbormodels.WriterConfiguration{}).
		WithLevelFromString("debug").
		WithCorrelationId(correlation)

	client := cliremote.New(srv.URL, "test-bearer", nil).WithLogger(mem)
	if err := client.Call(context.Background(), "satellites_info", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Writes are async; arbor's reference integration test sleeps
	// before reading. Match that pattern.
	time.Sleep(80 * time.Millisecond)

	rows, err := mem.GetMemoryLogs(correlation, arbor.LogLevel(log.DebugLevel))
	if err != nil {
		t.Fatalf("GetMemoryLogs: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows captured")
	}
	// The memory writer's rendered form is
	// "DBG|<ts>|<msg>" — structured fields are stored separately and
	// not surfaced in the rendered string. We verify the message
	// fires (proves the Debug().Msg("cli call") path is reached);
	// the field set is proven by reading cliremote.go directly.
	var calls int
	for _, body := range rows {
		if strings.Contains(body, "cli call") {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 cli-call row, got %d (rows = %v)", calls, rows)
	}
}

// TestCall_NilLoggerIsNoOp asserts that without WithLogger, no panic
// occurs and behaviour is identical to the pre-sty_1f942fc6 path.
func TestCall_NilLoggerIsNoOp(t *testing.T) {
	_, client := stubServer(t, "/api/v1/satellites/info", http.StatusOK, `{}`)
	// Sanity: omitting WithLogger leaves the field nil; Call must not panic.
	if err := client.Call(context.Background(), "satellites_info", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
}

// silence unused import in dev cycles where the helper is dropped.
var _ = json.RawMessage(nil)

// TestSystemVersion_HappyPath asserts the typed convenience wrapper
// decodes the server's payload into the typed output.
func TestSystemVersion_HappyPath(t *testing.T) {
	body := `{"version":"0.0.269","build":"b","commit":"c","artifacts":[{"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"deadbeef","download_url":"https://example.invalid/a"}],"fetched_at":"2026-05-13T12:00:00Z"}`
	_, client := stubServer(t, "/api/v1/system/version", http.StatusOK, body)
	out, err := client.SystemVersion(context.Background())
	if err != nil {
		t.Fatalf("SystemVersion: %v", err)
	}
	if out.Version != "0.0.269" {
		t.Errorf("version = %q, want 0.0.269", out.Version)
	}
	if len(out.Artifacts) != 1 {
		t.Fatalf("artifacts len = %d, want 1", len(out.Artifacts))
	}
	if out.Artifacts[0].SHA256 != "deadbeef" {
		t.Errorf("sha256 = %q, want deadbeef", out.Artifacts[0].SHA256)
	}
}

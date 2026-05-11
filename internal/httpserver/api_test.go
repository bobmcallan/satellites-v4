package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// expectedRoutes lists the 20 verb routes the HTTP API surface
// exposes after sty_068a6c46. The integration test in
// tests/api/api_integration_test.go drives each against a live
// satellites-server boot via testcontainers (AC-4); this smoke
// test asserts that registration is complete and the mux is wired.
var expectedRoutes = []string{
	"POST /api/v1/satellites/info",
	"POST /api/v1/session/whoami",
	"POST /api/v1/session/register",
	"POST /api/v1/ledger/get",
	"POST /api/v1/ledger/list",
	"POST /api/v1/ledger/search",
	"POST /api/v1/ledger/recall",
	"POST /api/v1/ledger/append",
	"POST /api/v1/ledger/dereference",
	"POST /api/v1/document/get",
	"POST /api/v1/document/list",
	"POST /api/v1/task/get",
	"POST /api/v1/task/walk",
	"POST /api/v1/task/claim",
	"POST /api/v1/task/update",
	"POST /api/v1/task/add",
	"POST /api/v1/task/plan",
	"POST /api/v1/story/update-status",
	"POST /api/v1/story/field-set",
	"POST /api/v1/project/set",
}

// TestAPI_RoutesRegistered asserts the registrar attaches all 20
// verb routes to the supplied mux. Routes that respond 404 indicate
// missing registration; any other status (400/500/200) proves the
// handler was hit.
func TestAPI_RoutesRegistered(t *testing.T) {
	if got := len(expectedRoutes); got != 20 {
		t.Fatalf("expected 20 routes, got %d (update the slice as the verb set grows)", got)
	}

	reg := NewAPIRegistrar(client.New(client.Deps{StartedAt: time.Now().UTC()}))
	mux := http.NewServeMux()
	reg.Register(mux)

	for _, route := range expectedRoutes {
		t.Run(route, func(t *testing.T) {
			parts := strings.SplitN(route, " ", 2)
			method, path := parts[0], parts[1]
			req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}")))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("route %s returned 404 — not registered", route)
			}
		})
	}
}

// TestAPI_SatellitesInfo_HappyPath asserts the simplest end-to-end
// flow: caller resolved on ctx via auth.WithCaller, the registrar's
// handler delegates to client.SatellitesInfo, and the JSON response
// matches the typed-method output.
func TestAPI_SatellitesInfo_HappyPath(t *testing.T) {
	startedAt := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	reg := NewAPIRegistrar(client.New(client.Deps{StartedAt: startedAt}))
	mux := http.NewServeMux()
	reg.Register(mux)

	req := httptest.NewRequest("POST", "/api/v1/satellites/info", nil)
	req = req.WithContext(auth.WithCaller(req.Context(), auth.CallerIdentity{
		Email:  "operator@example.com",
		UserID: "u_test",
		Source: "apikey",
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		UserEmail string `json:"user_email"`
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.UserEmail != "operator@example.com" {
		t.Errorf("user_email = %q, want operator@example.com", got.UserEmail)
	}
	if got.StartedAt == "" {
		t.Errorf("started_at empty")
	}
}

// TestAPI_ErrorEnvelope_Shape pins the JSON error envelope so the
// MCP-parity integration test (AC-4) can assert byte-equal decoded
// outputs modulo documented exemptions.
func TestAPI_ErrorEnvelope_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAPIStatus(rec, http.StatusBadRequest, "test error")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	var env apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "test error" {
		t.Errorf("error = %q, want %q", env.Error, "test error")
	}
}

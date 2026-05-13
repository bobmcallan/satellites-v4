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

// expectedRoutes lists the verb routes the HTTP API surface exposes.
// sty_068a6c46 shipped 20; sty_73207fc8 added /api/v1/story/get;
// sty_ef248ab2 added 30 operator-tier reads; sty_f38bd573 Tier A added
// 37 operator-tier mutates; sty_004f3d3a added 9 reviewer/role/skill
// read wrappers; sty_0b419d98 Tier B added 7; sty_e68ce6fb added
// portal/replicate. Total: 105. Every CLI verb is now non-stub.
var expectedRoutes = []string{
	// Order:07a anchor (21).
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
	"POST /api/v1/story/get",
	"POST /api/v1/story/update-status",
	"POST /api/v1/story/field-set",
	"POST /api/v1/project/set",
	// sty_ef248ab2 operator-tier reads (28).
	"POST /api/v1/story/list",
	"POST /api/v1/story/template-get",
	"POST /api/v1/story/template-list",
	"POST /api/v1/story/export-walk",
	"POST /api/v1/task/list",
	"POST /api/v1/project/get",
	"POST /api/v1/project/list",
	"POST /api/v1/workspace/get",
	"POST /api/v1/workspace/list",
	"POST /api/v1/workspace/member-list",
	"POST /api/v1/changelog/get",
	"POST /api/v1/changelog/list",
	"POST /api/v1/document/search",
	"POST /api/v1/agent/list",
	"POST /api/v1/agent/search",
	"POST /api/v1/agent/ephemeral-summary",
	"POST /api/v1/contract/list",
	"POST /api/v1/contract/search",
	"POST /api/v1/principle/get",
	"POST /api/v1/principle/search",
	"POST /api/v1/kv/get",
	"POST /api/v1/kv/list",
	"POST /api/v1/kv/get-resolved",
	"POST /api/v1/repo/get",
	"POST /api/v1/repo/list",
	"POST /api/v1/repo/search",
	"POST /api/v1/repo/search-text",
	"POST /api/v1/repo/get-symbol-source",
	"POST /api/v1/repo/get-file",
	"POST /api/v1/repo/get-outline",
	// sty_f38bd573 Tier A operator-tier mutates (37).
	"POST /api/v1/story/add",
	"POST /api/v1/story/update",
	"POST /api/v1/story/delete",
	"POST /api/v1/project/create",
	"POST /api/v1/project/update",
	"POST /api/v1/project/delete",
	"POST /api/v1/workspace/create",
	"POST /api/v1/workspace/member-add",
	"POST /api/v1/workspace/member-update-role",
	"POST /api/v1/workspace/member-remove",
	"POST /api/v1/kv/set",
	"POST /api/v1/kv/delete",
	"POST /api/v1/changelog/add",
	"POST /api/v1/changelog/update",
	"POST /api/v1/changelog/delete",
	"POST /api/v1/repo/add",
	"POST /api/v1/document/add",
	"POST /api/v1/document/update",
	"POST /api/v1/document/delete",
	"POST /api/v1/agent/add",
	"POST /api/v1/agent/update",
	"POST /api/v1/agent/delete",
	"POST /api/v1/contract/add",
	"POST /api/v1/contract/update",
	"POST /api/v1/contract/delete",
	"POST /api/v1/principle/add",
	"POST /api/v1/principle/update",
	"POST /api/v1/principle/delete",
	"POST /api/v1/reviewer/add",
	"POST /api/v1/reviewer/update",
	"POST /api/v1/reviewer/delete",
	"POST /api/v1/role/add",
	"POST /api/v1/role/update",
	"POST /api/v1/role/delete",
	"POST /api/v1/skill/add",
	"POST /api/v1/skill/update",
	"POST /api/v1/skill/delete",
	// sty_004f3d3a reviewer/role/skill read wrappers (9).
	"POST /api/v1/reviewer/get",
	"POST /api/v1/reviewer/list",
	"POST /api/v1/reviewer/search",
	"POST /api/v1/role/get",
	"POST /api/v1/role/list",
	"POST /api/v1/role/search",
	"POST /api/v1/skill/get",
	"POST /api/v1/skill/list",
	"POST /api/v1/skill/search",
	// sty_0b419d98 Tier B mutates (7).
	"POST /api/v1/agent/apikey-create",
	"POST /api/v1/agent/apikey-list",
	"POST /api/v1/agent/apikey-delete",
	"POST /api/v1/agent/compose",
	"POST /api/v1/project/seed-run",
	"POST /api/v1/system/seed-run",
	"POST /api/v1/document/ingest-file",
	// sty_e68ce6fb portal_replicate (last verbStub closed).
	"POST /api/v1/portal/replicate",
}

// TestAPI_RoutesRegistered asserts the registrar attaches all 105
// verb routes to the supplied mux. Routes that respond 404 indicate
// missing registration; any other status (400/500/200) proves the
// handler was hit.
func TestAPI_RoutesRegistered(t *testing.T) {
	if got := len(expectedRoutes); got != 105 {
		t.Fatalf("expected 105 routes, got %d (update the slice as the verb set grows)", got)
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

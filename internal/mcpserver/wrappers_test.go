package mcpserver

import (
	"context"
	"github.com/bobmcallan/satellites/internal/auth"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

func mustDoc(t *testing.T, typ, scope, name, binding string) document.Document {
	t.Helper()
	d := document.Document{Type: typ, Scope: scope, Name: name}
	if binding != "" {
		d.ContractBinding = document.StringPtr(binding)
	}
	return d
}

// TestWrapper_RejectsCallerType confirms each *_create rejects an
// explicit `type` value supplied by the caller — the wrapper is
// authoritative for type.
func TestWrapper_RejectsCallerType(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	cases := []struct {
		kind string
	}{
		{"principle"},
		{"contract"},
		{"skill"},
		{"reviewer"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			handler := s.wrapperCreate(tc.kind)
			res, err := handler(ctx, newCallToolReq(tc.kind+"_create", map[string]any{
				"type":  "artifact",
				"scope": "system",
				"name":  "x",
			}))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError {
				t.Errorf("%s_create with caller type should isError; got %s", tc.kind, firstText(res))
			}
			if !strings.Contains(firstText(res), "rejects caller-supplied type") {
				t.Errorf("rejection text missing expected phrase: %s", firstText(res))
			}
		})
	}
}

// TestWrapper_PerTypePayloadValidation enumerates the four per-type
// validation rules and confirms each rejects the missing-payload case.
func TestWrapper_PerTypePayloadValidation(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	t.Run("contract_add requires structured", func(t *testing.T) {
		res, _ := s.wrapperCreate("contract")(ctx, newCallToolReq("contract_add", map[string]any{
			"scope": "system", "name": "c",
		}))
		if !res.IsError {
			t.Errorf("contract_add without structured should isError; got %s", firstText(res))
		}
	})

	t.Run("contract_add requires required keys", func(t *testing.T) {
		res, _ := s.wrapperCreate("contract")(ctx, newCallToolReq("contract_add", map[string]any{
			"scope":      "system",
			"name":       "c",
			"structured": `{"category":"plan"}`,
		}))
		if !res.IsError {
			t.Errorf("contract_add missing required keys should isError; got %s", firstText(res))
		}
	})

	t.Run("skill_add requires contract_binding", func(t *testing.T) {
		res, _ := s.wrapperCreate("skill")(ctx, newCallToolReq("skill_add", map[string]any{
			"scope": "system", "name": "s",
		}))
		if !res.IsError {
			t.Errorf("skill_add without contract_binding should isError; got %s", firstText(res))
		}
	})

	t.Run("reviewer_add requires contract_binding", func(t *testing.T) {
		res, _ := s.wrapperCreate("reviewer")(ctx, newCallToolReq("reviewer_add", map[string]any{
			"scope": "system", "name": "r",
		}))
		if !res.IsError {
			t.Errorf("reviewer_add without contract_binding should isError; got %s", firstText(res))
		}
	})

	t.Run("principle_add requires scope and tags", func(t *testing.T) {
		res, _ := s.wrapperCreate("principle")(ctx, newCallToolReq("principle_add", map[string]any{
			"scope": "system", "name": "p",
		}))
		if !res.IsError {
			t.Errorf("principle_add without tags should isError; got %s", firstText(res))
		}
	})
}

// TestWrapper_ListPinsType confirms wrapperList overrides any caller
// type with the wrapper's kind.
func TestWrapper_ListPinsType(t *testing.T) {
	t.Parallel()
	s := newDocumentTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_a", Source: "session"})

	res, _ := s.wrapperList("principle")(ctx, newCallToolReq("principle_list", map[string]any{
		"type": "contract", // try to escape
	}))
	if res.IsError {
		t.Fatalf("wrapperList should succeed with empty store: %s", firstText(res))
	}
	// The handler returns "null" for an empty store; the more important
	// assertion is that the request that reaches handleDocumentList has
	// type=principle. Confirm by re-invoking via the dispatcher with a
	// seeded contract row that should NOT appear in the principle list.
	if _, err := s.docs.Create(ctx, mustDoc(t, "contract", "system", "c1", ""), time.Now()); err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	res2, _ := s.wrapperList("principle")(ctx, newCallToolReq("principle_list", map[string]any{
		"type": "contract",
	}))
	rows := decodeArray(t, res2)
	if len(rows) != 0 {
		t.Errorf("principle_list returned a contract row: %+v", rows)
	}
}

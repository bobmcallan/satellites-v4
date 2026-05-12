package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/client"
)

// registerDocumentWrappers exposes the §9 type-specific thin wrappers
// (`principle_*`, `contract_*`, `skill_*`, `reviewer_*`, `agent_*`,
// `role_*`) on the MCP surface. After sty_4db0e025 slice B1 the
// surface is read-only: only `<kind>_get` and `<kind>_list` register,
// with two carve-outs — reviewer/role drop `_get` (redundant with
// `agent_get` per the kept-catalogue plan) and agent drops `_list`
// (agent extras are reachable via /api/v1 + CLI). Wrapper write verbs
// (`_add` / `_update` / `_delete` / `_search`) are reachable only
// through the typed methods on *client.Client via the HTTP API and
// the satellites-client CLI; the wrapperCreate / wrapperSearch
// closures and validateWrapperPayload remain because the typed
// client surface still calls into the same per-type validation /
// type-pinning helpers (handler bodies stay untouched per slice B1).
//
// `artifact` intentionally has no wrapper (per docs/architecture.md §9
// note); artefacts are exercised through the generic verbs and the
// ledger.
//
// The wrapper-kind list lives on internal/client
// (client.KnownDocumentKinds) so this transport file no longer imports
// internal/document directly — see pr_mcp_cli_shared_path.
func (s *Server) registerDocumentWrappers() {
	if s.deps.Documents == nil {
		return
	}
	for _, kind := range client.KnownDocumentKinds() {
		s.registerWrapperFamily(kind)
	}
}

// wrapperKindHasGet reports whether kind exposes `<kind>_get` on the
// MCP surface. sty_4db0e025 slice B1: reviewer + role are redundant
// with agent_get; callers should switch.
func wrapperKindHasGet(kind string) bool {
	switch kind {
	case "reviewer", "role":
		return false
	}
	return true
}

// wrapperKindHasList reports whether kind exposes `<kind>_list` on the
// MCP surface. sty_4db0e025 slice B1: agent_list is reachable only
// through the HTTP API + CLI.
func wrapperKindHasList(kind string) bool {
	return kind != "agent"
}

// registerWrapperFamily registers the kept read verbs for one kind.
// Write verbs (`_add` / `_update` / `_delete` / `_search`) were
// unregistered in sty_4db0e025 slice B1; the handler bodies remain on
// *Server (wrapperCreate / wrapperSearch / handleDocumentUpdate /
// handleDocumentDelete) so the HTTP layer + internal/client continue
// to drive them.
func (s *Server) registerWrapperFamily(kind string) {
	if wrapperKindHasGet(kind) {
		get := mcpgo.NewTool(kind+"_get",
			mcpgo.WithDescription(fmt.Sprintf("Get a %s document by id (preferred) or name.", kind)),
			mcpgo.WithString("id", mcpgo.Description("Document id.")),
			mcpgo.WithString("name", mcpgo.Description("Document name (used when id is omitted).")),
			mcpgo.WithString("project_id", mcpgo.Description("Project scope for name-keyed lookups.")),
		)
		s.mcp.AddTool(get, s.wrapperGet(kind))
	}

	if wrapperKindHasList(kind) {
		list := mcpgo.NewTool(kind+"_list",
			mcpgo.WithDescription(fmt.Sprintf("List %s documents in the caller's workspaces.", kind)),
			mcpgo.WithString("scope", mcpgo.Description("Filter by scope.")),
			mcpgo.WithString("project_id", mcpgo.Description("Filter by project.")),
			mcpgo.WithString("contract_binding", mcpgo.Description("Filter by contract_binding.")),
			mcpgo.WithArray("tags", mcpgo.Description("Filter by tags (any-of)."),
				mcpgo.Items(map[string]any{"type": "string"})),
			mcpgo.WithNumber("limit", mcpgo.Description("Max rows to return.")),
		)
		s.mcp.AddTool(list, s.wrapperList(kind))
	}
}

// wrapperCreate returns a handler that pins the type, runs per-type
// payload validation, and forwards to handleDocumentCreate.
func (s *Server) wrapperCreate(kind string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()
		if v, ok := args["type"]; ok {
			if s, _ := v.(string); s != "" && s != kind {
				return mcpgo.NewToolResultError(fmt.Sprintf("%s_add rejects caller-supplied type=%q", kind, s)), nil
			}
		}
		if err := validateWrapperPayload(kind, args); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		args["type"] = kind
		req.Params.Arguments = args
		return s.handleDocumentCreate(ctx, req)
	}
}

// wrapperGet pins the type filter to kind on document_get so a typed
// wrapper rejects a same-name row of a different type. Mirrors the
// shape of wrapperList / wrapperSearch.
func (s *Server) wrapperGet(kind string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()
		if args == nil {
			args = map[string]any{}
		}
		args["type"] = kind
		req.Params.Arguments = args
		return s.handleDocumentGet(ctx, req)
	}
}

// wrapperList pins the type filter to kind; ignores any caller-supplied
// type override.
func (s *Server) wrapperList(kind string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()
		if args == nil {
			args = map[string]any{}
		}
		args["type"] = kind
		req.Params.Arguments = args
		return s.handleDocumentList(ctx, req)
	}
}

// wrapperSearch pins the type filter to kind on document_search.
func (s *Server) wrapperSearch(kind string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()
		if args == nil {
			args = map[string]any{}
		}
		args["type"] = kind
		req.Params.Arguments = args
		return s.handleDocumentSearch(ctx, req)
	}
}

// validateWrapperPayload runs per-type structured/payload checks before
// forwarding to handleDocumentCreate. Reads from args without mutating.
// kind is the wire-level document type string (compared as literals so
// this file no longer imports internal/document — pr_mcp_cli_shared_path).
// The MCP verbs that reach this validator are the `<kind>_add` wrappers.
func validateWrapperPayload(kind string, args map[string]any) error {
	switch kind {
	case "contract":
		raw, _ := args["structured"].(string)
		if raw == "" {
			return fmt.Errorf("contract_add requires structured payload with category, required_for_close, validation_mode")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return fmt.Errorf("contract_add structured must be valid JSON: %w", err)
		}
		for _, k := range []string{"category", "required_for_close", "validation_mode"} {
			if _, ok := payload[k]; !ok {
				return fmt.Errorf("contract_add structured missing required key %q", k)
			}
		}
	case "skill", "reviewer":
		s, _ := args["contract_binding"].(string)
		if s == "" {
			return fmt.Errorf("%s_add requires contract_binding", kind)
		}
	case "principle":
		scope, _ := args["scope"].(string)
		if scope != "system" && scope != "project" {
			return fmt.Errorf("principle_add requires scope=system|project")
		}
		tags, _ := args["tags"].([]any)
		if len(tags) == 0 {
			return fmt.Errorf("principle_add requires at least one tag")
		}
	}
	return nil
}

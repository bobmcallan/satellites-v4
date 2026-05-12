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
// `role_*`) on the MCP surface. Each wrapper pins `type` to its kind,
// applies per-type payload validation on create, and forwards to the
// matching generic handleDocument* method. There is exactly one
// storage path per operation (the generic verb's handler) — wrappers
// never duplicate store calls.
//
// `artifact` intentionally has no wrapper (per docs/architecture.md §9
// note); artefacts are exercised through the generic verbs and the
// ledger.
//
// The wrapper-kind list lives on internal/client
// (client.KnownDocumentKinds) so this transport file no longer imports
// internal/document directly — see pr_mcp_cli_shared_path.
func (s *Server) registerDocumentWrappers() {
	if s.docs == nil {
		return
	}
	for _, kind := range client.KnownDocumentKinds() {
		s.registerWrapperFamily(kind)
	}
}

// registerWrapperFamily registers the six wrapper verbs for one kind.
func (s *Server) registerWrapperFamily(kind string) {
	create := mcpgo.NewTool(kind+"_create",
		mcpgo.WithDescription(fmt.Sprintf("Create a %s document. Type is pinned to %q.", kind, kind)),
		mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Description("system | project")),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Document name.")),
		mcpgo.WithString("project_id", mcpgo.Description("Required when scope=project.")),
		mcpgo.WithString("body", mcpgo.Description("Markdown body.")),
		mcpgo.WithString("structured", mcpgo.Description("Type-specific JSON payload.")),
		mcpgo.WithString("contract_binding", mcpgo.Description("Required for skill/reviewer.")),
		mcpgo.WithArray("tags", mcpgo.Description("Free-form tags."),
			mcpgo.Items(map[string]any{"type": "string"})),
		mcpgo.WithString("status", mcpgo.Description("active (default) | archived")),
	)
	s.mcp.AddTool(create, s.wrapperCreate(kind))

	get := mcpgo.NewTool(kind+"_get",
		mcpgo.WithDescription(fmt.Sprintf("Get a %s document by id (preferred) or name.", kind)),
		mcpgo.WithString("id", mcpgo.Description("Document id.")),
		mcpgo.WithString("name", mcpgo.Description("Document name (used when id is omitted).")),
		mcpgo.WithString("project_id", mcpgo.Description("Project scope for name-keyed lookups.")),
	)
	s.mcp.AddTool(get, s.wrapperGet(kind))

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

	update := mcpgo.NewTool(kind+"_update",
		mcpgo.WithDescription(fmt.Sprintf("Patch a %s document. Type is pinned; immutable fields rejected.", kind)),
		mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Document id.")),
		mcpgo.WithString("body", mcpgo.Description("Markdown body.")),
		mcpgo.WithString("structured", mcpgo.Description("Type-specific JSON payload.")),
		mcpgo.WithArray("tags", mcpgo.Description("Replace the tag set."),
			mcpgo.Items(map[string]any{"type": "string"})),
		mcpgo.WithString("status", mcpgo.Description("active | archived")),
		mcpgo.WithString("contract_binding", mcpgo.Description("Document id of an active type=contract row.")),
	)
	s.mcp.AddTool(update, s.handleDocumentUpdate)

	del := mcpgo.NewTool(kind+"_delete",
		mcpgo.WithDescription(fmt.Sprintf("Archive (default) or hard-delete a %s document.", kind)),
		mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Document id.")),
		mcpgo.WithString("mode", mcpgo.Description("archive (default) | hard")),
	)
	s.mcp.AddTool(del, s.handleDocumentDelete)

	search := mcpgo.NewTool(kind+"_search",
		mcpgo.WithDescription(fmt.Sprintf("Search %s documents in the caller's workspaces.", kind)),
		mcpgo.WithString("query", mcpgo.Description("Free-text query.")),
		mcpgo.WithString("scope", mcpgo.Description("Filter by scope.")),
		mcpgo.WithString("project_id", mcpgo.Description("Filter by project.")),
		mcpgo.WithString("contract_binding", mcpgo.Description("Filter by contract_binding.")),
		mcpgo.WithArray("tags", mcpgo.Description("Filter by tags (any-of)."),
			mcpgo.Items(map[string]any{"type": "string"})),
		mcpgo.WithNumber("top_k", mcpgo.Description("Max rows to return.")),
	)
	s.mcp.AddTool(search, s.wrapperSearch(kind))
}

// wrapperCreate returns a handler that pins the type, runs per-type
// payload validation, and forwards to handleDocumentCreate.
func (s *Server) wrapperCreate(kind string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()
		if v, ok := args["type"]; ok {
			if s, _ := v.(string); s != "" && s != kind {
				return mcpgo.NewToolResultError(fmt.Sprintf("%s_create rejects caller-supplied type=%q", kind, s)), nil
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
func validateWrapperPayload(kind string, args map[string]any) error {
	switch kind {
	case "contract":
		raw, _ := args["structured"].(string)
		if raw == "" {
			return fmt.Errorf("contract_create requires structured payload with category, required_for_close, validation_mode")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return fmt.Errorf("contract_create structured must be valid JSON: %w", err)
		}
		for _, k := range []string{"category", "required_for_close", "validation_mode"} {
			if _, ok := payload[k]; !ok {
				return fmt.Errorf("contract_create structured missing required key %q", k)
			}
		}
	case "skill", "reviewer":
		s, _ := args["contract_binding"].(string)
		if s == "" {
			return fmt.Errorf("%s_create requires contract_binding", kind)
		}
	case "principle":
		scope, _ := args["scope"].(string)
		if scope != "system" && scope != "project" {
			return fmt.Errorf("principle_create requires scope=system|project")
		}
		tags, _ := args["tags"].([]any)
		if len(tags) == 0 {
			return fmt.Errorf("principle_create requires at least one tag")
		}
	}
	return nil
}

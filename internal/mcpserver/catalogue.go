// Catalogue snapshot writer (sty_cd8b89c6). At server boot, the
// registered MCP tool list is materialised into the document store
// as a system-scope artifact tagged kind:mcp-catalogue. The portal
// /mcp page reads from that artifact via the existing SSR + doc-store
// path; there is no live tools/list proxy.
//
// Boundary: the snapshot is owned by the boot path. configseed.RunAll
// does not produce or read a kind:mcp-catalogue artifact — there is no
// markdown seed file for it — so a re-seed never overwrites or
// duplicates the snapshot. A re-boot re-derives the body and Upsert
// short-circuits when the body hash matches.
//
// Layering (sty_4db0e025 slice A7): BuildCatalogue is a pure
// MCP-SDK-only projection; MaterialiseCatalogue delegates the
// document-store write to client.Client.CatalogueUpsert so this file
// imports no substrate domain packages directly. The typed client
// surface owns the system-workspace lookup + UpsertInput shape.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/bobmcallan/satellites/internal/client"
)

const (
	CatalogueArtifactName = "mcp-catalogue"
	CatalogueKindTag      = "kind:mcp-catalogue"
)

// CatalogueParam is the per-parameter projection of a tool's input
// schema. Mirrors what tools/list emits to the model — name, type,
// required flag, description — and nothing else. Drift between this
// shape and what Claude reads is what the page protects against.
type CatalogueParam struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// CatalogueEntry is a single tool row.
type CatalogueEntry struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Parameters  []CatalogueParam `json:"parameters"`
}

// Catalogue is the body shape persisted to the artifact. SnapshotAt is
// included so operators can correlate the rendered snapshot with a
// specific server boot.
type Catalogue struct {
	SnapshotAt time.Time        `json:"snapshot_at"`
	Tools      []CatalogueEntry `json:"tools"`
}

// BuildCatalogue projects the registered tool set on the supplied
// MCPServer into a Catalogue. Tools are sorted by name for stable
// output; parameters are sorted by name and lifted out of the JSON
// Schema map into a typed slice so the page renders deterministically.
func BuildCatalogue(srv *mcpserver.MCPServer, snapshotAt time.Time) Catalogue {
	out := Catalogue{SnapshotAt: snapshotAt}
	if srv == nil {
		out.Tools = []CatalogueEntry{}
		return out
	}
	registered := srv.ListTools()
	names := make([]string, 0, len(registered))
	for name := range registered {
		names = append(names, name)
	}
	sort.Strings(names)
	out.Tools = make([]CatalogueEntry, 0, len(names))
	for _, name := range names {
		st := registered[name]
		if st == nil {
			continue
		}
		t := st.Tool
		out.Tools = append(out.Tools, CatalogueEntry{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  paramsFromSchema(t.InputSchema.Properties, t.InputSchema.Required),
		})
	}
	return out
}

// paramsFromSchema flattens the JSON Schema properties map into a
// sorted slice of CatalogueParam. Each property value in the upstream
// schema is itself a map[string]any with at least "type" and
// (optionally) "description" — keeping just those two fields enforces
// the AC's "no embellishment" rule at the projection boundary.
func paramsFromSchema(properties map[string]any, required []string) []CatalogueParam {
	if len(properties) == 0 {
		return []CatalogueParam{}
	}
	requiredSet := make(map[string]bool, len(required))
	for _, r := range required {
		requiredSet[r] = true
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]CatalogueParam, 0, len(names))
	for _, name := range names {
		schema, _ := properties[name].(map[string]any)
		p := CatalogueParam{Name: name, Required: requiredSet[name]}
		if schema != nil {
			if v, ok := schema["type"].(string); ok {
				p.Type = v
			}
			if v, ok := schema["description"].(string); ok {
				p.Description = v
			}
		}
		out = append(out, p)
	}
	return out
}

// MaterialiseCatalogue captures the registered MCP tool set into the
// document store as a system-scope artifact. Idempotent — the typed
// client surface short-circuits the Upsert when the body hash matches
// the existing row.
//
// The wire-layer responsibility here is: project the in-memory MCP
// tool registry into a Catalogue (BuildCatalogue), marshal the body,
// and hand the bytes to client.Client.CatalogueUpsert. The typed
// surface owns the workspace lookup + document-store call.
func (s *Server) MaterialiseCatalogue(ctx context.Context) error {
	if s == nil || s.docs == nil || s.mcp == nil {
		return nil
	}
	now := s.nowUTC()
	cat := BuildCatalogue(s.mcp, now)
	body, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal catalogue: %w", err)
	}
	if err := s.cli().CatalogueUpsert(ctx, client.CatalogueUpsertInput{
		Name: CatalogueArtifactName,
		Body: body,
		Tag:  CatalogueKindTag,
		Now:  now,
	}); err != nil {
		return fmt.Errorf("upsert mcp-catalogue artifact: %w", err)
	}
	if s.logger != nil {
		s.logger.Info().Int("tools", len(cat.Tools)).Str("name", CatalogueArtifactName).Msg("mcp catalogue snapshot materialised")
	}
	return nil
}

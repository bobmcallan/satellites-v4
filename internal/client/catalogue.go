// Package client — MCP catalogue snapshot writer (sty_4db0e025 slice A7).
//
// The MCP server boot path materialises the registered tool set into
// the document store as a system-scope artifact tagged
// kind:mcp-catalogue (sty_cd8b89c6). Slice A7 lifts the document-store
// write off the transport layer so internal/mcpserver/catalogue.go
// imports only the MCP-go SDK + stdlib + internal/client, satisfying
// pr_mcp_cli_shared_path.
//
// The wire-layer file keeps BuildCatalogue (pure MCP-SDK projection,
// no substrate types) and delegates the artifact write to
// CatalogueUpsert. The typed surface owns the workspace lookup, the
// UpsertInput shape, and the system-scope tags.
package client

import (
	"context"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// CatalogueUpsertInput carries the catalogue body the wire layer has
// already marshalled. Name + Tag are passed in so the caller controls
// the artifact identity (a single boot-time materialisation today, but
// the typed method does not assume that).
type CatalogueUpsertInput struct {
	// Name is the artifact's document name (e.g. "mcp-catalogue").
	Name string
	// Body is the JSON body the catalogue projection produced.
	Body []byte
	// Tag is the kind: tag stamped on the artifact (e.g.
	// "kind:mcp-catalogue").
	Tag string
	// Now overrides the timestamp written on the upsert. Zero falls back
	// to time.Now().UTC().
	Now time.Time
}

// CatalogueUpsert writes the catalogue body to the document store as a
// system-scope artifact. System-tier rows carry no workspace id per
// sty_e2512dbd (the doc store's validate path rejects a non-empty
// workspace_id when scope=system) — the pre-extraction Server code
// performed a ListByMember("system") lookup that, in practice, only
// produced a non-empty result when the system workspace had been
// minted with the same name; the lookup was dead-code for system rows.
// The typed surface drops it and stamps workspace_id="" directly.
//
// Returns nil when the Documents store is unconfigured — matches the
// pre-extraction behaviour of MaterialiseCatalogue, which silently
// no-ops on early-boot Servers whose docs field is nil.
//
// Idempotent: the underlying Upsert short-circuits when the body hash
// matches the existing row.
func (c *Client) CatalogueUpsert(ctx context.Context, in CatalogueUpsertInput) error {
	if c.deps.Documents == nil {
		return nil
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := c.deps.Documents.Upsert(ctx, document.UpsertInput{
		WorkspaceID: "",
		Type:        document.TypeArtifact,
		Name:        in.Name,
		Body:        in.Body,
		Scope:       document.ScopeSystem,
		Tags:        []string{in.Tag},
		Actor:       "system",
	}, now)
	return err
}

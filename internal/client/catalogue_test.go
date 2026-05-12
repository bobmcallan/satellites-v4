package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestCatalogueUpsert_WritesSystemArtifact pins the typed surface
// CatalogueUpsert lifted off mcpserver/catalogue.go in sty_4db0e025
// slice A7. The artifact lands scope=system / type=artifact with
// the supplied tag + body and empty workspace_id (sty_e2512dbd: the
// system tier is non-tenant).
func TestCatalogueUpsert_WritesSystemArtifact(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	docs := document.NewMemoryStore()

	c := New(Deps{Documents: docs})
	body := []byte(`{"snapshot_at":"2026-05-13T12:00:00Z","tools":[]}`)
	require.NoError(t, c.CatalogueUpsert(context.Background(), CatalogueUpsertInput{
		Name: "mcp-catalogue",
		Body: body,
		Tag:  "kind:mcp-catalogue",
		Now:  now,
	}))

	doc, err := docs.GetByName(context.Background(), "", "mcp-catalogue", nil)
	require.NoError(t, err)
	assert.Equal(t, document.TypeArtifact, doc.Type)
	assert.Equal(t, document.ScopeSystem, doc.Scope)
	assert.Equal(t, "", doc.WorkspaceID, "system rows must carry empty workspace_id")
	assert.Equal(t, string(body), string(doc.Body))
	require.Len(t, doc.Tags, 1)
	assert.Equal(t, "kind:mcp-catalogue", doc.Tags[0])
}

// TestCatalogueUpsert_Idempotent pins the idempotency contract — a
// second call with the same body short-circuits to the same row id
// without bumping version.
func TestCatalogueUpsert_Idempotent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	docs := document.NewMemoryStore()

	c := New(Deps{Documents: docs})
	in := CatalogueUpsertInput{
		Name: "mcp-catalogue",
		Body: []byte(`{"tools":[]}`),
		Tag:  "kind:mcp-catalogue",
		Now:  now,
	}
	require.NoError(t, c.CatalogueUpsert(context.Background(), in))
	first, err := docs.GetByName(context.Background(), "", "mcp-catalogue", nil)
	require.NoError(t, err)
	require.NoError(t, c.CatalogueUpsert(context.Background(), in))
	second, err := docs.GetByName(context.Background(), "", "mcp-catalogue", nil)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.Version, second.Version)
}

// TestCatalogueUpsert_NilDocumentsNoOp pins the early-boot behaviour:
// a client with no Documents store returns nil silently. Matches the
// pre-extraction MaterialiseCatalogue nil-guard on s.docs.
func TestCatalogueUpsert_NilDocumentsNoOp(t *testing.T) {
	t.Parallel()
	c := New(Deps{})
	err := c.CatalogueUpsert(context.Background(), CatalogueUpsertInput{
		Name: "mcp-catalogue",
		Body: []byte(`{}`),
		Tag:  "kind:mcp-catalogue",
	})
	assert.NoError(t, err)
}

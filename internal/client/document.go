package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/bobmcallan/satellites/internal/document"
)

// ErrDocumentStoreNotConfigured is returned when the typed document
// surface is called against a Client whose Deps.Documents is nil.
var ErrDocumentStoreNotConfigured = errors.New("document store not configured")

// DocumentGetInput names the document being read. Either ID or Name
// must be supplied; ID wins when both are present. Type pins the
// expected document type — in ID mode it's validated post-fetch
// (returns an error on mismatch); in Name mode it's threaded into the
// store's ResolveByName so the typed `_get` wrappers (agent_get,
// contract_get, principle_get) can keep their type-pinning role.
//
// WorkspaceID + ResolvedProjectID + Memberships are resolved by the
// wire-layer caller and threaded in. The typed method does NOT call
// back into transport-layer resolvers.
type DocumentGetInput struct {
	ID                string
	Name              string
	Type              string
	WorkspaceID       string
	ResolvedProjectID string
	Memberships       []string
}

// DocumentGet returns a document by id (preferred) or by hierarchical
// name resolution (project → workspace → system). ID-mode reads the
// row workspace-blind and re-applies the membership predicate only
// for non-system rows; Name-mode delegates to the store's
// ResolveByName which walks the tier ladder internally.
func (c *Client) DocumentGet(ctx context.Context, caller Caller, in DocumentGetInput) (document.Document, error) {
	if c.deps.Documents == nil {
		return document.Document{}, ErrDocumentStoreNotConfigured
	}
	if in.ID != "" {
		doc, err := c.deps.Documents.GetByID(ctx, in.ID, nil)
		if err != nil {
			return document.Document{}, err
		}
		if doc.Scope != document.ScopeSystem && !inDocMemberships(doc.WorkspaceID, in.Memberships) {
			return document.Document{}, document.ErrNotFound
		}
		if in.Type != "" && doc.Type != in.Type {
			return document.Document{}, fmt.Errorf("document_get: row %s has type=%q, not %q", in.ID, doc.Type, in.Type)
		}
		return doc, nil
	}
	if in.Name == "" {
		return document.Document{}, errors.New("either id or name is required")
	}
	return c.deps.Documents.ResolveByName(ctx, in.Type, in.Name, in.WorkspaceID, in.ResolvedProjectID, in.Memberships)
}

// DocumentListInput captures the filter knobs for document_list. The
// wire-layer resolves WorkspaceID + ResolvedProjectID + Memberships
// from the caller's context and passes them in.
type DocumentListInput struct {
	Options     document.ListOptions
	WorkspaceID string
	Memberships []string
}

// DocumentList walks the project → workspace → system tier ladder
// the store's ResolveList encapsulates and returns the union. Empty
// scope filter unions across all three tiers; an explicit scope
// pins to that tier alone.
func (c *Client) DocumentList(ctx context.Context, caller Caller, in DocumentListInput) ([]document.Document, error) {
	if c.deps.Documents == nil {
		return nil, ErrDocumentStoreNotConfigured
	}
	return c.deps.Documents.ResolveList(ctx, in.Options, in.WorkspaceID, in.Memberships)
}

// inDocMemberships mirrors mcpserver/scope_read.go:workspaceInMemberships.
// Nil memberships grants visibility (no scoping); an empty (non-nil)
// slice denies. Inlined here so the typed surface has no wire-layer
// dependency.
func inDocMemberships(wsID string, memberships []string) bool {
	if memberships == nil {
		return true
	}
	for _, m := range memberships {
		if m == wsID {
			return true
		}
	}
	return false
}

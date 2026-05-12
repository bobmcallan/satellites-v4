package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// ErrDocumentStoreNotConfigured is returned when the typed document
// surface is called against a Client whose Deps.Documents is nil.
var ErrDocumentStoreNotConfigured = errors.New("document store not configured")

// wrapperDocumentKinds enumerates the document types that the §9
// type-specific wrapper verbs (`principle_*`, `contract_*`, `skill_*`,
// `reviewer_*`, `agent_*`, `role_*`) register on the MCP surface.
// Lives in the client tier so transport-layer registration loops do
// not need to import internal/document directly
// (pr_mcp_cli_shared_path). `artifact` intentionally has no wrapper
// per docs/architecture.md §9.
var wrapperDocumentKinds = []string{
	document.TypePrinciple,
	document.TypeContract,
	document.TypeSkill,
	document.TypeReviewer,
	document.TypeAgent,
	document.TypeRole,
}

// KnownDocumentKinds returns a fresh copy of the document type strings
// that the MCP wrapper-verb registration loop fans out across. Returned
// in stable order; callers may sort/mutate the returned slice.
func KnownDocumentKinds() []string {
	out := make([]string, len(wrapperDocumentKinds))
	copy(out, wrapperDocumentKinds)
	return out
}

// ErrDocumentNoCallerIdentity is returned when document_add is
// invoked without a caller user id. Mirrors the "no caller identity"
// wire envelope previously emitted by handleDocumentAdd.
var ErrDocumentNoCallerIdentity = errors.New("no caller identity")

// ErrDocumentSystemRejectsProject is returned when document_add is
// invoked with scope=system and a non-empty project_id. Mirrors the
// "scope=system does not accept project_id" wire envelope.
var ErrDocumentSystemRejectsProject = errors.New("scope=system does not accept project_id")

// ErrDocumentStructuredInvalid is returned when document_add or
// document_update is supplied a structured payload that is not valid
// JSON. Mirrors the "structured must be valid JSON" wire envelope.
var ErrDocumentStructuredInvalid = errors.New("structured must be valid JSON")

// ErrDocumentImmutableField is returned when document_update is
// supplied an immutable key in its argument map. The wire layer
// stringifies this with the offending key.
var ErrDocumentImmutableField = errors.New("immutable field rejected")

// immutableUpdateFields are the document keys that DocumentUpdate
// must reject if the caller supplies them. Mirrors the wire-layer
// `immutableUpdateFields` slice; lives here so the typed surface
// owns the policy.
var immutableUpdateFields = []string{"workspace_id", "project_id", "type", "scope", "name"}

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

// DocumentListArgs is the flat-field equivalent of DocumentListInput.
// Lets the MCP wire layer call DocumentListByArgs without naming
// document.ListOptions in source (sty_4db0e025 slice A11).
type DocumentListArgs struct {
	Type            string
	Scope           string
	ContractBinding string
	ProjectID       string
	Tags            []string
	Limit           int
	WorkspaceID     string
	Memberships     []string
}

// DocumentListByArgs is the wire-friendly equivalent of DocumentList.
// Caps Limit at 500. Mirrors the prior wire-layer opts construction.
func (c *Client) DocumentListByArgs(ctx context.Context, caller Caller, in DocumentListArgs) ([]document.Document, error) {
	if c.deps.Documents == nil {
		return nil, ErrDocumentStoreNotConfigured
	}
	limit := in.Limit
	if limit > 500 {
		limit = 500
	}
	opts := document.ListOptions{
		Type:            in.Type,
		Scope:           in.Scope,
		ContractBinding: in.ContractBinding,
		ProjectID:       in.ProjectID,
		Tags:            in.Tags,
		Limit:           limit,
	}
	return c.deps.Documents.ResolveList(ctx, opts, in.WorkspaceID, in.Memberships)
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

// DocumentIngestFileInput captures the document_ingest_file request
// shape. Path is the docsDir-relative file to read; WorkspaceID +
// ResolvedProjectID are pre-resolved by the wire layer. Now overrides
// the timestamp for deterministic tests.
type DocumentIngestFileInput struct {
	Path              string
	WorkspaceID       string
	ResolvedProjectID string
	Now               time.Time
}

// DocumentIngestFileOutput mirrors the JSON shape the wire handler
// previously emitted.
type DocumentIngestFileOutput struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Version   int    `json:"version"`
	Changed   bool   `json:"changed"`
	Created   bool   `json:"created"`
}

// DocumentIngestFile ingests a docsDir-relative file as a
// project-scoped artifact. Wraps document.IngestFile and threads the
// Client's configured DocsDir + Logger so the wire layer no longer
// owns those globals.
func (c *Client) DocumentIngestFile(ctx context.Context, caller Caller, in DocumentIngestFileInput) (DocumentIngestFileOutput, error) {
	if c.deps.Documents == nil {
		return DocumentIngestFileOutput{}, ErrDocumentStoreNotConfigured
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := document.IngestFile(ctx, c.deps.Documents, c.deps.Logger, in.WorkspaceID, in.ResolvedProjectID, c.deps.DocsDir, in.Path, now)
	if err != nil {
		return DocumentIngestFileOutput{}, err
	}
	return DocumentIngestFileOutput{
		ID:        res.Document.ID,
		ProjectID: in.ResolvedProjectID,
		Name:      res.Document.Name,
		Version:   res.Document.Version,
		Changed:   res.Changed,
		Created:   res.Created,
	}, nil
}

// DocumentAddInput captures the document_add request shape. The
// wire layer pre-resolves WorkspaceID + ResolvedProjectID (when
// scope=project) and threads them in; the typed method owns the
// per-scope branching (scope=project sets ProjectID, scope=system
// drops WorkspaceID per sty_e2512dbd).
type DocumentAddInput struct {
	Type              string
	Scope             string
	Name              string
	Body              string
	Tags              []string
	Status            string
	ContractBinding   string
	Structured        string
	WorkspaceID       string
	ResolvedProjectID string
	Now               time.Time
}

// DocumentAdd mints a new document row in the supplied scope.
// Mirrors mcpserver.Server.handleDocumentAdd minus the wire-format
// wrap; per-scope branching + structured-JSON validation live here.
func (c *Client) DocumentAdd(ctx context.Context, caller Caller, in DocumentAddInput) (document.Document, error) {
	if c.deps.Documents == nil {
		return document.Document{}, ErrDocumentStoreNotConfigured
	}
	if caller.UserID == "" {
		return document.Document{}, ErrDocumentNoCallerIdentity
	}
	doc := document.Document{
		WorkspaceID: in.WorkspaceID,
		Type:        in.Type,
		Scope:       in.Scope,
		Name:        in.Name,
		Body:        in.Body,
		Tags:        in.Tags,
		Status:      in.Status,
		CreatedBy:   caller.UserID,
		UpdatedBy:   caller.UserID,
	}
	if doc.Status == "" {
		doc.Status = document.StatusActive
	}
	switch in.Scope {
	case document.ScopeProject:
		doc.ProjectID = document.StringPtr(in.ResolvedProjectID)
	case document.ScopeSystem:
		if in.ResolvedProjectID != "" {
			return document.Document{}, ErrDocumentSystemRejectsProject
		}
		// sty_e2512dbd: system tier is non-tenant.
		doc.WorkspaceID = ""
	}
	if in.ContractBinding != "" {
		doc.ContractBinding = document.StringPtr(in.ContractBinding)
	}
	if in.Structured != "" {
		if !json.Valid([]byte(in.Structured)) {
			return document.Document{}, ErrDocumentStructuredInvalid
		}
		doc.Structured = []byte(in.Structured)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Documents.Create(ctx, doc, now)
}

// DocumentUpdateInput captures the document_update partial-mutation
// shape. The wire layer parses pointer-vs-absent semantics from the
// inbound arg map and threads them in via Fields + RawArgs (RawArgs
// powers the immutable-field rejection without re-parsing types).
type DocumentUpdateInput struct {
	ID          string
	Fields      document.UpdateFields
	RawArgs     map[string]any
	Memberships []string
	Now         time.Time
}

// DocumentUpdateFieldsFromArgs translates a wire-layer argument map
// into a typed document.UpdateFields. Pointer semantics distinguish
// "absent" (nil) from "explicit empty" (pointer to ""), matching the
// store's mutation contract. Lives on the client so the wire layer
// (mcpserver, httpserver) no longer references document.UpdateFields
// directly (pr_mcp_cli_shared_path).
//
// tags is the resolved tag slice the wire layer would have read via
// its CallToolRequest.GetStringSlice helper; the args map carries the
// presence-bit for every other key.
func (c *Client) DocumentUpdateFieldsFromArgs(args map[string]any, tags []string) document.UpdateFields {
	fields := document.UpdateFields{}
	if v, ok := args["body"]; ok {
		sv, _ := v.(string)
		fields.Body = &sv
	}
	if v, ok := args["structured"]; ok {
		sv, _ := v.(string)
		buf := []byte(sv)
		fields.Structured = &buf
	}
	if _, ok := args["tags"]; ok {
		t := tags
		fields.Tags = &t
	}
	if v, ok := args["status"]; ok {
		sv, _ := v.(string)
		fields.Status = &sv
	}
	if v, ok := args["contract_binding"]; ok {
		sv, _ := v.(string)
		fields.ContractBinding = &sv
	}
	return fields
}

// DocumentUpdateByArgs is the wire-friendly equivalent of
// DocumentUpdate. The wire layer hands its argument map + tag slice
// to the client, which assembles the typed UpdateFields internally
// (sty_4db0e025 slice A11 — keeps document.UpdateFields out of the
// transport file).
func (c *Client) DocumentUpdateByArgs(ctx context.Context, caller Caller, id string, args map[string]any, tags []string, memberships []string, now time.Time) (document.Document, error) {
	fields := c.DocumentUpdateFieldsFromArgs(args, tags)
	return c.DocumentUpdate(ctx, caller, DocumentUpdateInput{
		ID:          id,
		Fields:      fields,
		RawArgs:     args,
		Memberships: memberships,
		Now:         now,
	})
}

// DocumentUpdate applies a partial mutation to a document. Rejects
// immutable keys, validates structured JSON, and drops memberships
// for system-tier rows (sty_e2512dbd).
func (c *Client) DocumentUpdate(ctx context.Context, caller Caller, in DocumentUpdateInput) (document.Document, error) {
	if c.deps.Documents == nil {
		return document.Document{}, ErrDocumentStoreNotConfigured
	}
	if in.ID == "" {
		return document.Document{}, errors.New("id required")
	}
	for _, k := range immutableUpdateFields {
		if _, ok := in.RawArgs[k]; ok {
			return document.Document{}, fmt.Errorf("%w: %s", ErrDocumentImmutableField, k)
		}
	}
	if in.Fields.Structured != nil && len(*in.Fields.Structured) > 0 && !json.Valid(*in.Fields.Structured) {
		return document.Document{}, ErrDocumentStructuredInvalid
	}
	memberships := in.Memberships
	// sty_e2512dbd: system tier rows have no workspace; membership-
	// scoped writes would 404 them. Read the row workspace-blind to
	// learn its scope, then drop memberships for system-scope writes.
	if existing, gerr := c.deps.Documents.GetByID(ctx, in.ID, nil); gerr == nil && existing.Scope == document.ScopeSystem {
		memberships = nil
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Documents.Update(ctx, in.ID, in.Fields, caller.UserID, now, memberships)
}

// DocumentDeleteInput captures the document_delete request shape.
// Memberships is pre-resolved by the wire layer; system-tier rows
// drop the predicate per sty_e2512dbd.
type DocumentDeleteInput struct {
	ID          string
	Mode        document.DeleteMode
	Memberships []string
}

// DocumentDeleteOutput mirrors the JSON shape the wire handler
// previously emitted ({id, mode, deleted}).
type DocumentDeleteOutput struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Deleted bool   `json:"deleted"`
}

// DocumentDeleteByArgs is the wire-friendly equivalent of
// DocumentDelete. modeArg is the raw mode string (empty => archive).
// Sty_4db0e025 slice A11 — keeps document.DeleteMode out of the
// transport file.
func (c *Client) DocumentDeleteByArgs(ctx context.Context, caller Caller, id, modeArg string, memberships []string) (DocumentDeleteOutput, error) {
	mode := document.DeleteMode(modeArg)
	if mode == "" {
		mode = document.DeleteArchive
	}
	return c.DocumentDelete(ctx, caller, DocumentDeleteInput{ID: id, Mode: mode, Memberships: memberships})
}

// DocumentDelete archives (default) or hard-deletes a document.
func (c *Client) DocumentDelete(ctx context.Context, caller Caller, in DocumentDeleteInput) (DocumentDeleteOutput, error) {
	if c.deps.Documents == nil {
		return DocumentDeleteOutput{}, ErrDocumentStoreNotConfigured
	}
	if in.ID == "" {
		return DocumentDeleteOutput{}, errors.New("id required")
	}
	mode := in.Mode
	if mode == "" {
		mode = document.DeleteArchive
	}
	memberships := in.Memberships
	if existing, gerr := c.deps.Documents.GetByID(ctx, in.ID, nil); gerr == nil && existing.Scope == document.ScopeSystem {
		memberships = nil
	}
	if err := c.deps.Documents.Delete(ctx, in.ID, mode, memberships); err != nil {
		return DocumentDeleteOutput{}, err
	}
	return DocumentDeleteOutput{ID: in.ID, Mode: string(mode), Deleted: true}, nil
}

// DocumentSearchScopedInput captures the document_search request
// shape used by the wire-layer handler — both the structured-filter
// path and the semantic-search-with-fallback path. The
// type-pinned wrappers (agent_search etc.) continue to call the
// simpler DocumentSearch helper which routes through Store.Search;
// this method preserves the handler's union semantics across the
// project → system tier ladder.
type DocumentSearchScopedInput struct {
	Type            string
	Query           string
	Scope           string
	ProjectID       string
	ContractBinding string
	Tags            []string
	TopK            int
	Memberships     []string
}

// DocumentSearchScoped mirrors handleDocumentSearch's routing:
// non-empty query → SearchSemantic with fallback to Search on
// ErrSemanticUnavailable; empty query → structured-filter Search.
// Per-scope union semantics live in the embedded helpers so the
// system tier remains visible without forcing the caller to be a
// workspace member.
func (c *Client) DocumentSearchScoped(ctx context.Context, caller Caller, in DocumentSearchScopedInput) ([]document.Document, error) {
	if c.deps.Documents == nil {
		return nil, ErrDocumentStoreNotConfigured
	}
	opts := document.SearchOptions{
		ListOptions: document.ListOptions{
			Type:            in.Type,
			Scope:           in.Scope,
			ContractBinding: in.ContractBinding,
			ProjectID:       in.ProjectID,
			Tags:            in.Tags,
		},
		Query: in.Query,
		TopK:  in.TopK,
	}
	if opts.Query != "" {
		rows, err := docSearchSemanticScoped(ctx, c.deps.Documents, opts.Query, opts, in.Memberships)
		if errors.Is(err, document.ErrSemanticUnavailable) {
			return docSearchScoped(ctx, c.deps.Documents, opts, in.Memberships)
		}
		return rows, err
	}
	return docSearchScoped(ctx, c.deps.Documents, opts, in.Memberships)
}

// docSearchScoped is the structured-filter routing used by
// DocumentSearchScoped. Migrated from mcpserver/scope_read.go
// (searchScoped); kept unexported because callers outside this
// package should use DocumentSearchScoped.
func docSearchScoped(ctx context.Context, store document.Store, opts document.SearchOptions, memberships []string) ([]document.Document, error) {
	switch opts.Scope {
	case document.ScopeSystem:
		return store.Search(ctx, opts, nil)
	case "":
		// fall through to union path
	default:
		return store.Search(ctx, opts, memberships)
	}
	sysOpts := opts
	sysOpts.Scope = document.ScopeSystem
	sysRows, err := store.Search(ctx, sysOpts, nil)
	if err != nil {
		return nil, err
	}
	memRows, err := store.Search(ctx, opts, memberships)
	if err != nil {
		return nil, err
	}
	return mergeDocsByID(sysRows, memRows), nil
}

// docSearchSemanticScoped is the semantic-search analogue.
func docSearchSemanticScoped(ctx context.Context, store document.Store, query string, opts document.SearchOptions, memberships []string) ([]document.Document, error) {
	switch opts.Scope {
	case document.ScopeSystem:
		return store.SearchSemantic(ctx, query, opts, nil)
	case "":
		// fall through to union path
	default:
		return store.SearchSemantic(ctx, query, opts, memberships)
	}
	sysOpts := opts
	sysOpts.Scope = document.ScopeSystem
	sysRows, err := store.SearchSemantic(ctx, query, sysOpts, nil)
	if err != nil {
		return nil, err
	}
	memRows, err := store.SearchSemantic(ctx, query, opts, memberships)
	if err != nil {
		return nil, err
	}
	merged := mergeDocsByID(sysRows, memRows)
	sort.SliceStable(merged, func(i, j int) bool {
		var si, sj float32
		if merged[i].BestChunkScore != nil {
			si = *merged[i].BestChunkScore
		}
		if merged[j].BestChunkScore != nil {
			sj = *merged[j].BestChunkScore
		}
		return si > sj
	})
	return merged, nil
}

// mergeDocsByID concatenates two row slices into one with no
// duplicate ids. The first slice's ordering is preserved.
func mergeDocsByID(first, second []document.Document) []document.Document {
	out := make([]document.Document, 0, len(first)+len(second))
	seen := make(map[string]struct{}, len(first)+len(second))
	for _, r := range first {
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		out = append(out, r)
	}
	for _, r := range second {
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		out = append(out, r)
	}
	return out
}

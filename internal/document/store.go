package document

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bobmcallan/satellites/internal/embeddings"
)

// ErrNotFound is returned when a document lookup misses.
var ErrNotFound = errors.New("document: not found")

// ErrImmutableField is returned when Update is asked to mutate a field
// that the schema forbids changing post-create.
var ErrImmutableField = errors.New("document: field is immutable")

// ErrDanglingBinding is returned when a write references a
// ContractBinding id that does not resolve to an active type=contract row
// inside the same workspace visibility.
var ErrDanglingBinding = errors.New("document: contract_binding does not resolve to an active type=contract document")

// UpsertResult is the outcome of Upsert. Changed==false means the body
// matched the existing hash, so version and body were left untouched.
type UpsertResult struct {
	Document Document
	Changed  bool
	Created  bool
}

// UpsertInput collects the fields Upsert needs in one struct (per
// golang-code-style: ≥4 parameters → options struct).
type UpsertInput struct {
	WorkspaceID     string
	ProjectID       *string
	Type            string
	Name            string
	Body            []byte
	Structured      []byte
	ContractBinding *string
	Scope           string
	Tags            []string
	Actor           string
}

// UpdateFields names the per-call mutable subset for Update. Nil-valued
// fields mean "leave alone"; non-nil means "set to this value". The
// caller cannot pass id, workspace_id, project_id, type, scope, or name
// — those are immutable post-create.
type UpdateFields struct {
	Body            *string
	Structured      *[]byte
	Tags            *[]string
	Status          *string
	ContractBinding *string
}

// DeleteMode discriminates Delete behaviour. DeleteArchive is the default
// and flips Status to StatusArchived; DeleteHard removes the row.
type DeleteMode string

const (
	DeleteArchive DeleteMode = "archive"
	DeleteHard    DeleteMode = "hard"
)

// ListOptions are the structured filters consumed by Store.List.
// Workspace scoping is non-negotiable and supplied via the memberships
// slice on the call itself, not through this struct.
type ListOptions struct {
	Type            string
	Scope           string
	Tags            []string
	ContractBinding string
	ProjectID       string
	Limit           int
}

// SearchOptions extend ListOptions with a free-text Query and an upper
// bound TopK on the rank. Query is matched against name + body using
// case-insensitive substring; the semantic-ranking path lands when the
// embeddings primitive ships.
type SearchOptions struct {
	ListOptions
	Query string
	TopK  int
}

// Store is the persistence surface for documents. SurrealStore is the
// production implementation; MemoryStore is the in-process test double.
//
// Workspace scoping is enforced on every read via the memberships slice
// (per docs/architecture.md §8): nil = no scoping, empty = deny-all,
// non-empty = workspace_id IN memberships.
type Store interface {
	// Upsert inserts or updates a document keyed by (project_id, name).
	// If the body hash matches the existing row, no write happens and
	// Changed=false. Used by the seed/ingest path; per-doc Create is
	// available for the explicit create surface.
	Upsert(ctx context.Context, in UpsertInput, now time.Time) (UpsertResult, error)

	// Create writes a new document. The caller supplies a fully-formed
	// Document (id may be empty — Create mints one); shape and
	// contract-binding integrity are validated before the write.
	Create(ctx context.Context, doc Document, now time.Time) (Document, error)

	// Update applies fields to the document with the given id. Immutable
	// fields cannot be set; ErrImmutableField is returned if attempted
	// at the wrapper layer.
	Update(ctx context.Context, id string, fields UpdateFields, actor string, now time.Time, memberships []string) (Document, error)

	// Delete archives or hard-deletes the document with the given id.
	// memberships scoping enforced.
	Delete(ctx context.Context, id string, mode DeleteMode, memberships []string) error

	// List returns documents matching opts. Filters compose with AND.
	List(ctx context.Context, opts ListOptions, memberships []string) ([]Document, error)

	// Search returns documents matching the structured filters. The
	// previous substring-on-Query branch (6.3 stand-in) was removed when
	// SearchSemantic landed; the query path lives on SearchSemantic now.
	// Rows ranked by updated_at DESC; capped at TopK.
	Search(ctx context.Context, opts SearchOptions, memberships []string) ([]Document, error)

	// SearchSemantic embeds query, pre-filters parents via opts (Type /
	// Scope / Tags / ContractBinding / ProjectID), runs cosine over the
	// chunk store restricted to those parents, and returns documents in
	// score order with BestChunkScore populated. Returns
	// ErrSemanticUnavailable when the store wasn't constructed with an
	// Embedder + ChunkStore.
	SearchSemantic(ctx context.Context, query string, opts SearchOptions, memberships []string) ([]Document, error)

	// GetByID returns the document with the given id, or ErrNotFound.
	GetByID(ctx context.Context, id string, memberships []string) (Document, error)

	// GetByName returns the active document with the given name inside
	// projectID. Replaces the v3 GetByFilename surface.
	GetByName(ctx context.Context, projectID, name string, memberships []string) (Document, error)

	// ResolveByName walks the tier ladder project → workspace → system
	// and returns the first active row whose name matches and (when
	// docType is non-empty) whose type matches. Empty-string keys cause
	// the corresponding tier to be skipped: projectID="" skips the
	// project tier, workspaceID="" skips the workspace tier; the system
	// tier is always considered. Membership scoping is applied to the
	// project + workspace tiers exactly as GetByName enforces it; the
	// system tier is workspace-blind (per ClearSystemTenantStamps —
	// system rows carry no tenant key). Returns ErrNotFound when no
	// tier matches.
	ResolveByName(ctx context.Context, docType, name, workspaceID, projectID string, memberships []string) (Document, error)

	// ResolveList is the list-shape sibling of ResolveByName: it walks
	// the project → workspace → system tier ladder and returns the
	// merged set with project precedence on (Name, Type) collisions.
	// opts.ProjectID keys the project tier (empty skips it); workspaceID
	// keys the workspace tier (empty skips it); the system tier is
	// always considered (workspace-blind, mirrors ResolveByName).
	// opts.Scope filters which tiers participate: ScopeProject /
	// ScopeWorkspace / ScopeSystem walk a single tier; "" walks all
	// three. The remaining ListOptions filters (Type, ContractBinding,
	// Tags) apply per-tier. Memberships scope project + workspace
	// tiers exactly as ResolveByName enforces it; an empty (deny-all)
	// memberships slice keeps only the system tier. Rows are sorted by
	// updated_at DESC after merge; opts.Limit is applied post-merge so
	// one tier can never starve another.
	ResolveList(ctx context.Context, opts ListOptions, workspaceID string, memberships []string) ([]Document, error)

	// Count returns the number of active documents in projectID. Boot
	// seeding uses this to skip work on a pre-populated project.
	Count(ctx context.Context, projectID string, memberships []string) (int, error)

	// BackfillWorkspaceID stamps workspaceID on documents with the given
	// projectID whose workspace_id is empty. Idempotent.
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error)

	// ClearSystemTenantStamps clears workspace_id and project_id on
	// every scope=system row where either field is currently set
	// (sty_e2512dbd: system tier is non-tenant, so any stamped
	// workspace or project is stale). Bypasses the Upsert body-hash
	// short-circuit that lets pre-rule rows survive intact.
	// Idempotent: a second invocation on a clean DB finds zero rows
	// to update. Returns the number of rows mutated.
	ClearSystemTenantStamps(ctx context.Context, now time.Time) (int, error)

	// ClearWorkspaceProjectStamps clears project_id on every
	// scope=workspace row where it is currently set (sty_21d4d830:
	// workspace tier is workspace-shared, so a stamped project_id
	// pins the row to one project and breaks cross-project
	// resolution). Bypasses the Upsert body-hash short-circuit that
	// would otherwise leave a stale stamp from a prior unscoped
	// BackfillProjectID pass intact. Idempotent: a clean DB finds
	// zero rows to update. Returns the number of rows mutated.
	ClearWorkspaceProjectStamps(ctx context.Context, now time.Time) (int, error)

	// ListVersions returns prior versions of the document with id
	// documentID, in DESC version order. The live document.Document
	// itself is not included in the result. Workspace scoping reuses
	// the same memberships predicate as the other read paths via
	// GetByID; an empty memberships slice denies all.
	ListVersions(ctx context.Context, documentID string, memberships []string) ([]DocumentVersion, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu       sync.Mutex
	rows     map[string]Document          // key = id
	versions map[string][]DocumentVersion // key = id; ordered DESC
	embedder embeddings.Embedder          // optional; nil disables SearchSemantic
	chunks   ChunkStore                   // optional; nil disables SearchSemantic
}

// NewMemoryStore returns an empty MemoryStore without semantic search.
// SearchSemantic returns ErrSemanticUnavailable. Use NewMemoryStoreWithEmbeddings
// to opt in.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Document), versions: make(map[string][]DocumentVersion)}
}

// NewMemoryStoreWithEmbeddings is the SearchSemantic-capable constructor.
// Either argument can be nil; both must be non-nil for SearchSemantic to
// run. Tests that exercise the semantic path pass a stub embedder + a
// fresh MemoryChunkStore.
func NewMemoryStoreWithEmbeddings(embedder embeddings.Embedder, chunks ChunkStore) *MemoryStore {
	return &MemoryStore{
		rows:     make(map[string]Document),
		versions: make(map[string][]DocumentVersion),
		embedder: embedder,
		chunks:   chunks,
	}
}

// appendVersionLocked freezes the prior state of doc into the versions
// map, prepending so the slice stays in DESC version order. Caller must
// hold m.mu. Skips when prior.Version is zero (Create has not happened
// yet).
func (m *MemoryStore) appendVersionLocked(prior Document) {
	if prior.Version == 0 {
		return
	}
	v := versionFromDocument(prior)
	m.versions[prior.ID] = append([]DocumentVersion{v}, m.versions[prior.ID]...)
}

// findByName scans for the active row matching (projectID, name). Caller
// must hold m.mu.
func (m *MemoryStore) findByName(projectID, name string) (Document, bool) {
	for _, d := range m.rows {
		if d.Name != name || d.Status != StatusActive {
			continue
		}
		if d.ProjectID == nil {
			if projectID == "" {
				return d, true
			}
			continue
		}
		if *d.ProjectID == projectID {
			return d, true
		}
	}
	return Document{}, false
}

// findForUpsert is the scope-aware existence check Upsert uses. Sty_92271886
// added the workspace tier (scope=workspace, ProjectID=nil); a same-name
// system-tier row (scope=system, ProjectID=nil) would collide under the old
// (project_id, name) lookup and silently update across the tier boundary.
// findForUpsert keys on the natural per-tier identity so an upsert at one
// scope never re-targets a row at a different scope:
//
//   - system    → (name, scope=system)
//   - workspace → (name, scope=workspace, workspace_id)
//   - project   → (name, scope=project, project_id)
//   - user      → (name, scope=user, workspace_id, created_by)
//
// Caller must hold m.mu.
func (m *MemoryStore) findForUpsert(in UpsertInput) (Document, bool) {
	for _, d := range m.rows {
		if d.Name != in.Name || d.Status != StatusActive || d.Scope != in.Scope {
			continue
		}
		switch in.Scope {
		case ScopeSystem:
			return d, true
		case ScopeWorkspace:
			if d.WorkspaceID == in.WorkspaceID {
				return d, true
			}
		case ScopeProject:
			if in.ProjectID == nil || d.ProjectID == nil {
				continue
			}
			if *d.ProjectID == *in.ProjectID {
				return d, true
			}
		case ScopeUser:
			if d.WorkspaceID == in.WorkspaceID && d.CreatedBy == in.Actor {
				return d, true
			}
		}
	}
	return Document{}, false
}

// validateBindingLocked enforces FK integrity against the in-memory rows.
// Caller must hold m.mu.
func (m *MemoryStore) validateBindingLocked(binding *string) error {
	if binding == nil || *binding == "" {
		return nil
	}
	target, ok := m.rows[*binding]
	if !ok || target.Type != TypeContract || target.Status != StatusActive {
		return ErrDanglingBinding
	}
	return nil
}

// Upsert implements Store for MemoryStore.
func (m *MemoryStore) Upsert(ctx context.Context, in UpsertInput, now time.Time) (UpsertResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hash := HashContent(in.Body, in.Structured, in.Tags, in.ContractBinding)
	candidate := Document{
		WorkspaceID:     in.WorkspaceID,
		ProjectID:       in.ProjectID,
		Type:            in.Type,
		Name:            in.Name,
		Body:            string(in.Body),
		Structured:      in.Structured,
		ContractBinding: in.ContractBinding,
		Scope:           in.Scope,
		Tags:            in.Tags,
		Status:          StatusActive,
		BodyHash:        hash,
	}
	if err := candidate.Validate(); err != nil {
		return UpsertResult{}, err
	}
	if err := m.validateBindingLocked(in.ContractBinding); err != nil {
		return UpsertResult{}, err
	}
	if existing, ok := m.findForUpsert(in); ok {
		// Body-hash match short-circuits unless the type has drifted —
		// see surreal.go for the corresponding rationale (story_7992c382).
		if existing.BodyHash == hash && existing.Type == in.Type {
			return UpsertResult{Document: existing}, nil
		}
		m.appendVersionLocked(existing)
		updated := existing
		updated.Body = string(in.Body)
		updated.BodyHash = hash
		updated.Version++
		updated.UpdatedAt = now
		updated.UpdatedBy = in.Actor
		updated.Type = in.Type
		updated.Structured = in.Structured
		updated.ContractBinding = in.ContractBinding
		updated.Tags = in.Tags
		if updated.WorkspaceID == "" {
			updated.WorkspaceID = in.WorkspaceID
		}
		m.rows[updated.ID] = updated
		return UpsertResult{Document: updated, Changed: true}, nil
	}
	doc := candidate
	doc.ID = NewID()
	doc.Version = 1
	doc.CreatedAt = now
	doc.CreatedBy = in.Actor
	doc.UpdatedAt = now
	doc.UpdatedBy = in.Actor
	m.rows[doc.ID] = doc
	return UpsertResult{Document: doc, Changed: true, Created: true}, nil
}

// Create implements Store for MemoryStore.
func (m *MemoryStore) Create(ctx context.Context, doc Document, now time.Time) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if doc.Status == "" {
		doc.Status = StatusActive
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	if err := m.validateBindingLocked(doc.ContractBinding); err != nil {
		return Document{}, err
	}
	if doc.ID == "" {
		doc.ID = NewID()
	}
	if _, exists := m.rows[doc.ID]; exists {
		return Document{}, fmt.Errorf("document: id %q already exists", doc.ID)
	}
	doc.BodyHash = HashContent([]byte(doc.Body), doc.Structured, doc.Tags, doc.ContractBinding)
	doc.Version = 1
	doc.CreatedAt = now
	doc.UpdatedAt = now
	m.rows[doc.ID] = doc
	return doc, nil
}

// Update implements Store for MemoryStore.
func (m *MemoryStore) Update(ctx context.Context, id string, fields UpdateFields, actor string, now time.Time, memberships []string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.rows[id]
	if !ok || !inDocMemberships(doc.WorkspaceID, memberships) {
		return Document{}, ErrNotFound
	}
	prior := doc
	if fields.Body != nil {
		doc.Body = *fields.Body
	}
	if fields.Structured != nil {
		doc.Structured = *fields.Structured
	}
	if fields.Tags != nil {
		doc.Tags = *fields.Tags
	}
	if fields.Status != nil {
		switch *fields.Status {
		case StatusActive, StatusArchived:
			doc.Status = *fields.Status
		default:
			return Document{}, fmt.Errorf("document: invalid status %q", *fields.Status)
		}
	}
	if fields.ContractBinding != nil {
		if err := m.validateBindingLocked(fields.ContractBinding); err != nil {
			return Document{}, err
		}
		doc.ContractBinding = fields.ContractBinding
	}
	// sty_34130d76: hash covers body + structured + tags + contract_binding;
	// any of those mutating bumps version and writes a prior-state row.
	// Status-only edits leave the content hash unchanged and so
	// deliberately do not bump version.
	newHash := HashContent([]byte(doc.Body), doc.Structured, doc.Tags, doc.ContractBinding)
	if newHash != prior.BodyHash {
		m.appendVersionLocked(prior)
		doc.BodyHash = newHash
		doc.Version = prior.Version + 1
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	doc.UpdatedAt = now
	doc.UpdatedBy = actor
	m.rows[id] = doc
	return doc, nil
}

// Delete implements Store for MemoryStore.
func (m *MemoryStore) Delete(ctx context.Context, id string, mode DeleteMode, memberships []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.rows[id]
	if !ok || !inDocMemberships(doc.WorkspaceID, memberships) {
		return ErrNotFound
	}
	switch mode {
	case DeleteHard:
		delete(m.rows, id)
	case DeleteArchive, "":
		doc.Status = StatusArchived
		m.rows[id] = doc
	default:
		return fmt.Errorf("document: invalid delete mode %q", mode)
	}
	return nil
}

// List implements Store for MemoryStore.
func (m *MemoryStore) List(ctx context.Context, opts ListOptions, memberships []string) ([]Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Document, 0, len(m.rows))
	for _, d := range m.rows {
		if !inDocMemberships(d.WorkspaceID, memberships) {
			continue
		}
		if opts.Type != "" && d.Type != opts.Type {
			continue
		}
		if opts.Scope != "" && d.Scope != opts.Scope {
			continue
		}
		if opts.ProjectID != "" {
			if d.ProjectID == nil || *d.ProjectID != opts.ProjectID {
				continue
			}
		}
		if opts.ContractBinding != "" {
			if d.ContractBinding == nil || *d.ContractBinding != opts.ContractBinding {
				continue
			}
		}
		if len(opts.Tags) > 0 && !anyTagMatch(d.Tags, opts.Tags) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Search implements Store for MemoryStore. When opts.Query is non-empty,
// applies a case-insensitive substring filter on name + body after the
// structured filter — matching the behaviour advertised by the
// document_search MCP tool description. Used as the fallback path when
// SearchSemantic returns ErrSemanticUnavailable (deployments without an
// embedder configured) so callers still get a query-narrowed result rather
// than a full unfiltered list. Capped at TopK, ordered by updated_at DESC.
func (m *MemoryStore) Search(_ context.Context, opts SearchOptions, memberships []string) ([]Document, error) {
	rows, err := m.List(context.Background(), opts.ListOptions, memberships)
	if err != nil {
		return nil, err
	}
	if q := strings.TrimSpace(opts.Query); q != "" {
		needle := strings.ToLower(q)
		filtered := rows[:0]
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Name), needle) ||
				strings.Contains(strings.ToLower(r.Body), needle) {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 20
	}
	if topK > 100 {
		topK = 100
	}
	if len(rows) > topK {
		rows = rows[:topK]
	}
	return rows, nil
}

// SearchSemantic implements Store for MemoryStore. Returns
// ErrSemanticUnavailable when no embedder + chunk store were configured.
// The order of operations is: pre-filter parents via opts → embed query
// → cosine via the chunk store → group by parent → take best score per
// parent → return parents in score order with BestChunkScore populated.
func (m *MemoryStore) SearchSemantic(ctx context.Context, query string, opts SearchOptions, memberships []string) ([]Document, error) {
	if m.embedder == nil || m.chunks == nil {
		return nil, ErrSemanticUnavailable
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return m.Search(ctx, opts, memberships)
	}
	parents, err := m.List(ctx, opts.ListOptions, memberships)
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		return nil, nil
	}
	parentIDs := make([]string, 0, len(parents))
	parentByID := make(map[string]Document, len(parents))
	for _, p := range parents {
		parentIDs = append(parentIDs, p.ID)
		parentByID[p.ID] = p
	}
	vecs, err := m.embedder.Embed(ctx, []string{q})
	if err != nil {
		return nil, fmt.Errorf("document: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	hits, err := m.chunks.SearchByEmbedding(ctx, ChunkSearchOptions{
		Embedding:           vecs[0],
		TopK:                opts.TopK * 4, // over-fetch so per-parent best can win
		RestrictDocumentIDs: parentIDs,
	}, memberships)
	if err != nil {
		return nil, err
	}
	bestPerDoc := make(map[string]float32, len(parentIDs))
	for _, h := range hits {
		if cur, ok := bestPerDoc[h.DocumentID]; !ok || h.Score > cur {
			bestPerDoc[h.DocumentID] = h.Score
		}
	}
	out := make([]Document, 0, len(bestPerDoc))
	for id, score := range bestPerDoc {
		d, ok := parentByID[id]
		if !ok {
			continue
		}
		s := score
		d.BestChunkScore = &s
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := float32(0), float32(0)
		if out[i].BestChunkScore != nil {
			si = *out[i].BestChunkScore
		}
		if out[j].BestChunkScore != nil {
			sj = *out[j].BestChunkScore
		}
		return si > sj
	})
	topK := opts.TopK
	if topK <= 0 {
		topK = 20
	}
	if topK > 100 {
		topK = 100
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func anyTagMatch(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, t := range have {
		set[t] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; ok {
			return true
		}
	}
	return false
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.rows[id]
	if !ok || !inDocMemberships(doc.WorkspaceID, memberships) {
		return Document{}, ErrNotFound
	}
	return doc, nil
}

// GetByName implements Store for MemoryStore.
func (m *MemoryStore) GetByName(ctx context.Context, projectID, name string, memberships []string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.findByName(projectID, name)
	if !ok {
		return Document{}, ErrNotFound
	}
	if !inDocMemberships(doc.WorkspaceID, memberships) {
		return Document{}, ErrNotFound
	}
	return doc, nil
}

// ResolveByName implements Store for MemoryStore. Walks
// project → workspace → system in precedence order; the first hit
// wins. Memberships scope project + workspace tiers; the system tier
// is workspace-blind per the ClearSystemTenantStamps invariant.
func (m *MemoryStore) ResolveByName(ctx context.Context, docType, name, workspaceID, projectID string, memberships []string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tiers := []func(d Document) bool{}
	if projectID != "" {
		tiers = append(tiers, func(d Document) bool {
			if d.Scope != ScopeProject || d.ProjectID == nil || *d.ProjectID != projectID {
				return false
			}
			return inDocMemberships(d.WorkspaceID, memberships)
		})
	}
	if workspaceID != "" {
		tiers = append(tiers, func(d Document) bool {
			if d.Scope != ScopeWorkspace || d.WorkspaceID != workspaceID {
				return false
			}
			return inDocMemberships(d.WorkspaceID, memberships)
		})
	}
	tiers = append(tiers, func(d Document) bool { return d.Scope == ScopeSystem })
	for _, match := range tiers {
		for _, d := range m.rows {
			if d.Status != StatusActive || d.Name != name {
				continue
			}
			if docType != "" && d.Type != docType {
				continue
			}
			if match(d) {
				return d, nil
			}
		}
	}
	return Document{}, ErrNotFound
}

// ResolveList implements Store for MemoryStore. Walks the tier ladder
// project → workspace → system in precedence order, deduping by
// (Name, Type) so a project row beats a workspace row beats a system
// row of the same name+type. Per-tier predicates apply the rest of
// ListOptions (Type, ContractBinding, Tags). The final slice is sorted
// by UpdatedAt DESC and truncated to opts.Limit post-merge.
func (m *MemoryStore) ResolveList(ctx context.Context, opts ListOptions, workspaceID string, memberships []string) ([]Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	walkProject := opts.Scope == "" || opts.Scope == ScopeProject
	walkWorkspace := opts.Scope == "" || opts.Scope == ScopeWorkspace
	walkSystem := opts.Scope == "" || opts.Scope == ScopeSystem
	tiers := make([]func(d Document) bool, 0, 3)
	if walkProject && opts.ProjectID != "" {
		projectID := opts.ProjectID
		tiers = append(tiers, func(d Document) bool {
			if d.Scope != ScopeProject || d.ProjectID == nil || *d.ProjectID != projectID {
				return false
			}
			return inDocMemberships(d.WorkspaceID, memberships)
		})
	}
	if walkWorkspace && workspaceID != "" {
		wsID := workspaceID
		tiers = append(tiers, func(d Document) bool {
			if d.Scope != ScopeWorkspace || d.WorkspaceID != wsID {
				return false
			}
			return inDocMemberships(d.WorkspaceID, memberships)
		})
	}
	if walkSystem {
		tiers = append(tiers, func(d Document) bool { return d.Scope == ScopeSystem })
	}
	seen := make(map[string]struct{})
	out := make([]Document, 0)
	for _, match := range tiers {
		for _, d := range m.rows {
			if d.Status != StatusActive {
				continue
			}
			if opts.Type != "" && d.Type != opts.Type {
				continue
			}
			if opts.ContractBinding != "" {
				if d.ContractBinding == nil || *d.ContractBinding != opts.ContractBinding {
					continue
				}
			}
			if len(opts.Tags) > 0 && !anyTagMatch(d.Tags, opts.Tags) {
				continue
			}
			if !match(d) {
				continue
			}
			key := d.Name + "|" + d.Type
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Count implements Store for MemoryStore.
func (m *MemoryStore) Count(ctx context.Context, projectID string, memberships []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, d := range m.rows {
		if d.Status != StatusActive {
			continue
		}
		if d.ProjectID == nil || *d.ProjectID != projectID {
			continue
		}
		if !inDocMemberships(d.WorkspaceID, memberships) {
			continue
		}
		n++
	}
	return n, nil
}

// inDocMemberships is the shared membership predicate. nil = no filter,
// empty = deny-all, non-empty = workspace_id IN memberships.
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

// NewID returns a fresh document id in the canonical `doc_<8hex>` form.
// Exported so the surreal store + memory store + tests mint ids
// identically.
func NewID() string {
	return fmt.Sprintf("doc_%s", uuid.NewString()[:8])
}

// ListVersions implements Store for MemoryStore.
func (m *MemoryStore) ListVersions(ctx context.Context, documentID string, memberships []string) ([]DocumentVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.rows[documentID]
	if !ok || !inDocMemberships(doc.WorkspaceID, memberships) {
		return nil, ErrNotFound
	}
	src := m.versions[documentID]
	if len(src) == 0 {
		return []DocumentVersion{}, nil
	}
	out := make([]DocumentVersion, len(src))
	copy(out, src)
	return out, nil
}

// ClearSystemTenantStamps implements Store for MemoryStore.
func (m *MemoryStore) ClearSystemTenantStamps(ctx context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, d := range m.rows {
		if d.Scope != ScopeSystem {
			continue
		}
		stale := d.WorkspaceID != "" || (d.ProjectID != nil && *d.ProjectID != "")
		if !stale {
			continue
		}
		d.WorkspaceID = ""
		d.ProjectID = nil
		d.UpdatedAt = now
		m.rows[k] = d
		n++
	}
	return n, nil
}

// ClearWorkspaceProjectStamps implements Store for MemoryStore.
func (m *MemoryStore) ClearWorkspaceProjectStamps(ctx context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, d := range m.rows {
		if d.Scope != ScopeWorkspace {
			continue
		}
		if d.ProjectID == nil || *d.ProjectID == "" {
			continue
		}
		d.ProjectID = nil
		d.UpdatedAt = now
		m.rows[k] = d
		n++
	}
	return n, nil
}

// BackfillWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, d := range m.rows {
		if d.ProjectID == nil || *d.ProjectID != projectID || d.WorkspaceID != "" {
			continue
		}
		d.WorkspaceID = workspaceID
		d.UpdatedAt = now
		m.rows[k] = d
		n++
	}
	return n, nil
}

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

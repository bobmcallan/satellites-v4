package document

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"

	"github.com/bobmcallan/satellites/internal/embeddings"
)

// sortChunkHits sorts hits by Score descending. Helper to keep the
// SurrealChunkStore impl readable.
func sortChunkHits(hits []ChunkHit) {
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
}

// SurrealStore is a SurrealDB-backed Store. It assumes the caller has
// already authenticated + selected ns/db on the supplied *surrealdb.DB.
type SurrealStore struct {
	db       *surrealdb.DB
	embedder embeddings.Embedder
	chunks   ChunkStore
}

// NewSurrealStore wraps db as a Store. Defines the `documents` and
// `document_chunks` tables schemaless so first-time SELECTs don't error
// on missing tables — v3 SurrealDB rejects SELECT from undefined tables.
//
// SearchSemantic returns ErrSemanticUnavailable until WithEmbeddings has
// installed an Embedder + ChunkStore.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS documents SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS document_chunks SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS document_versions SCHEMALESS", nil)
	return s
}

// WithEmbeddings installs the Embedder + ChunkStore so SearchSemantic
// runs the real cosine ranking instead of returning ErrSemanticUnavailable.
// Returns the same store for chaining at boot.
func (s *SurrealStore) WithEmbeddings(embedder embeddings.Embedder, chunks ChunkStore) *SurrealStore {
	s.embedder = embedder
	s.chunks = chunks
	return s
}

// NewSurrealChunkStore returns a SurrealDB-backed ChunkStore writing to
// the `document_chunks` table.
func NewSurrealChunkStore(db *surrealdb.DB) *SurrealChunkStore {
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS document_chunks SCHEMALESS", nil)
	return &SurrealChunkStore{db: db}
}

// SurrealChunkStore is the SurrealDB-backed ChunkStore.
type SurrealChunkStore struct {
	db *surrealdb.DB
}

// chunkSelectCols pins the column projection for the chunks table.
const chunkSelectCols = "meta::id(id) AS id, document_id, workspace_id, chunk_idx, body, embedding, embedding_model, created_at"

// Upsert implements ChunkStore for SurrealChunkStore.
func (c *SurrealChunkStore) Upsert(ctx context.Context, documentID string, chunks []Chunk) error {
	// Replace-the-set semantics: drop existing rows for this document
	// then write the new batch.
	if err := c.DeleteByDocumentID(ctx, documentID); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	for _, ch := range chunks {
		if ch.ID == "" {
			ch.ID = NewID()
		}
		if ch.DocumentID == "" {
			ch.DocumentID = documentID
		}
		sql := "UPSERT $rid CONTENT $row"
		vars := map[string]any{
			"rid": surrealmodels.NewRecordID("document_chunks", ch.ID),
			"row": ch,
		}
		if _, err := surrealdb.Query[any](ctx, c.db, sql, vars); err != nil {
			return fmt.Errorf("document_chunks: upsert: %w", err)
		}
	}
	return nil
}

// DeleteByDocumentID implements ChunkStore for SurrealChunkStore.
func (c *SurrealChunkStore) DeleteByDocumentID(ctx context.Context, documentID string) error {
	sql := "DELETE FROM document_chunks WHERE document_id = $doc"
	vars := map[string]any{"doc": documentID}
	if _, err := surrealdb.Query[any](ctx, c.db, sql, vars); err != nil {
		return fmt.Errorf("document_chunks: delete: %w", err)
	}
	return nil
}

// SearchByEmbedding implements ChunkStore for SurrealChunkStore. Brute-
// force cosine over workspace-visible rows after structured-filter
// pre-application. Linear scan; swap to a vector index when chunk count
// crosses ~10k (limitation documented in docs/architecture.md §2).
func (c *SurrealChunkStore) SearchByEmbedding(ctx context.Context, opts ChunkSearchOptions, memberships []string) ([]ChunkHit, error) {
	if len(opts.Embedding) == 0 {
		return nil, fmt.Errorf("document_chunks: SearchByEmbedding requires an Embedding")
	}
	conds := []string{}
	vars := map[string]any{}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	if len(opts.RestrictDocumentIDs) > 0 {
		conds = append(conds, "document_id IN $docs")
		vars["docs"] = opts.RestrictDocumentIDs
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	sql := fmt.Sprintf("SELECT %s FROM document_chunks%s", chunkSelectCols, where)
	results, err := surrealdb.Query[[]Chunk](ctx, c.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("document_chunks: search: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	rows := (*results)[0].Result
	hits := make([]ChunkHit, 0, len(rows))
	for _, r := range rows {
		score, err := embeddings.Cosine(opts.Embedding, r.Embedding)
		if err != nil {
			continue
		}
		hits = append(hits, ChunkHit{Chunk: r, Score: score})
	}
	sortChunkHits(hits)
	topK := opts.TopK
	if topK <= 0 {
		topK = defaultChunkTopK
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// selectCols preserves the string form of id. SurrealDB otherwise returns
// id as a RecordID object which JSON-unmarshals as empty into `ID string`.
const selectCols = "meta::id(id) AS id, workspace_id, project_id, type, name, body, structured, contract_binding, scope, tags, status, body_hash, version, created_at, created_by, updated_at, updated_by"

// Upsert implements Store for SurrealStore.
func (s *SurrealStore) Upsert(ctx context.Context, in UpsertInput, now time.Time) (UpsertResult, error) {
	hash := HashBody(in.Body)
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
	if err := s.validateBinding(ctx, in.ContractBinding); err != nil {
		return UpsertResult{}, err
	}
	projectID := ""
	if in.ProjectID != nil {
		projectID = *in.ProjectID
	}
	existing, err := s.GetByName(ctx, projectID, in.Name, nil)
	if err == nil {
		if existing.BodyHash == hash {
			return UpsertResult{Document: existing}, nil
		}
		if verr := s.writeVersion(ctx, versionFromDocument(existing)); verr != nil {
			return UpsertResult{}, verr
		}
		updated := existing
		updated.Body = string(in.Body)
		updated.BodyHash = hash
		updated.Version = existing.Version + 1
		updated.UpdatedAt = now
		updated.UpdatedBy = in.Actor
		updated.Type = in.Type
		updated.Structured = in.Structured
		updated.ContractBinding = in.ContractBinding
		updated.Tags = in.Tags
		if updated.WorkspaceID == "" {
			updated.WorkspaceID = in.WorkspaceID
		}
		if err := s.write(ctx, updated); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Document: updated, Changed: true}, nil
	}
	doc := candidate
	doc.ID = NewID()
	doc.Version = 1
	doc.CreatedAt = now
	doc.CreatedBy = in.Actor
	doc.UpdatedAt = now
	doc.UpdatedBy = in.Actor
	if err := s.write(ctx, doc); err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Document: doc, Changed: true, Created: true}, nil
}

// Create implements Store for SurrealStore.
func (s *SurrealStore) Create(ctx context.Context, doc Document, now time.Time) (Document, error) {
	if doc.Status == "" {
		doc.Status = StatusActive
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	if err := s.validateBinding(ctx, doc.ContractBinding); err != nil {
		return Document{}, err
	}
	if doc.ID == "" {
		doc.ID = NewID()
	}
	doc.BodyHash = HashBody([]byte(doc.Body))
	doc.Version = 1
	doc.CreatedAt = now
	doc.UpdatedAt = now
	if err := s.write(ctx, doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Update implements Store for SurrealStore.
func (s *SurrealStore) Update(ctx context.Context, id string, fields UpdateFields, actor string, now time.Time, memberships []string) (Document, error) {
	doc, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return Document{}, err
	}
	if fields.Body != nil {
		newHash := HashBody([]byte(*fields.Body))
		if newHash != doc.BodyHash {
			if verr := s.writeVersion(ctx, versionFromDocument(doc)); verr != nil {
				return Document{}, verr
			}
			doc.Body = *fields.Body
			doc.BodyHash = newHash
			doc.Version++
		}
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
		if err := s.validateBinding(ctx, fields.ContractBinding); err != nil {
			return Document{}, err
		}
		doc.ContractBinding = fields.ContractBinding
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	doc.UpdatedAt = now
	doc.UpdatedBy = actor
	if err := s.write(ctx, doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Delete implements Store for SurrealStore.
func (s *SurrealStore) Delete(ctx context.Context, id string, mode DeleteMode, memberships []string) error {
	doc, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return err
	}
	switch mode {
	case DeleteHard:
		_, err := surrealdb.Query[any](ctx, s.db, "DELETE $rid", map[string]any{
			"rid": surrealmodels.NewRecordID("documents", doc.ID),
		})
		if err != nil {
			return fmt.Errorf("document: hard delete: %w", err)
		}
		return nil
	case DeleteArchive, "":
		doc.Status = StatusArchived
		doc.UpdatedAt = time.Now().UTC()
		return s.write(ctx, doc)
	default:
		return fmt.Errorf("document: invalid delete mode %q", mode)
	}
}

// List implements Store for SurrealStore.
func (s *SurrealStore) List(ctx context.Context, opts ListOptions, memberships []string) ([]Document, error) {
	if memberships != nil && len(memberships) == 0 {
		return nil, nil
	}
	conds := []string{}
	vars := map[string]any{}
	if opts.Type != "" {
		if _, ok := validTypes[opts.Type]; !ok {
			return nil, fmt.Errorf("document: invalid type filter %q", opts.Type)
		}
		conds = append(conds, "type = $type")
		vars["type"] = opts.Type
	}
	if opts.Scope != "" {
		if _, ok := validScopes[opts.Scope]; !ok {
			return nil, fmt.Errorf("document: invalid scope filter %q", opts.Scope)
		}
		conds = append(conds, "scope = $scope")
		vars["scope"] = opts.Scope
	}
	if opts.ProjectID != "" {
		conds = append(conds, "project_id = $project")
		vars["project"] = opts.ProjectID
	}
	if opts.ContractBinding != "" {
		conds = append(conds, "contract_binding = $binding")
		vars["binding"] = opts.ContractBinding
	}
	if len(opts.Tags) > 0 {
		conds = append(conds, "tags ANYINSIDE $tags")
		vars["tags"] = opts.Tags
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	limit := ""
	if opts.Limit > 0 {
		limit = fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	sql := fmt.Sprintf("SELECT %s FROM documents%s ORDER BY updated_at DESC%s", selectCols, where, limit)
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("document: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	return (*results)[0].Result, nil
}

// Search implements Store for SurrealStore.
func (s *SurrealStore) Search(ctx context.Context, opts SearchOptions, memberships []string) ([]Document, error) {
	if memberships != nil && len(memberships) == 0 {
		return nil, nil
	}
	conds := []string{}
	vars := map[string]any{}
	if opts.Type != "" {
		if _, ok := validTypes[opts.Type]; !ok {
			return nil, fmt.Errorf("document: invalid type filter %q", opts.Type)
		}
		conds = append(conds, "type = $type")
		vars["type"] = opts.Type
	}
	if opts.Scope != "" {
		if _, ok := validScopes[opts.Scope]; !ok {
			return nil, fmt.Errorf("document: invalid scope filter %q", opts.Scope)
		}
		conds = append(conds, "scope = $scope")
		vars["scope"] = opts.Scope
	}
	if opts.ProjectID != "" {
		conds = append(conds, "project_id = $project")
		vars["project"] = opts.ProjectID
	}
	if opts.ContractBinding != "" {
		conds = append(conds, "contract_binding = $binding")
		vars["binding"] = opts.ContractBinding
	}
	if len(opts.Tags) > 0 {
		conds = append(conds, "tags ANYINSIDE $tags")
		vars["tags"] = opts.Tags
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	// The substring-on-Query branch (slice 6.3 stand-in) was removed when
	// the semantic path landed (story_5abfe61c). SearchSemantic is the
	// query path now.
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 20
	}
	if topK > 100 {
		topK = 100
	}
	sql := fmt.Sprintf("SELECT %s FROM documents%s ORDER BY updated_at DESC LIMIT %d", selectCols, where, topK)
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("document: search: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	return (*results)[0].Result, nil
}

// SearchSemantic implements Store for SurrealStore. Returns
// ErrSemanticUnavailable when WithEmbeddings hasn't been called. Otherwise
// delegates to the same algorithm as MemoryStore: pre-filter parents via
// List → embed query → cosine via the chunk store → group hits by parent
// → return parents in score order with BestChunkScore populated.
func (s *SurrealStore) SearchSemantic(ctx context.Context, query string, opts SearchOptions, memberships []string) ([]Document, error) {
	if s.embedder == nil || s.chunks == nil {
		return nil, ErrSemanticUnavailable
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return s.Search(ctx, opts, memberships)
	}
	parents, err := s.List(ctx, opts.ListOptions, memberships)
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
	vecs, err := s.embedder.Embed(ctx, []string{q})
	if err != nil {
		return nil, fmt.Errorf("document: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	hits, err := s.chunks.SearchByEmbedding(ctx, ChunkSearchOptions{
		Embedding:           vecs[0],
		TopK:                opts.TopK * 4,
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
		ss := score
		d.BestChunkScore = &ss
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

// validateBinding rejects a non-nil binding that does not resolve to an
// active type=contract row. nil binding is a no-op.
func (s *SurrealStore) validateBinding(ctx context.Context, binding *string) error {
	if binding == nil || *binding == "" {
		return nil
	}
	target, err := s.GetByID(ctx, *binding, nil)
	if err != nil {
		return ErrDanglingBinding
	}
	if target.Type != TypeContract || target.Status != StatusActive {
		return ErrDanglingBinding
	}
	return nil
}

// write upserts doc by record id.
func (s *SurrealStore) write(ctx context.Context, doc Document) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("documents", doc.ID),
		"doc": doc,
	}
	if _, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("document: write: %w", err)
	}
	return nil
}

// writeVersion upserts a frozen DocumentVersion. Idempotent: the row id
// is `<documentID>_v<version>`, so re-writing the same version replaces
// in place rather than duplicating.
func (s *SurrealStore) writeVersion(ctx context.Context, v DocumentVersion) error {
	if v.Version == 0 || v.DocumentID == "" {
		return nil
	}
	rowID := fmt.Sprintf("%s_v%d", v.DocumentID, v.Version)
	sql := "UPSERT $rid CONTENT $v"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("document_versions", rowID),
		"v":   v,
	}
	if _, err := surrealdb.Query[[]DocumentVersion](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("document: write version: %w", err)
	}
	return nil
}

// ListVersions implements Store for SurrealStore.
func (s *SurrealStore) ListVersions(ctx context.Context, documentID string, memberships []string) ([]DocumentVersion, error) {
	if memberships != nil && len(memberships) == 0 {
		return nil, ErrNotFound
	}
	if _, err := s.GetByID(ctx, documentID, memberships); err != nil {
		return nil, err
	}
	sql := "SELECT document_id, version, body_hash, body, structured, updated_at, updated_by FROM document_versions WHERE document_id = $id ORDER BY version DESC"
	vars := map[string]any{"id": documentID}
	results, err := surrealdb.Query[[]DocumentVersion](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("document: list versions: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []DocumentVersion{}, nil
	}
	rows := (*results)[0].Result
	if rows == nil {
		return []DocumentVersion{}, nil
	}
	return rows, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string, memberships []string) (Document, error) {
	if memberships != nil && len(memberships) == 0 {
		return Document{}, ErrNotFound
	}
	conds := []string{"meta::id(id) = $id"}
	vars := map[string]any{"id": id}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM documents WHERE %s LIMIT 1", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return Document{}, fmt.Errorf("document: select by id: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Document{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// GetByName implements Store for SurrealStore.
func (s *SurrealStore) GetByName(ctx context.Context, projectID, name string, memberships []string) (Document, error) {
	if memberships != nil && len(memberships) == 0 {
		return Document{}, ErrNotFound
	}
	conds := []string{"name = $name", "status = 'active'"}
	vars := map[string]any{"name": name}
	if projectID != "" {
		conds = append(conds, "project_id = $project")
		vars["project"] = projectID
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM documents WHERE %s LIMIT 1", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return Document{}, fmt.Errorf("document: select by name: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return Document{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// Count implements Store for SurrealStore.
func (s *SurrealStore) Count(ctx context.Context, projectID string, memberships []string) (int, error) {
	if memberships != nil && len(memberships) == 0 {
		return 0, nil
	}
	conds := []string{"project_id = $project", "status = 'active'"}
	vars := map[string]any{"project": projectID}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT count() AS n FROM documents WHERE %s GROUP ALL", strings.Join(conds, " AND "))
	type row struct {
		N int `json:"n"`
	}
	results, err := surrealdb.Query[[]row](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("document: count: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return 0, nil
	}
	return (*results)[0].Result[0].N, nil
}

// BackfillProjectID stamps rows that lack a project_id with defaultID.
// One-pass idempotent migration for documents seeded before the project
// primitive existed. Second boot is a no-op because the WHERE clause
// filters out already-stamped rows.
func (s *SurrealStore) BackfillProjectID(ctx context.Context, defaultID string) (int, error) {
	sql := "UPDATE documents SET project_id = $project WHERE project_id IS NONE OR project_id = '' RETURN AFTER"
	vars := map[string]any{"project": defaultID}
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("document: backfill project_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// BackfillWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	sql := "UPDATE documents SET workspace_id = $ws, updated_at = $now WHERE project_id = $project AND (workspace_id IS NONE OR workspace_id = '') RETURN AFTER"
	vars := map[string]any{"ws": workspaceID, "project": projectID, "now": now}
	results, err := surrealdb.Query[[]Document](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("document: backfill workspace_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// MigrateLegacyRows stamps the v4 fields (type, scope, name) on legacy
// rows that pre-date the schema-discriminator refactor (story_509f1111).
// Once every row has a non-empty name, the legacy `filename` field is
// dropped from the schema. Idempotent: a second invocation finds zero
// rows to stamp and the field-drop is a no-op.
func (s *SurrealStore) MigrateLegacyRows(ctx context.Context, now time.Time) (int, error) {
	stamps := []struct {
		label string
		sql   string
		vars  map[string]any
	}{
		{
			label: "type=artifact",
			sql:   "UPDATE documents SET type = 'artifact', updated_at = $now WHERE type IS NONE OR type = '' OR type NOT IN ['artifact','contract','skill','principle','reviewer'] RETURN AFTER",
			vars:  map[string]any{"now": now},
		},
		{
			label: "scope=project",
			sql:   "UPDATE documents SET scope = 'project', updated_at = $now WHERE scope IS NONE OR scope = '' RETURN AFTER",
			vars:  map[string]any{"now": now},
		},
		{
			label: "name=filename",
			sql:   "UPDATE documents SET name = filename, updated_at = $now WHERE (name IS NONE OR name = '') AND filename IS NOT NONE AND filename != '' RETURN AFTER",
			vars:  map[string]any{"now": now},
		},
	}
	stamped := 0
	for _, q := range stamps {
		results, err := surrealdb.Query[[]Document](ctx, s.db, q.sql, q.vars)
		if err != nil {
			return stamped, fmt.Errorf("document: migrate %s: %w", q.label, err)
		}
		if results != nil && len(*results) > 0 {
			stamped += len((*results)[0].Result)
		}
	}
	type cnt struct {
		N int `json:"n"`
	}
	countSQL := "SELECT count() AS n FROM documents WHERE filename IS NOT NONE AND filename != '' GROUP ALL"
	cres, err := surrealdb.Query[[]cnt](ctx, s.db, countSQL, nil)
	if err != nil {
		return stamped, nil
	}
	remaining := 0
	if cres != nil && len(*cres) > 0 && len((*cres)[0].Result) > 0 {
		remaining = (*cres)[0].Result[0].N
	}
	if remaining == 0 {
		_, _ = surrealdb.Query[any](ctx, s.db, "REMOVE FIELD filename ON documents", nil)
	}
	return stamped, nil
}

// Compile-time assertion.
var _ Store = (*SurrealStore)(nil)

package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"

	"github.com/bobmcallan/satellites/internal/embeddings"
)

// SurrealStore is a SurrealDB-backed Store. The caller must have already
// authenticated and selected ns/db on the supplied *surrealdb.DB.
type SurrealStore struct {
	db        *surrealdb.DB
	embedder  embeddings.Embedder
	chunks    ChunkStore
	listenMu  sync.Mutex
	listeners []Listener
}

// WithEmbeddings installs the Embedder + ChunkStore so SearchSemantic
// runs the real cosine ranking instead of returning ErrSemanticUnavailable.
func (s *SurrealStore) WithEmbeddings(embedder embeddings.Embedder, chunks ChunkStore) *SurrealStore {
	s.embedder = embedder
	s.chunks = chunks
	return s
}

// AddListener registers l on the listener slice (sty_e805a01a).
func (s *SurrealStore) AddListener(l Listener) {
	if l == nil {
		return
	}
	s.listenMu.Lock()
	s.listeners = append(s.listeners, l)
	s.listenMu.Unlock()
}

// NewSurrealStore wraps db as a Store. Defines the `ledger` table
// schemaless so first-time SELECTs don't error on a missing table; also
// declares the §6 access indexes — idempotent under DEFINE INDEX IF NOT
// EXISTS.
func NewSurrealStore(db *surrealdb.DB) *SurrealStore {
	s := &SurrealStore{db: db}
	ctx := context.Background()
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS ledger SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS ledger_ws_story_created ON ledger FIELDS workspace_id, story_id, created_at", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE INDEX IF NOT EXISTS ledger_ws_tags ON ledger FIELDS workspace_id, tags", nil)
	_, _ = surrealdb.Query[any](ctx, db, "DEFINE TABLE IF NOT EXISTS ledger_chunks SCHEMALESS", nil)
	// sty_509a46fa: contract_id was the FK to the deleted contract_instance
	// row type. Drop the legacy index and clear lingering values from old
	// rows; new ledger writes carry task_id / phase tags instead.
	_, _ = surrealdb.Query[any](ctx, db, "REMOVE INDEX IF EXISTS ledger_ws_contract ON TABLE ledger", nil)
	_, _ = surrealdb.Query[any](ctx, db, "UPDATE ledger UNSET contract_id", nil)
	return s
}

// NewSurrealChunkStore returns a SurrealDB-backed ChunkStore writing to
// the `ledger_chunks` table.
func NewSurrealChunkStore(db *surrealdb.DB) *SurrealChunkStore {
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS ledger_chunks SCHEMALESS", nil)
	return &SurrealChunkStore{db: db}
}

// SurrealChunkStore is the SurrealDB-backed ChunkStore for ledger rows.
type SurrealChunkStore struct {
	db *surrealdb.DB
}

const chunkSelectCols = "meta::id(id) AS id, ledger_id, workspace_id, chunk_idx, body, embedding, embedding_model, created_at"

// Upsert implements ChunkStore for SurrealChunkStore.
func (c *SurrealChunkStore) Upsert(ctx context.Context, ledgerID string, chunks []Chunk) error {
	if err := c.DeleteByLedgerID(ctx, ledgerID); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	for _, ch := range chunks {
		if ch.ID == "" {
			ch.ID = NewID()
		}
		if ch.LedgerID == "" {
			ch.LedgerID = ledgerID
		}
		sql := "UPSERT $rid CONTENT $row"
		vars := map[string]any{
			"rid": surrealmodels.NewRecordID("ledger_chunks", ch.ID),
			"row": ch,
		}
		if _, err := surrealdb.Query[any](ctx, c.db, sql, vars); err != nil {
			return fmt.Errorf("ledger_chunks: upsert: %w", err)
		}
	}
	return nil
}

// DeleteByLedgerID implements ChunkStore for SurrealChunkStore.
func (c *SurrealChunkStore) DeleteByLedgerID(ctx context.Context, ledgerID string) error {
	sql := "DELETE FROM ledger_chunks WHERE ledger_id = $lid"
	vars := map[string]any{"lid": ledgerID}
	if _, err := surrealdb.Query[any](ctx, c.db, sql, vars); err != nil {
		return fmt.Errorf("ledger_chunks: delete: %w", err)
	}
	return nil
}

// SearchByEmbedding implements ChunkStore for SurrealChunkStore. Brute-
// force cosine over rows matching memberships + RestrictLedgerIDs.
func (c *SurrealChunkStore) SearchByEmbedding(ctx context.Context, opts ChunkSearchOptions, memberships []string) ([]ChunkHit, error) {
	if len(opts.Embedding) == 0 {
		return nil, fmt.Errorf("ledger_chunks: SearchByEmbedding requires an Embedding")
	}
	conds := []string{}
	vars := map[string]any{}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	if len(opts.RestrictLedgerIDs) > 0 {
		conds = append(conds, "ledger_id IN $ids")
		vars["ids"] = opts.RestrictLedgerIDs
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger_chunks%s", chunkSelectCols, where)
	results, err := surrealdb.Query[[]Chunk](ctx, c.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger_chunks: search: %w", err)
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
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	topK := opts.TopK
	if topK <= 0 {
		topK = defaultChunkTopK
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// selectCols preserves the string form of id (see internal/project/surreal.go).
const selectCols = "meta::id(id) AS id, workspace_id, project_id, story_id, type, tags, content, structured, durability, expires_at, source_type, sensitive, status, created_at, created_by, impersonating_as_workspace"

// Append implements Store for SurrealStore.
func (s *SurrealStore) Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error) {
	applyDefaults(&entry)
	stampImpersonationFromCtx(ctx, &entry)
	if err := entry.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	entry.ID = NewID()
	entry.CreatedAt = now
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("ledger", entry.ID),
		"doc": entry,
	}
	if _, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars); err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: append: %w", err)
	}
	s.listenMu.Lock()
	listeners := append([]Listener(nil), s.listeners...)
	s.listenMu.Unlock()
	fanoutListeners(ctx, listeners, entry)
	return entry, nil
}

// List implements Store for SurrealStore. Newest-first, limit clamped.
// Default behaviour excludes dereferenced rows; callers opt in via
// ListOptions.Status='dereferenced' or ListOptions.IncludeDerefd=true.
func (s *SurrealStore) List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error) {
	opts = opts.normalised()
	if memberships != nil && len(memberships) == 0 {
		return []LedgerEntry{}, nil
	}
	conds, vars := s.buildListWhere(projectID, opts, memberships)
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at DESC LIMIT $lim", selectCols, strings.Join(conds, " AND "))
	vars["lim"] = opts.Limit
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return []LedgerEntry{}, nil
	}
	return (*results)[0].Result, nil
}

// buildListWhere translates ListOptions into a SurrealDB WHERE clause +
// vars map. Workspace memberships and the dereferenced-default-exclude
// rules live here so List + Search share one source of truth.
func (s *SurrealStore) buildListWhere(projectID string, opts ListOptions, memberships []string) ([]string, map[string]any) {
	conds := []string{}
	vars := map[string]any{}
	if projectID != "" {
		conds = append(conds, "project_id = $project")
		vars["project"] = projectID
	}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	if opts.Type != "" {
		conds = append(conds, "type = $type")
		vars["type"] = opts.Type
	}
	if opts.StoryID != "" {
		conds = append(conds, "story_id = $story")
		vars["story"] = opts.StoryID
	}
	if len(opts.Tags) > 0 {
		conds = append(conds, "tags ANYINSIDE $tags")
		vars["tags"] = opts.Tags
	}
	if opts.Durability != "" {
		conds = append(conds, "durability = $durability")
		vars["durability"] = opts.Durability
	}
	if opts.SourceType != "" {
		conds = append(conds, "source_type = $source_type")
		vars["source_type"] = opts.SourceType
	}
	if opts.Sensitive != nil {
		conds = append(conds, "sensitive = $sensitive")
		vars["sensitive"] = *opts.Sensitive
	}
	if opts.Status != "" {
		conds = append(conds, "status = $status")
		vars["status"] = opts.Status
	} else if !opts.IncludeDerefd {
		conds = append(conds, "(status IS NONE OR status != 'dereferenced')")
	}
	return conds, vars
}

// Search implements Store for SurrealStore.
func (s *SurrealStore) Search(ctx context.Context, projectID string, opts SearchOptions, memberships []string) ([]LedgerEntry, error) {
	if memberships != nil && len(memberships) == 0 {
		return nil, nil
	}
	listOpts := opts.normalised()
	conds, vars := s.buildListWhere(projectID, listOpts, memberships)
	// The substring-on-Query branch (slice 7.2 stand-in) was removed when
	// the semantic path landed (story_5abfe61c). SearchSemantic is the
	// query path now.
	topK := opts.TopK
	if topK <= 0 {
		topK = 20
	}
	if topK > 100 {
		topK = 100
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at DESC LIMIT %d", selectCols, strings.Join(conds, " AND "), topK)
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: search: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	return (*results)[0].Result, nil
}

// SearchSemantic implements Store for SurrealStore. Returns
// ErrSemanticUnavailable when WithEmbeddings hasn't been called. Otherwise
// runs the same algorithm as MemoryStore.
func (s *SurrealStore) SearchSemantic(ctx context.Context, projectID, query string, opts SearchOptions, memberships []string) ([]LedgerEntry, error) {
	if s.embedder == nil || s.chunks == nil {
		return nil, ErrSemanticUnavailable
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return s.Search(ctx, projectID, opts, memberships)
	}
	parents, err := s.List(ctx, projectID, opts.ListOptions, memberships)
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		return nil, nil
	}
	parentIDs := make([]string, 0, len(parents))
	parentByID := make(map[string]LedgerEntry, len(parents))
	for _, p := range parents {
		if p.Status == StatusDereferenced {
			continue
		}
		parentIDs = append(parentIDs, p.ID)
		parentByID[p.ID] = p
	}
	if len(parentIDs) == 0 {
		return nil, nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{q})
	if err != nil {
		return nil, fmt.Errorf("ledger: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	hits, err := s.chunks.SearchByEmbedding(ctx, ChunkSearchOptions{
		Embedding:         vecs[0],
		TopK:              opts.TopK * 4,
		RestrictLedgerIDs: parentIDs,
	}, memberships)
	if err != nil {
		return nil, err
	}
	bestPerRow := make(map[string]float32, len(parentIDs))
	for _, h := range hits {
		if cur, ok := bestPerRow[h.LedgerID]; !ok || h.Score > cur {
			bestPerRow[h.LedgerID] = h.Score
		}
	}
	out := make([]LedgerEntry, 0, len(bestPerRow))
	for id, score := range bestPerRow {
		row, ok := parentByID[id]
		if !ok {
			continue
		}
		ss := score
		row.BestChunkScore = &ss
		out = append(out, row)
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

// Recall implements Store for SurrealStore. Returns the chain of rows
// tagged recall_root:<rootID> plus the root row, ordered by CreatedAt
// ASC.
func (s *SurrealStore) Recall(ctx context.Context, rootID string, memberships []string) ([]LedgerEntry, error) {
	if rootID == "" {
		return nil, errors.New("ledger: recall requires root id")
	}
	if memberships != nil && len(memberships) == 0 {
		return nil, nil
	}
	conds := []string{"(meta::id(id) = $root OR tags CONTAINS $tag)"}
	vars := map[string]any{"root": rootID, "tag": "recall_root:" + rootID}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s ORDER BY created_at ASC", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("ledger: recall: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	return (*results)[0].Result, nil
}

// GetByID implements Store for SurrealStore.
func (s *SurrealStore) GetByID(ctx context.Context, id string, memberships []string) (LedgerEntry, error) {
	if memberships != nil && len(memberships) == 0 {
		return LedgerEntry{}, ErrNotFound
	}
	conds := []string{"meta::id(id) = $id"}
	vars := map[string]any{"id": id}
	if memberships != nil {
		conds = append(conds, "workspace_id IN $memberships")
		vars["memberships"] = memberships
	}
	sql := fmt.Sprintf("SELECT %s FROM ledger WHERE %s LIMIT 1", selectCols, strings.Join(conds, " AND "))
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: get: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return LedgerEntry{}, ErrNotFound
	}
	return (*results)[0].Result[0], nil
}

// Dereference implements Store for SurrealStore.
func (s *SurrealStore) Dereference(ctx context.Context, id, reason, actor string, now time.Time, memberships []string) (LedgerEntry, error) {
	target, err := s.GetByID(ctx, id, memberships)
	if err != nil {
		return LedgerEntry{}, err
	}
	auditEntry := LedgerEntry{
		WorkspaceID: target.WorkspaceID,
		ProjectID:   target.ProjectID,
		StoryID:     target.StoryID,
		Type:        TypeDecision,
		Tags:        []string{"kind:dereference", "target:" + id},
		Content:     reason,
		CreatedBy:   actor,
	}
	written, err := s.Append(ctx, auditEntry, now)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: write audit row: %w", err)
	}
	updateSQL := "UPDATE $rid SET status = 'dereferenced'"
	updateVars := map[string]any{"rid": surrealmodels.NewRecordID("ledger", id)}
	if _, err := surrealdb.Query[any](ctx, s.db, updateSQL, updateVars); err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: dereference target: %w", err)
	}
	// Cascade chunks (story_5abfe61c). Best-effort — failure to drop
	// chunks is logged at the caller layer; SearchSemantic also filters
	// dereferenced parents as defence-in-depth.
	if s.chunks != nil {
		_ = s.chunks.DeleteByLedgerID(ctx, id)
	}
	return written, nil
}

// BackfillWorkspaceID implements Store for SurrealStore.
func (s *SurrealStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error) {
	sql := "UPDATE ledger SET workspace_id = $ws WHERE project_id = $project AND (workspace_id IS NONE OR workspace_id = '') RETURN AFTER"
	vars := map[string]any{"ws": workspaceID, "project": projectID}
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("ledger: backfill workspace_id: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// DeleteByProjectID implements Store for SurrealStore. Sty_d357b28d.
func (s *SurrealStore) DeleteByProjectID(ctx context.Context, projectID string) (int, error) {
	type idRow struct {
		ID string `json:"id"`
	}
	idSQL := "SELECT meta::id(id) AS id FROM ledger WHERE project_id = $project"
	idResults, err := surrealdb.Query[[]idRow](ctx, s.db, idSQL, map[string]any{"project": projectID})
	if err != nil {
		return 0, fmt.Errorf("ledger: list ids by project: %w", err)
	}
	ids := []string{}
	if idResults != nil && len(*idResults) > 0 {
		for _, r := range (*idResults)[0].Result {
			ids = append(ids, r.ID)
		}
	}
	if _, err := surrealdb.Query[any](ctx, s.db, "DELETE ledger WHERE project_id = $project", map[string]any{"project": projectID}); err != nil {
		return 0, fmt.Errorf("ledger: delete by project: %w", err)
	}
	if s.chunks != nil {
		for _, id := range ids {
			_ = s.chunks.DeleteByLedgerID(ctx, id)
		}
	}
	return len(ids), nil
}

// SetWorkspaceIDByProjectID implements Store for SurrealStore. Sty_d357b28d.
func (s *SurrealStore) SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string) (int, error) {
	sql := "UPDATE ledger SET workspace_id = $ws WHERE project_id = $project AND workspace_id != $ws RETURN AFTER"
	vars := map[string]any{"ws": newWorkspaceID, "project": projectID}
	results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, sql, vars)
	if err != nil {
		return 0, fmt.Errorf("ledger: set workspace_id by project: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return 0, nil
	}
	return len((*results)[0].Result), nil
}

// MigrateLegacyRows stamps the v4 enum + naming on rows that pre-date the
// schema reshape (story_368cd70f). Idempotent on every boot. Once every
// row has a non-empty `created_by`, the legacy `actor` field is dropped.
func (s *SurrealStore) MigrateLegacyRows(ctx context.Context, now time.Time) (int, error) {
	stamps := []struct {
		label string
		sql   string
	}{
		{"created_by=actor", "UPDATE ledger SET created_by = actor WHERE (created_by IS NONE OR created_by = '') AND actor IS NOT NONE RETURN AFTER"},
		{"durability=durable", "UPDATE ledger SET durability = 'durable' WHERE durability IS NONE OR durability = '' RETURN AFTER"},
		{"source_type=agent", "UPDATE ledger SET source_type = 'agent' WHERE source_type IS NONE OR source_type = '' RETURN AFTER"},
		{"status=active", "UPDATE ledger SET status = 'active' WHERE status IS NONE OR status = '' RETURN AFTER"},
	}
	stamped := 0
	for _, q := range stamps {
		results, err := surrealdb.Query[[]LedgerEntry](ctx, s.db, q.sql, nil)
		if err != nil {
			return stamped, fmt.Errorf("ledger: migrate %s: %w", q.label, err)
		}
		if results != nil && len(*results) > 0 {
			stamped += len((*results)[0].Result)
		}
	}
	type cnt struct {
		N int `json:"n"`
	}
	countSQL := "SELECT count() AS n FROM ledger WHERE actor IS NOT NONE AND actor != '' GROUP ALL"
	cres, err := surrealdb.Query[[]cnt](ctx, s.db, countSQL, nil)
	if err != nil {
		return stamped, nil
	}
	remaining := 0
	if cres != nil && len(*cres) > 0 && len((*cres)[0].Result) > 0 {
		remaining = (*cres)[0].Result[0].N
	}
	if remaining == 0 {
		_, _ = surrealdb.Query[any](ctx, s.db, "REMOVE FIELD actor ON ledger", nil)
	}
	return stamped, nil
}

// Compile-time assertion that SurrealStore satisfies Store.
var _ Store = (*SurrealStore)(nil)

package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bobmcallan/satellites/internal/embeddings"
)

// DefaultListLimit is applied when ListOptions.Limit <= 0.
const DefaultListLimit = 100

// MaxListLimit is the ceiling — higher values clamp to this.
const MaxListLimit = 500

// ListOptions filters a List call. All non-zero fields compose with
// AND. Limit<=0 uses DefaultListLimit; Limit>MaxListLimit clamps to
// MaxListLimit. Status defaults to active+archived (excluding
// dereferenced); supply Status explicitly to opt in.
type ListOptions struct {
	Type          string
	StoryID       string
	Tags          []string
	Durability    string
	SourceType    string
	Status        string
	Sensitive     *bool
	IncludeDerefd bool
	Limit         int
}

// SearchOptions extend ListOptions with a free-text Query and an upper
// bound TopK on the rank. Query is matched against content +
// structured-as-string using case-insensitive substring; the
// semantic-ranking path lands when the embeddings primitive ships
// (mirrors documents 6.3 stand-in).
type SearchOptions struct {
	ListOptions
	Query string
	TopK  int
}

// normalised returns opts with Limit clamped into [1, MaxListLimit].
func (o ListOptions) normalised() ListOptions {
	if o.Limit <= 0 {
		o.Limit = DefaultListLimit
	}
	if o.Limit > MaxListLimit {
		o.Limit = MaxListLimit
	}
	return o
}

// ErrNotFound is returned when a ledger lookup misses.
var ErrNotFound = errors.New("ledger: not found")

// Store is the persistence surface for the ledger. The interface is
// append-only by intent — `Append` is the sole creation path, and
// `Dereference` is the sole status-mutation path (per `pr_root_cause`,
// rows are never deleted; dereference flips Status='dereferenced' so the
// row stays in the audit chain but vanishes from default queries).
//
// BackfillWorkspaceID is the feature-order:2 migration exception and
// only stamps workspace_id on rows where it was empty.
type Store interface {
	Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error)
	GetByID(ctx context.Context, id string, memberships []string) (LedgerEntry, error)
	List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error)
	Search(ctx context.Context, projectID string, opts SearchOptions, memberships []string) ([]LedgerEntry, error)
	// SearchSemantic embeds query, pre-filters via opts, runs cosine over
	// the chunk store restricted to those parents, and returns rows in
	// score order with BestChunkScore populated. Returns
	// ErrSemanticUnavailable when the store wasn't constructed with an
	// Embedder + ChunkStore. Dereferenced rows are excluded from results.
	SearchSemantic(ctx context.Context, projectID, query string, opts SearchOptions, memberships []string) ([]LedgerEntry, error)
	Recall(ctx context.Context, rootID string, memberships []string) ([]LedgerEntry, error)
	Dereference(ctx context.Context, id, reason, actor string, now time.Time, memberships []string) (LedgerEntry, error)
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error)

	// DeleteByProjectID hard-removes every ledger row whose ProjectID
	// matches. Returns the count removed. Memberships scoping is NOT
	// applied; the caller (project_delete hard-purge cascade) is
	// responsible for authorising the destructive op. Append-only is
	// the substrate's default posture; this method is the explicit
	// operator-driven escape valve. Sty_d357b28d.
	DeleteByProjectID(ctx context.Context, projectID string) (int, error)

	// SetWorkspaceIDByProjectID stamps newWorkspaceID on every ledger
	// row whose ProjectID matches. Returns the count touched. Used by
	// the project_move_workspace cascade. Sty_d357b28d.
	SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string) (int, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu        sync.Mutex
	rows      []LedgerEntry
	embedder  embeddings.Embedder
	chunks    ChunkStore
	listeners []Listener
}

// NewMemoryStore returns an empty MemoryStore without semantic search.
// SearchSemantic returns ErrSemanticUnavailable. Use
// NewMemoryStoreWithEmbeddings to opt in.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make([]LedgerEntry, 0)}
}

// NewMemoryStoreWithEmbeddings is the SearchSemantic-capable constructor.
// Either argument can be nil; both must be non-nil for SearchSemantic to
// run. Tests that exercise the semantic path pass a stub embedder + a
// fresh MemoryChunkStore.
func NewMemoryStoreWithEmbeddings(embedder embeddings.Embedder, chunks ChunkStore) *MemoryStore {
	return &MemoryStore{
		rows:     make([]LedgerEntry, 0),
		embedder: embedder,
		chunks:   chunks,
	}
}

// AddListener registers l on the bus-subscriber slice. Listeners fire
// inline at Append time; panics are recovered per listener so a buggy
// subscriber cannot abort the writer. Cross-workspace consumers
// (e.g. the storystatus reconciler) attach here at boot time
// (sty_e805a01a). Post-cutover (sty_010a0543) this is the only fan-out
// path — surreallive picks up the row mutation for WS subscribers.
func (m *MemoryStore) AddListener(l Listener) {
	if l == nil {
		return
	}
	m.mu.Lock()
	m.listeners = append(m.listeners, l)
	m.mu.Unlock()
}

// Append implements Store for MemoryStore.
func (m *MemoryStore) Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error) {
	applyDefaults(&entry)
	stampImpersonationFromCtx(ctx, &entry)
	if err := entry.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	m.mu.Lock()
	entry.ID = NewID()
	entry.CreatedAt = now
	m.rows = append(m.rows, entry)
	listeners := append([]Listener(nil), m.listeners...)
	m.mu.Unlock()
	fanoutListeners(ctx, listeners, entry)
	return entry, nil
}

// applyDefaults stamps the v4 enum fields that callers may leave empty.
// The default shape (Durability=durable, SourceType=agent, Status=active)
// matches the most common write — agents emitting durable evidence.
func applyDefaults(entry *LedgerEntry) {
	if entry.Durability == "" {
		entry.Durability = DurabilityDurable
	}
	if entry.SourceType == "" {
		entry.SourceType = SourceAgent
	}
	if entry.Status == "" {
		entry.Status = StatusActive
	}
}

// matches reports whether e satisfies opts. Used by both List and
// Search. memberships is checked separately by the caller.
func matches(e LedgerEntry, opts ListOptions) bool {
	if opts.Type != "" && e.Type != opts.Type {
		return false
	}
	if opts.StoryID != "" {
		if e.StoryID == nil || *e.StoryID != opts.StoryID {
			return false
		}
	}
	if len(opts.Tags) > 0 && !anyTagMatch(e.Tags, opts.Tags) {
		return false
	}
	if opts.Durability != "" && e.Durability != opts.Durability {
		return false
	}
	if opts.SourceType != "" && e.SourceType != opts.SourceType {
		return false
	}
	if opts.Sensitive != nil && e.Sensitive != *opts.Sensitive {
		return false
	}
	if opts.Status != "" {
		if e.Status != opts.Status {
			return false
		}
	} else if !opts.IncludeDerefd && e.Status == StatusDereferenced {
		return false
	}
	return true
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

// List implements Store for MemoryStore.
func (m *MemoryStore) List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]LedgerEntry, error) {
	opts = opts.normalised()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LedgerEntry, 0)
	for _, e := range m.rows {
		if projectID != "" && e.ProjectID != projectID {
			continue
		}
		if !inLedgerMemberships(e.WorkspaceID, memberships) {
			continue
		}
		if !matches(e, opts) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Search implements Store for MemoryStore. The previous substring-on-Query
// branch (slice 7.2 stand-in) was removed when SearchSemantic landed
// (story_5abfe61c) per pr_no_unrequested_compat. Search is now a
// structured-filter list capped at TopK; the query path lives on
// SearchSemantic.
func (m *MemoryStore) Search(ctx context.Context, projectID string, opts SearchOptions, memberships []string) ([]LedgerEntry, error) {
	rows, err := m.List(ctx, projectID, opts.ListOptions, memberships)
	if err != nil {
		return nil, err
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
// Dereferenced rows are filtered out — both as a list pre-filter and by
// the chunk store's natural absence of chunks for those rows (Dereference
// cascades to DeleteByLedgerID).
func (m *MemoryStore) SearchSemantic(ctx context.Context, projectID, query string, opts SearchOptions, memberships []string) ([]LedgerEntry, error) {
	if m.embedder == nil || m.chunks == nil {
		return nil, ErrSemanticUnavailable
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return m.Search(ctx, projectID, opts, memberships)
	}
	parents, err := m.List(ctx, projectID, opts.ListOptions, memberships)
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
	vecs, err := m.embedder.Embed(ctx, []string{q})
	if err != nil {
		return nil, fmt.Errorf("ledger: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	hits, err := m.chunks.SearchByEmbedding(ctx, ChunkSearchOptions{
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
		s := score
		row.BestChunkScore = &s
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

// Recall returns the chain of ledger rows that share the recall_root tag
// pointing at rootID, ordered by CreatedAt ASC. Used by contract claim /
// resume to load prior evidence.
func (m *MemoryStore) Recall(ctx context.Context, rootID string, memberships []string) ([]LedgerEntry, error) {
	if rootID == "" {
		return nil, errors.New("ledger: recall requires root id")
	}
	tag := "recall_root:" + rootID
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LedgerEntry, 0)
	for _, e := range m.rows {
		if !inLedgerMemberships(e.WorkspaceID, memberships) {
			continue
		}
		if e.ID == rootID {
			out = append(out, e)
			continue
		}
		for _, t := range e.Tags {
			if t == tag {
				out = append(out, e)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// GetByID implements Store for MemoryStore.
func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.rows {
		if e.ID != id {
			continue
		}
		if !inLedgerMemberships(e.WorkspaceID, memberships) {
			return LedgerEntry{}, ErrNotFound
		}
		return e, nil
	}
	return LedgerEntry{}, ErrNotFound
}

// Dereference flips the target row's Status to dereferenced and writes a
// new audit row of Type=decision tagged kind:dereference. Both writes are
// returned: the new audit entry is returned to the caller; the target's
// status mutation is the only schema-permitted write besides Append.
func (m *MemoryStore) Dereference(ctx context.Context, id, reason, actor string, now time.Time, memberships []string) (LedgerEntry, error) {
	target, err := m.GetByID(ctx, id, memberships)
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
	written, err := m.Append(ctx, auditEntry, now)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: write audit row: %w", err)
	}
	m.mu.Lock()
	for i, e := range m.rows {
		if e.ID == id {
			e.Status = StatusDereferenced
			m.rows[i] = e
			break
		}
	}
	chunks := m.chunks
	m.mu.Unlock()
	// Cascade: drop the dereferenced row's chunks so it vanishes from
	// SearchSemantic results. Best-effort. The SearchSemantic
	// parent-status filter is the defence-in-depth: even if the
	// cascade fails, dereferenced rows are excluded from results.
	if chunks != nil {
		_ = chunks.DeleteByLedgerID(ctx, id)
	}
	return written, nil
}

// inLedgerMemberships is the shared membership predicate for ledger rows.
// nil = no filter, empty = deny-all, non-empty = workspace_id IN memberships.
func inLedgerMemberships(wsID string, memberships []string) bool {
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

// BackfillWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i, e := range m.rows {
		if e.ProjectID != projectID || e.WorkspaceID != "" {
			continue
		}
		e.WorkspaceID = workspaceID
		m.rows[i] = e
		n++
	}
	return n, nil
}

// DeleteByProjectID implements Store for MemoryStore. Sty_d357b28d.
func (m *MemoryStore) DeleteByProjectID(ctx context.Context, projectID string) (int, error) {
	m.mu.Lock()
	keep := make([]LedgerEntry, 0, len(m.rows))
	removed := make([]string, 0)
	for _, e := range m.rows {
		if e.ProjectID == projectID {
			removed = append(removed, e.ID)
			continue
		}
		keep = append(keep, e)
	}
	m.rows = keep
	chunks := m.chunks
	m.mu.Unlock()
	if chunks != nil {
		for _, id := range removed {
			_ = chunks.DeleteByLedgerID(ctx, id)
		}
	}
	return len(removed), nil
}

// SetWorkspaceIDByProjectID implements Store for MemoryStore. Sty_d357b28d.
func (m *MemoryStore) SetWorkspaceIDByProjectID(ctx context.Context, projectID, newWorkspaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i, e := range m.rows {
		if e.ProjectID != projectID {
			continue
		}
		if e.WorkspaceID == newWorkspaceID {
			continue
		}
		e.WorkspaceID = newWorkspaceID
		m.rows[i] = e
		n++
	}
	return n, nil
}

// Compile-time assertion that MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
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
	Type            string
	StoryID         string
	ContractID      string
	Tags            []string
	Durability      string
	SourceType      string
	Status          string
	Sensitive       *bool
	IncludeDerefd   bool
	Limit           int
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
	Recall(ctx context.Context, rootID string, memberships []string) ([]LedgerEntry, error)
	Dereference(ctx context.Context, id, reason, actor string, now time.Time, memberships []string) (LedgerEntry, error)
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string) (int, error)
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows []LedgerEntry
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make([]LedgerEntry, 0)}
}

// Append implements Store for MemoryStore.
func (m *MemoryStore) Append(ctx context.Context, entry LedgerEntry, now time.Time) (LedgerEntry, error) {
	applyDefaults(&entry)
	if err := entry.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.ID = NewID()
	entry.CreatedAt = now
	m.rows = append(m.rows, entry)
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
	if opts.ContractID != "" {
		if e.ContractID == nil || *e.ContractID != opts.ContractID {
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

// Search implements Store for MemoryStore.
func (m *MemoryStore) Search(ctx context.Context, projectID string, opts SearchOptions, memberships []string) ([]LedgerEntry, error) {
	rows, err := m.List(ctx, projectID, opts.ListOptions, memberships)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(opts.Query))
	if q != "" {
		filtered := rows[:0]
		for _, e := range rows {
			if strings.Contains(strings.ToLower(e.Content), q) ||
				strings.Contains(strings.ToLower(string(e.Structured)), q) {
				filtered = append(filtered, e)
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
		ContractID:  target.ContractID,
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
	m.mu.Unlock()
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

// Compile-time assertion that MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

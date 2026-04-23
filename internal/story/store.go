package story

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// LedgerEntryType is the canonical event-type for story status changes.
const LedgerEntryType = "story.status_change"

// ErrNotFound is returned when a story lookup misses.
var ErrNotFound = errors.New("story: not found")

// ListOptions filters a List call.
type ListOptions struct {
	Status   string
	Priority string
	Tag      string
	Limit    int
}

const (
	defaultListLimit = 100
	maxListLimit     = 500
)

func (o ListOptions) normalised() ListOptions {
	if o.Limit <= 0 {
		o.Limit = defaultListLimit
	}
	if o.Limit > maxListLimit {
		o.Limit = maxListLimit
	}
	return o
}

// Store is the persistence surface for stories. UpdateStatus is the only
// mutation verb beyond Create — other field updates are out of scope at v4
// baseline per the story's AC 3.
type Store interface {
	Create(ctx context.Context, s Story, now time.Time) (Story, error)
	// GetByID / List / UpdateStatus all take a memberships slice: nil = no
	// scoping, empty = deny-all, non-empty = workspace_id IN memberships.
	// See docs/architecture.md §8.
	GetByID(ctx context.Context, id string, memberships []string) (Story, error)
	List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]Story, error)
	UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time, memberships []string) (Story, error)

	// BackfillWorkspaceID stamps workspaceID on every row with ProjectID ==
	// projectID whose workspace_id is empty. Returns the number of rows
	// touched. Boot-time backfill for feature-order:2.
	BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error)
}

// transitionPayload is the JSON shape written into the ledger `content`
// column on every status change. Kept as an exported struct so callers can
// unmarshal it for UI/audit surfaces.
type transitionPayload struct {
	StoryID string `json:"story_id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Actor   string `json:"actor"`
}

// MemoryStore is a concurrency-safe in-process Store used by unit tests.
// It emits a ledger row on every successful UpdateStatus; if the ledger
// append fails, the in-memory status change is reverted.
type MemoryStore struct {
	mu     sync.Mutex
	rows   map[string]Story
	ledger ledger.Store
}

// NewMemoryStore returns an empty MemoryStore backed by the supplied
// ledger.Store. A nil ledger is rejected — status transitions MUST emit
// evidence per pr_20440c77.
func NewMemoryStore(led ledger.Store) *MemoryStore {
	if led == nil {
		panic("story.MemoryStore requires a non-nil ledger.Store")
	}
	return &MemoryStore{rows: make(map[string]Story), ledger: led}
}

func (m *MemoryStore) Create(ctx context.Context, s Story, now time.Time) (Story, error) {
	if s.Status == "" {
		s.Status = StatusBacklog
	}
	if !IsKnownStatus(s.Status) {
		return Story{}, fmt.Errorf("story: unknown status %q", s.Status)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.ID = NewID()
	s.CreatedAt = now
	s.UpdatedAt = now
	if s.Tags == nil {
		s.Tags = []string{}
	}
	m.rows[s.ID] = s
	return s, nil
}

func (m *MemoryStore) GetByID(ctx context.Context, id string, memberships []string) (Story, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[id]
	if !ok {
		return Story{}, ErrNotFound
	}
	if !inStoryMemberships(s.WorkspaceID, memberships) {
		return Story{}, ErrNotFound
	}
	return s, nil
}

func (m *MemoryStore) List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]Story, error) {
	opts = opts.normalised()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Story, 0)
	for _, s := range m.rows {
		if s.ProjectID != projectID {
			continue
		}
		if !inStoryMemberships(s.WorkspaceID, memberships) {
			continue
		}
		if opts.Status != "" && s.Status != opts.Status {
			continue
		}
		if opts.Priority != "" && s.Priority != opts.Priority {
			continue
		}
		if opts.Tag != "" && !containsTag(s.Tags, opts.Tag) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// inStoryMemberships is the shared membership predicate for story rows.
// nil = no filter, empty = deny-all, non-empty = workspace_id IN memberships.
func inStoryMemberships(wsID string, memberships []string) bool {
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

func (m *MemoryStore) UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time, memberships []string) (Story, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[id]
	if !ok {
		return Story{}, ErrNotFound
	}
	if !inStoryMemberships(s.WorkspaceID, memberships) {
		return Story{}, ErrNotFound
	}
	if !ValidTransition(s.Status, newStatus) {
		return Story{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, s.Status, newStatus)
	}
	prior := s.Status
	s.Status = newStatus
	s.UpdatedAt = now
	m.rows[id] = s

	payload := transitionPayload{
		StoryID: id,
		From:    prior,
		To:      newStatus,
		Actor:   actor,
	}
	content, _ := json.Marshal(payload)
	if _, err := m.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: s.WorkspaceID,
		ProjectID:   s.ProjectID,
		Type:        LedgerEntryType,
		Content:     string(content),
		Actor:       actor,
	}, now); err != nil {
		// Revert the in-memory status change — the ledger emission is
		// load-bearing (pr_20440c77). Caller sees the original error.
		s.Status = prior
		m.rows[id] = s
		return Story{}, fmt.Errorf("story: ledger emission failed (status reverted): %w", err)
	}
	return s, nil
}

// BackfillWorkspaceID implements Store for MemoryStore.
func (m *MemoryStore) BackfillWorkspaceID(ctx context.Context, projectID, workspaceID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.rows {
		if s.ProjectID != projectID || s.WorkspaceID != "" {
			continue
		}
		s.WorkspaceID = workspaceID
		s.UpdatedAt = now
		m.rows[id] = s
		n++
	}
	return n, nil
}

func containsTag(tags []string, target string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, target) {
			return true
		}
	}
	return false
}

// Compile-time assertion.
var _ Store = (*MemoryStore)(nil)

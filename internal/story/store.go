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

	"github.com/bobmcallan/satellites/internal/hubemit"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// LedgerEntryType is the canonical event-type for story status changes.
const LedgerEntryType = "story.status_change"

// derivedActor is recorded as the actor on UpdateStatusDerived calls
// from the storystatus reconciler so the audit trail explicitly
// carries the derivation provenance. Sty_dc121948.
const derivedActor = "system:reconciler"

// ErrNotFound is returned when a story lookup misses.
var ErrNotFound = errors.New("story: not found")

// ErrStoryHasOpenTasks is returned by UpdateStatus when a transition
// to done|cancelled is rejected because the story's task chain has
// open work (tasks at status=published or status=planned). Wrapped by
// *StoryHasOpenTasksError, which carries the open task ids so handlers
// can include them in their response. Sty_0233fabd.
var ErrStoryHasOpenTasks = errors.New("story: open tasks remain on chain")

// StoryHasOpenTasksError is the typed concrete error returned when the
// terminal-transition gate fires. errors.Is(err, ErrStoryHasOpenTasks)
// is true; callers extract the ids via errors.As to *StoryHasOpenTasksError.
type StoryHasOpenTasksError struct {
	StoryID     string
	OpenTaskIDs []string
}

func (e *StoryHasOpenTasksError) Error() string {
	return fmt.Sprintf("%s: %s has %d open task(s)", ErrStoryHasOpenTasks.Error(), e.StoryID, len(e.OpenTaskIDs))
}

// Unwrap supports errors.Is(err, ErrStoryHasOpenTasks).
func (e *StoryHasOpenTasksError) Unwrap() error { return ErrStoryHasOpenTasks }

// OpenTasksFunc returns the ids of tasks on storyID at
// status=published or status=planned. The Store calls this before
// permitting a transition to done|cancelled. When the func is nil
// (test wiring or pre-boot state), the gate is skipped and transitions
// proceed as before. Sty_0233fabd.
type OpenTasksFunc func(ctx context.Context, storyID string, memberships []string) ([]string, error)

// UpdateFields names the per-call mutable subset for Update. Nil-valued
// pointers mean "leave alone"; non-nil means "set to this value". The
// Tags slice is wholesale-replace: a non-nil empty slice clears the tag
// list, matching V3's "Tags/labels (replaces existing tags)" semantic.
// Widened in sty_330cc4ab to expose the fields the MCP tool advertises.
type UpdateFields struct {
	Title              *string
	Description        *string
	AcceptanceCriteria *string
	Category           *string
	Priority           *string
	Tags               *[]string
}

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

// Store is the persistence surface for stories. UpdateStatus is the
// status-only mutation verb; Update applies the narrow `UpdateFields`
// subset (story_4ca6cb1b: configuration_id). Other field updates remain
// out of scope until a future story explicitly requests them per
// pr_no_unrequested_compat.
type Store interface {
	Create(ctx context.Context, s Story, now time.Time) (Story, error)
	// GetByID / List / UpdateStatus / Update all take a memberships
	// slice: nil = no scoping, empty = deny-all, non-empty = workspace_id
	// IN memberships. See docs/architecture.md §8.
	GetByID(ctx context.Context, id string, memberships []string) (Story, error)
	List(ctx context.Context, projectID string, opts ListOptions, memberships []string) ([]Story, error)
	UpdateStatus(ctx context.Context, id, newStatus, actor string, now time.Time, memberships []string) (Story, error)

	// UpdateStatusDerived bypasses the forward-only transition guard
	// for derivation-driven flips (sty_dc121948). The substrate's
	// derived-status reconciler (internal/storystatus) calls this
	// path; manual operator updates continue to use UpdateStatus and
	// stay guarded against illegal jumps. The actor is recorded as
	// "system:reconciler" so the audit trail explicitly carries the
	// derivation provenance — no synthetic user impersonation.
	UpdateStatusDerived(ctx context.Context, id, newStatus string, now time.Time, memberships []string) (Story, error)

	// Update applies fields to the story with the given id. Fields whose
	// pointer is nil are left untouched. Status, immutable identity
	// fields, and timestamps are not eligible — UpdateStatus owns status;
	// identity is set at Create.
	Update(ctx context.Context, id string, fields UpdateFields, actor string, now time.Time, memberships []string) (Story, error)

	// SetField writes a single template-defined value onto the story's
	// Fields map. Empty value clears the key. Returns ErrNotFound when
	// the story doesn't exist or workspace scoping rejects it.
	// Sty_d2a03cea.
	SetField(ctx context.Context, id, field string, value any, actor string, now time.Time, memberships []string) (Story, error)

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
	mu          sync.Mutex
	rows        map[string]Story
	ledger      ledger.Store
	publisher   hubemit.Publisher
	openTasksFn OpenTasksFunc
}

// SetPublisher installs the hub emit sink for subsequent mutations.
func (m *MemoryStore) SetPublisher(p hubemit.Publisher) { m.publisher = p }

// SetOpenTasksFunc wires the terminal-transition gate. When set,
// UpdateStatus rejects transitions to done|cancelled while the story's
// chain has any task at status=published or status=planned. Nil disables
// the gate (boot ordering / unit-test paths). Sty_0233fabd.
func (m *MemoryStore) SetOpenTasksFunc(fn OpenTasksFunc) { m.openTasksFn = fn }

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
	emitStatus(ctx, m.publisher, s)
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
	fn := m.openTasksFn
	m.mu.Unlock()
	if fn != nil && isTerminalStatus(newStatus) {
		ids, err := fn(ctx, id, memberships)
		if err != nil {
			return Story{}, fmt.Errorf("story: open-tasks lookup: %w", err)
		}
		if len(ids) > 0 {
			return Story{}, &StoryHasOpenTasksError{StoryID: id, OpenTaskIDs: ids}
		}
	}
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
		StoryID:     ledger.StringPtr(s.ID),
		Type:        ledger.TypeDecision,
		Tags:        []string{"kind:" + LedgerEntryType},
		Content:     string(content),
		CreatedBy:   actor,
	}, now); err != nil {
		// Revert the in-memory status change — the ledger emission is
		// load-bearing (pr_20440c77). Caller sees the original error.
		s.Status = prior
		m.rows[id] = s
		return Story{}, fmt.Errorf("story: ledger emission failed (status reverted): %w", err)
	}
	emitStatus(ctx, m.publisher, s)
	return s, nil
}

// UpdateStatusDerived implements Store for MemoryStore. Mirrors
// UpdateStatus exactly except for the ValidTransition guard, which is
// skipped — derivation-driven flips may legitimately cross the manual
// path's forward-only walk (e.g. backlog → in_progress directly when
// a CI claim event arrives before the close walk would have run).
// Sty_dc121948.
func (m *MemoryStore) UpdateStatusDerived(ctx context.Context, id, newStatus string, now time.Time, memberships []string) (Story, error) {
	if !IsKnownStatus(newStatus) {
		return Story{}, fmt.Errorf("story: unknown status %q", newStatus)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[id]
	if !ok {
		return Story{}, ErrNotFound
	}
	if !inStoryMemberships(s.WorkspaceID, memberships) {
		return Story{}, ErrNotFound
	}
	if s.Status == newStatus {
		return s, nil
	}
	prior := s.Status
	s.Status = newStatus
	s.UpdatedAt = now
	m.rows[id] = s

	payload := transitionPayload{
		StoryID: id,
		From:    prior,
		To:      newStatus,
		Actor:   derivedActor,
	}
	content, _ := json.Marshal(payload)
	if _, err := m.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: s.WorkspaceID,
		ProjectID:   s.ProjectID,
		StoryID:     ledger.StringPtr(s.ID),
		Type:        ledger.TypeDecision,
		Tags:        []string{"kind:" + LedgerEntryType, "reason:derived"},
		Content:     string(content),
		CreatedBy:   derivedActor,
	}, now); err != nil {
		s.Status = prior
		m.rows[id] = s
		return Story{}, fmt.Errorf("story: derived ledger emission failed (status reverted): %w", err)
	}
	emitStatus(ctx, m.publisher, s)
	return s, nil
}

// Update implements Store for MemoryStore.
func (m *MemoryStore) Update(ctx context.Context, id string, fields UpdateFields, actor string, now time.Time, memberships []string) (Story, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[id]
	if !ok {
		return Story{}, ErrNotFound
	}
	if !inStoryMemberships(s.WorkspaceID, memberships) {
		return Story{}, ErrNotFound
	}
	applyUpdateFields(&s, fields)
	s.UpdatedAt = now
	m.rows[id] = s
	return s, nil
}

// applyUpdateFields copies non-nil pointers from fields onto s. Tags is
// wholesale-replace — a non-nil empty slice clears the tag list. Shared
// by Memory + Surreal stores so the semantics stay aligned.
func applyUpdateFields(s *Story, fields UpdateFields) {
	if fields.Title != nil {
		s.Title = *fields.Title
	}
	if fields.Description != nil {
		s.Description = *fields.Description
	}
	if fields.AcceptanceCriteria != nil {
		s.AcceptanceCriteria = *fields.AcceptanceCriteria
	}
	if fields.Category != nil {
		s.Category = *fields.Category
	}
	if fields.Priority != nil {
		s.Priority = *fields.Priority
	}
	if fields.Tags != nil {
		next := append([]string{}, (*fields.Tags)...)
		s.Tags = next
	}
}

// SetField implements Store for MemoryStore.
func (m *MemoryStore) SetField(ctx context.Context, id, field string, value any, actor string, now time.Time, memberships []string) (Story, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[id]
	if !ok {
		return Story{}, ErrNotFound
	}
	if !inStoryMemberships(s.WorkspaceID, memberships) {
		return Story{}, ErrNotFound
	}
	if s.Fields == nil {
		s.Fields = make(map[string]any)
	}
	if value == nil || isEmptyString(value) {
		delete(s.Fields, field)
	} else {
		s.Fields[field] = value
	}
	s.UpdatedAt = now
	m.rows[id] = s
	return s, nil
}

// isEmptyString reports whether v is the literal empty string. Used by
// SetField to interpret value="" as "clear the field" — so a caller can
// erase a value with the same shape as setting it.
func isEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s == ""
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

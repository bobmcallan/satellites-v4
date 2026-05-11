package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// ErrLedgerStoreNotConfigured is returned when the typed ledger
// surface is called against a Client whose Deps.Ledger is nil. The
// wire-layer caller maps this to a per-verb "unavailable" envelope.
var ErrLedgerStoreNotConfigured = errors.New("ledger store not configured")

// LedgerAppendInput captures the rich row fields ledger_append accepts.
// ResolvedProjectID + WorkspaceID + ImpersonatingAsWorkspace are
// pre-resolved by the wire layer (which owns project-access + global-
// admin impersonation policy); the typed method writes the row.
type LedgerAppendInput struct {
	ResolvedProjectID        string
	WorkspaceID              string
	StoryID                  string
	EventType                string
	Content                  string
	Tags                     []string
	Durability               string
	SourceType               string
	Sensitive                bool
	Structured               json.RawMessage
	ExpiresAt                *time.Time
	ImpersonatingAsWorkspace string
	Now                      time.Time
}

// LedgerAppend writes an event row to the project's ledger. The verb
// classifies raw event types into the architecture.md §6 enum
// (plan|action_claim|artifact|evidence|decision|close-request|verdict|
// workflow-claim|kv); unknown types fall through to TypeDecision with
// a `kind:<value>` tag preserving the original token.
func (c *Client) LedgerAppend(ctx context.Context, caller Caller, in LedgerAppendInput) (ledger.LedgerEntry, error) {
	if c.deps.Ledger == nil {
		return ledger.LedgerEntry{}, ErrLedgerStoreNotConfigured
	}
	if in.ResolvedProjectID == "" {
		return ledger.LedgerEntry{}, errors.New("project_id required")
	}
	if in.EventType == "" {
		return ledger.LedgerEntry{}, errors.New("type required")
	}
	entryType, classifiedTags := ClassifyLedgerEvent(in.EventType)
	tags := append([]string{}, classifiedTags...)
	tags = append(tags, in.Tags...)

	entry := ledger.LedgerEntry{
		WorkspaceID:              in.WorkspaceID,
		ProjectID:                in.ResolvedProjectID,
		StoryID:                  ledger.StringPtr(in.StoryID),
		Type:                     entryType,
		Tags:                     tags,
		Content:                  in.Content,
		Durability:               in.Durability,
		SourceType:               in.SourceType,
		Sensitive:                in.Sensitive,
		Structured:               in.Structured,
		ExpiresAt:                in.ExpiresAt,
		ImpersonatingAsWorkspace: in.ImpersonatingAsWorkspace,
		CreatedBy:                caller.UserID,
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Ledger.Append(ctx, entry, now)
}

// ClassifyLedgerEvent maps a raw event-type token to the architecture
// enum used on the ledger row. Mirrors the wire-layer helper that
// shipped pre-extraction (story_d2a03cea pattern). Exported so the
// wire layer + the CLI can produce identical classifications when
// constructing LedgerAppendInput.
func ClassifyLedgerEvent(eventType string) (string, []string) {
	switch eventType {
	case ledger.TypePlan, ledger.TypeActionClaim, ledger.TypeArtifact,
		ledger.TypeEvidence, ledger.TypeDecision, ledger.TypeCloseRequest,
		ledger.TypeVerdict, ledger.TypeWorkflowClaim, ledger.TypeKV:
		return eventType, nil
	default:
		return ledger.TypeDecision, []string{"kind:" + eventType}
	}
}

// LedgerListInput captures the filter knobs for ledger_list. The
// wire-layer caller pre-resolves the project context and threads it
// in as ResolvedProjectID.
type LedgerListInput struct {
	ResolvedProjectID string
	Options           ledger.ListOptions
	Memberships       []string
}

// LedgerList returns rows scoped to the resolved project, applying
// the supplied list options (type / story_id / tags / durability /
// source_type / status / sensitive / include_dereferenced / limit).
func (c *Client) LedgerList(ctx context.Context, caller Caller, in LedgerListInput) ([]ledger.LedgerEntry, error) {
	if c.deps.Ledger == nil {
		return nil, ErrLedgerStoreNotConfigured
	}
	if in.ResolvedProjectID == "" {
		return nil, errors.New("project_id required")
	}
	return c.deps.Ledger.List(ctx, in.ResolvedProjectID, in.Options, in.Memberships)
}

// LedgerGetInput is the input for LedgerGet.
type LedgerGetInput struct {
	ID          string
	Memberships []string
}

// LedgerGet returns the ledger row by id, scoped by membership.
func (c *Client) LedgerGet(ctx context.Context, caller Caller, in LedgerGetInput) (ledger.LedgerEntry, error) {
	if c.deps.Ledger == nil {
		return ledger.LedgerEntry{}, errors.New("ledger store not configured")
	}
	if in.ID == "" {
		return ledger.LedgerEntry{}, errors.New("id required")
	}
	return c.deps.Ledger.GetByID(ctx, in.ID, in.Memberships)
}

// LedgerSearchInput is the input for LedgerSearch.
type LedgerSearchInput struct {
	ResolvedProjectID string
	Memberships       []string
	Options           ledger.SearchOptions
}

// LedgerSearch performs a ledger search against the resolved project,
// preferring SearchSemantic when a non-empty query is supplied and
// falling back to Search on ErrSemanticUnavailable.
func (c *Client) LedgerSearch(ctx context.Context, caller Caller, in LedgerSearchInput) ([]ledger.LedgerEntry, error) {
	if c.deps.Ledger == nil {
		return nil, errors.New("ledger store not configured")
	}
	if in.Options.Query != "" {
		rows, err := c.deps.Ledger.SearchSemantic(ctx, in.ResolvedProjectID, in.Options.Query, in.Options, in.Memberships)
		if errors.Is(err, ledger.ErrSemanticUnavailable) {
			return c.deps.Ledger.Search(ctx, in.ResolvedProjectID, in.Options, in.Memberships)
		}
		return rows, err
	}
	return c.deps.Ledger.Search(ctx, in.ResolvedProjectID, in.Options, in.Memberships)
}

// LedgerRecallInput is the input for LedgerRecall.
type LedgerRecallInput struct {
	RootID      string
	Memberships []string
}

// LedgerRecall walks the dereferenced-row chain from the root.
func (c *Client) LedgerRecall(ctx context.Context, caller Caller, in LedgerRecallInput) ([]ledger.LedgerEntry, error) {
	if c.deps.Ledger == nil {
		return nil, errors.New("ledger store not configured")
	}
	if in.RootID == "" {
		return nil, errors.New("root_id required")
	}
	return c.deps.Ledger.Recall(ctx, in.RootID, in.Memberships)
}

// LedgerDereferenceInput is the input for LedgerDereference.
type LedgerDereferenceInput struct {
	ID          string
	Reason      string
	Memberships []string
	Now         time.Time
}

// LedgerDereference marks the row as dereferenced with audit metadata.
func (c *Client) LedgerDereference(ctx context.Context, caller Caller, in LedgerDereferenceInput) (ledger.LedgerEntry, error) {
	if c.deps.Ledger == nil {
		return ledger.LedgerEntry{}, errors.New("ledger store not configured")
	}
	if in.ID == "" {
		return ledger.LedgerEntry{}, errors.New("id required")
	}
	if in.Reason == "" {
		return ledger.LedgerEntry{}, errors.New("reason required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Ledger.Dereference(ctx, in.ID, in.Reason, caller.UserID, now, in.Memberships)
}

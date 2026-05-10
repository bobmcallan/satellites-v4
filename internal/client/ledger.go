package client

import (
	"context"
	"errors"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

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

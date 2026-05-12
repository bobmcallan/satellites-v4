package client

import (
	"context"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/changelog"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// Caller carries the resolved tenancy context for a typed client call.
// The wire-layer handler builds it from request headers + the auth
// store; the typed methods use the memberships slice for workspace
// scoping when calling the underlying stores. GlobalAdmin is the
// resolved global-admin flag from the auth pipeline; the typed
// resolution helpers consult it to widen lookups across tenancy.
type Caller struct {
	UserID      string
	Email       string
	Memberships []string
	GlobalAdmin bool
}

// Deps is the dependency bundle a Client needs. The wire layer (the
// mcpserver.Server) constructs the stores once and passes them in;
// the CLI scaffold of order:03+ constructs an HTTP-backed remote
// client that satisfies a subset of the same surface.
type Deps struct {
	Documents        document.Store
	Projects         project.Store
	Ledger           ledger.Store
	Stories          story.Store
	Workspaces       workspace.Store
	Sessions         session.Store
	Tasks            task.Store
	Repos            repo.Store
	Changelog        changelog.Store
	Indexer          codeindex.Indexer
	StartedAt        time.Time
	DefaultProjectID string
	Logger           arbor.ILogger
}

// Client carries the typed business surface that callers (MCP, CLI,
// any future caller) delegate to. Each method takes (ctx, caller, in)
// and returns (out, err) — no wire-format concerns.
type Client struct {
	deps Deps
}

// New constructs a Client bound to the supplied dependency bundle.
func New(d Deps) *Client {
	return &Client{deps: d}
}

// Stores exposes the underlying dependency bundle for callers that
// must temporarily delegate back to wire-layer helpers during the
// order:02 migration. Once every verb is extracted (order:02-followup
// or as part of a later slice), this accessor is removed.
//
// Do not add new uses of Stores() outside the migration path — the
// architectural contract (doc.go) is "typed methods, no exposed
// stores" once the migration completes.
func (c *Client) Stores() Deps {
	return c.deps
}

// noopCtx is a context-only helper to silence unused-import warnings
// in skeletons that have not yet wired any verbs. Removed once the
// first noun ships on the typed surface.
var _ = context.Background

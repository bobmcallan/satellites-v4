package client

import (
	"context"
	"errors"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// EnsureCallerWorkspaces returns the caller's member-workspace ids,
// minting a default workspace on first sight via workspace.EnsureDefault
// (matches the OnUserCreated hook for human logins, and covers
// synthetic callers like API keys that didn't flow through the auth
// bootstrap path). Returns nil when the workspace store is disabled
// (pre-tenant mode); empty slice when the caller is unauthenticated.
//
// Migrated from mcpserver.Server.ensureCallerWorkspaces per
// sty_068a6c46.
func (c *Client) EnsureCallerWorkspaces(ctx context.Context, caller Caller) []string {
	if c.deps.Workspaces == nil {
		return nil
	}
	if caller.UserID == "" {
		return []string{}
	}
	list, err := c.deps.Workspaces.ListByMember(ctx, caller.UserID)
	if err != nil {
		return []string{}
	}
	if len(list) == 0 {
		logger := c.deps.Logger
		if logger == nil {
			logger = satarbor.Default()
		}
		if _, err := workspace.EnsureDefault(ctx, c.deps.Workspaces, logger, caller.UserID, time.Now().UTC()); err == nil {
			list, _ = c.deps.Workspaces.ListByMember(ctx, caller.UserID)
		}
	}
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, w.ID)
	}
	return out
}

// ResolveCallerMemberships returns the caller's memberships slice as
// the store reads expect: nil when the workspace store is disabled
// (pre-tenant behaviour), empty slice when the caller has no
// membership yet (deny-all), non-empty workspace ids otherwise. See
// docs/architecture.md §8.
//
// Migrated from mcpserver.Server.resolveCallerMemberships per
// sty_068a6c46.
func (c *Client) ResolveCallerMemberships(ctx context.Context, caller Caller) []string {
	return c.EnsureCallerWorkspaces(ctx, caller)
}

// ResolveCallerWorkspaceID returns the caller's default workspace id,
// or empty when the caller is unauthenticated or the workspace store
// is off. Write paths use this to stamp workspace_id on new rows.
//
// Migrated from mcpserver.Server.resolveCallerWorkspaceID per
// sty_068a6c46.
func (c *Client) ResolveCallerWorkspaceID(ctx context.Context, caller Caller) string {
	ids := c.EnsureCallerWorkspaces(ctx, caller)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// ResolveProjectID picks the document-operation project scope for the
// caller. scopedProjectID is the URL-scoped value carried by the wire
// layer when present (mcpserver reads it from the inbound URL); the
// HTTP API path passes "".
//
// Rules:
//  1. If scopedProjectID is set, any explicit `requested` must match
//     it (cross-project tool calls are rejected); when `requested` is
//     empty, the scoped value is used.
//  2. If requested is set, the caller must own that project or it
//     must be the system default; cross-project access returns an
//     error unless caller.GlobalAdmin is true (story_3548cde2).
//  3. Otherwise, fall back to the caller's first owned project.
//  4. Otherwise, fall back to the system default (c.deps.DefaultProjectID).
//
// Migrated from mcpserver.Server.resolveProjectID per sty_068a6c46.
// The URL-scoping check that previously lived inside the helper now
// runs at the wire layer (mcpserver reads ScopedProjectIDFrom(ctx)
// and passes it in).
func (c *Client) ResolveProjectID(ctx context.Context, requested, scopedProjectID string, caller Caller, memberships []string) (string, error) {
	if scopedProjectID != "" {
		if requested != "" && requested != scopedProjectID {
			return "", errors.New("project_id parameter does not match the URL-scoped project_id")
		}
		requested = scopedProjectID
	}
	if requested != "" {
		if requested == c.deps.DefaultProjectID {
			return requested, nil
		}
		lookupMemberships := memberships
		if caller.GlobalAdmin {
			lookupMemberships = nil
		}
		p, err := c.projectsSafe().GetByID(ctx, requested, lookupMemberships)
		if err != nil {
			return "", errors.New("project not found or access denied")
		}
		if p.OwnerUserID != caller.UserID && !caller.GlobalAdmin {
			return "", errors.New("project not found or access denied")
		}
		return requested, nil
	}
	if c.deps.Projects != nil && caller.UserID != "" {
		list, err := c.deps.Projects.ListByOwner(ctx, caller.UserID, memberships)
		if err == nil && len(list) > 0 {
			return list[0].ID, nil
		}
	}
	if c.deps.DefaultProjectID != "" {
		return c.deps.DefaultProjectID, nil
	}
	return "", errors.New("no project context available")
}

// ResolveProjectWorkspaceID returns the workspace_id of the given
// project, or empty when the project has none yet (legacy path before
// backfill). The lookup uses a nil memberships filter because callers
// have already applied access scoping via ResolveProjectID.
//
// Migrated from mcpserver.Server.resolveProjectWorkspaceID per
// sty_068a6c46.
func (c *Client) ResolveProjectWorkspaceID(ctx context.Context, projectID string) string {
	if c.deps.Projects == nil || projectID == "" {
		return ""
	}
	p, err := c.deps.Projects.GetByID(ctx, projectID, nil)
	if err != nil {
		return ""
	}
	return p.WorkspaceID
}

// WorkspaceInMemberships reports whether wsID is in the caller's
// memberships slice. Used by ledger_append to decide whether a write
// crosses the tenancy boundary and warrants stamping
// impersonating_as_workspace (story_3548cde2).
//
// Migrated from mcpserver.ledgerWorkspaceInMemberships per
// sty_068a6c46.
func WorkspaceInMemberships(wsID string, memberships []string) bool {
	if wsID == "" || len(memberships) == 0 {
		return false
	}
	for _, m := range memberships {
		if m == wsID {
			return true
		}
	}
	return false
}

// projectsSafe returns the project store, or a zero-value memory
// implementation when the Client was constructed without one. The
// upstream verb registrations already gate project_* on a non-nil
// store; this is a safety net for callers that arrive with a
// requested project_id when projects are disabled.
func (c *Client) projectsSafe() project.Store {
	if c.deps.Projects != nil {
		return c.deps.Projects
	}
	return project.NewMemoryStore()
}

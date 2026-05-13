// Package client — workspace verbs (sty_f3f7bf9b slice 8).
//
// Slice 8 of sty_f3f7bf9b lifted the workspace mutators (create +
// member add/update-role/remove) out of mcpserver/mcp.go into this
// file, alongside the previously-extracted reads (get/list/member_list,
// which had landed inline in operator_reads.go). The mcpserver
// adapters now parse the request, build a Workspace*Input, call the
// typed surface, and marshal the wire envelope — byte-identical to
// the pre-extraction wire shape.
//
// The membership-mutation handlers (member_add, member_update_role,
// member_remove) carry an "admin role required" guard plus a
// last-admin guard on downgrade/removal. Both guards travel inside
// the typed methods so the wire layer stays a thin parse/marshal
// shell.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// ErrWorkspaceStoreNotConfigured is returned when a workspace verb is
// invoked against a Client whose Deps.Workspaces is nil.
var ErrWorkspaceStoreNotConfigured = errors.New("workspace: store not configured")

// WorkspaceGetInput identifies a workspace lookup.
type WorkspaceGetInput struct {
	ID string
}

// WorkspaceGet returns a workspace row when the caller is a member.
func (c *Client) WorkspaceGet(ctx context.Context, caller Caller, in WorkspaceGetInput) (workspace.Workspace, error) {
	if c.deps.Workspaces == nil {
		return workspace.Workspace{}, errors.New("workspace store not configured")
	}
	if in.ID == "" {
		return workspace.Workspace{}, errors.New("id required")
	}
	if caller.UserID == "" {
		return workspace.Workspace{}, errors.New("no caller identity")
	}
	is, err := c.deps.Workspaces.IsMember(ctx, in.ID, caller.UserID)
	if err != nil || !is {
		return workspace.Workspace{}, errors.New("workspace not found")
	}
	w, err := c.deps.Workspaces.GetByID(ctx, in.ID)
	if err != nil {
		return workspace.Workspace{}, errors.New("workspace not found")
	}
	return w, nil
}

// WorkspaceList returns the workspaces the caller is a member of.
func (c *Client) WorkspaceList(ctx context.Context, caller Caller) ([]workspace.Workspace, error) {
	if c.deps.Workspaces == nil {
		return nil, errors.New("workspace store not configured")
	}
	if caller.UserID == "" {
		return nil, errors.New("no caller identity")
	}
	return c.deps.Workspaces.ListByMember(ctx, caller.UserID)
}

// WorkspaceMemberListInput identifies a workspace member-list lookup.
type WorkspaceMemberListInput struct {
	WorkspaceID string
}

// WorkspaceMemberList returns the members of a workspace when the caller
// is a member.
func (c *Client) WorkspaceMemberList(ctx context.Context, caller Caller, in WorkspaceMemberListInput) ([]workspace.Member, error) {
	if c.deps.Workspaces == nil {
		return nil, errors.New("workspace store not configured")
	}
	if in.WorkspaceID == "" {
		return nil, errors.New("workspace_id required")
	}
	if caller.UserID == "" {
		return nil, errors.New("no caller identity")
	}
	is, err := c.deps.Workspaces.IsMember(ctx, in.WorkspaceID, caller.UserID)
	if err != nil || !is {
		return nil, errors.New("workspace not found")
	}
	return c.deps.Workspaces.ListMembers(ctx, in.WorkspaceID)
}

// WorkspaceAddInput captures the workspace_add request shape.
// Now overrides the timestamp the store stamps onto CreatedAt /
// UpdatedAt — tests inject a deterministic value; the wire layer
// leaves it zero so the typed method falls back to time.Now().UTC().
type WorkspaceAddInput struct {
	Name string
	Now  time.Time
}

// WorkspaceAdd persists a new workspace and records the caller as
// the founding admin. Returns the workspace row verbatim.
func (c *Client) WorkspaceAdd(ctx context.Context, caller Caller, in WorkspaceAddInput) (workspace.Workspace, error) {
	if c.deps.Workspaces == nil {
		return workspace.Workspace{}, ErrWorkspaceStoreNotConfigured
	}
	if caller.UserID == "" {
		return workspace.Workspace{}, errors.New("no caller identity")
	}
	if in.Name == "" {
		return workspace.Workspace{}, errors.New("name is required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Workspaces.Create(ctx, caller.UserID, in.Name, now)
}

// WorkspaceMemberAddInput captures the workspace_member_add request
// shape. ActorSource is unused today but kept on the input struct so
// future audit-row extensions (kind:workspace.member_add) can stamp
// the auth path without breaking callers. Now is the audit/AddedAt
// timestamp.
type WorkspaceMemberAddInput struct {
	WorkspaceID string
	UserID      string
	Role        string
	Now         time.Time
}

// WorkspaceMemberAddOutput mirrors the JSON shape the wire handler
// previously emitted. The bare-bones envelope omits the AddedBy /
// AddedAt fields the store records — historical wire shape, preserved
// verbatim.
type WorkspaceMemberAddOutput struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Role        string `json:"role"`
}

// WorkspaceMemberAdd inserts a membership row after asserting the
// caller is a workspace admin. Writes a kind:workspace.member_add
// ledger row.
func (c *Client) WorkspaceMemberAdd(ctx context.Context, caller Caller, in WorkspaceMemberAddInput) (WorkspaceMemberAddOutput, error) {
	if c.deps.Workspaces == nil {
		return WorkspaceMemberAddOutput{}, ErrWorkspaceStoreNotConfigured
	}
	if in.WorkspaceID == "" {
		return WorkspaceMemberAddOutput{}, errors.New("workspace_id is required")
	}
	if in.UserID == "" {
		return WorkspaceMemberAddOutput{}, errors.New("user_id is required")
	}
	if in.Role == "" {
		return WorkspaceMemberAddOutput{}, errors.New("role is required")
	}
	if err := c.requireWorkspaceAdmin(ctx, caller, in.WorkspaceID); err != nil {
		return WorkspaceMemberAddOutput{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := c.deps.Workspaces.AddMember(ctx, in.WorkspaceID, in.UserID, in.Role, caller.UserID, now); err != nil {
		return WorkspaceMemberAddOutput{}, err
	}
	c.appendMembershipAudit(ctx, in.WorkspaceID, "member_add", caller.UserID, now, map[string]any{
		"target_user_id": in.UserID,
		"role":           in.Role,
	})
	return WorkspaceMemberAddOutput{WorkspaceID: in.WorkspaceID, UserID: in.UserID, Role: in.Role}, nil
}

// WorkspaceMemberUpdateRoleInput captures the workspace_member_update_role
// request shape.
type WorkspaceMemberUpdateRoleInput struct {
	WorkspaceID string
	UserID      string
	Role        string
	Now         time.Time
}

// WorkspaceMemberUpdateRoleOutput mirrors the JSON shape the wire
// handler previously emitted.
type WorkspaceMemberUpdateRoleOutput struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Role        string `json:"role"`
}

// WorkspaceMemberUpdateRole mutates an existing member's role. The
// caller must be a workspace admin. Downgrading the last admin is
// rejected with "cannot downgrade the last admin". Writes a
// kind:workspace.member_update_role ledger row.
func (c *Client) WorkspaceMemberUpdateRole(ctx context.Context, caller Caller, in WorkspaceMemberUpdateRoleInput) (WorkspaceMemberUpdateRoleOutput, error) {
	if c.deps.Workspaces == nil {
		return WorkspaceMemberUpdateRoleOutput{}, ErrWorkspaceStoreNotConfigured
	}
	if in.WorkspaceID == "" {
		return WorkspaceMemberUpdateRoleOutput{}, errors.New("workspace_id is required")
	}
	if in.UserID == "" {
		return WorkspaceMemberUpdateRoleOutput{}, errors.New("user_id is required")
	}
	if in.Role == "" {
		return WorkspaceMemberUpdateRoleOutput{}, errors.New("role is required")
	}
	if err := c.requireWorkspaceAdmin(ctx, caller, in.WorkspaceID); err != nil {
		return WorkspaceMemberUpdateRoleOutput{}, err
	}
	currentRole, err := c.deps.Workspaces.GetRole(ctx, in.WorkspaceID, in.UserID)
	if err != nil {
		return WorkspaceMemberUpdateRoleOutput{}, errors.New("member not found")
	}
	if currentRole == workspace.RoleAdmin && in.Role != workspace.RoleAdmin {
		count, err := c.workspaceAdminCount(ctx, in.WorkspaceID)
		if err != nil {
			return WorkspaceMemberUpdateRoleOutput{}, err
		}
		if count <= 1 {
			return WorkspaceMemberUpdateRoleOutput{}, errors.New("cannot downgrade the last admin")
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := c.deps.Workspaces.UpdateRole(ctx, in.WorkspaceID, in.UserID, in.Role, now); err != nil {
		return WorkspaceMemberUpdateRoleOutput{}, err
	}
	c.appendMembershipAudit(ctx, in.WorkspaceID, "member_update_role", caller.UserID, now, map[string]any{
		"target_user_id": in.UserID,
		"previous_role":  currentRole,
		"new_role":       in.Role,
	})
	return WorkspaceMemberUpdateRoleOutput{WorkspaceID: in.WorkspaceID, UserID: in.UserID, Role: in.Role}, nil
}

// WorkspaceMemberRemoveInput captures the workspace_member_remove
// request shape.
type WorkspaceMemberRemoveInput struct {
	WorkspaceID string
	UserID      string
	Now         time.Time
}

// WorkspaceMemberRemoveOutput mirrors the JSON shape the wire handler
// previously emitted ({workspace_id, user_id, removed:true}).
type WorkspaceMemberRemoveOutput struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Removed     bool   `json:"removed"`
}

// WorkspaceMemberRemove deletes a membership row after asserting the
// caller is a workspace admin. Removing the last admin is rejected
// with "cannot remove the last admin". Writes a
// kind:workspace.member_remove ledger row.
func (c *Client) WorkspaceMemberRemove(ctx context.Context, caller Caller, in WorkspaceMemberRemoveInput) (WorkspaceMemberRemoveOutput, error) {
	if c.deps.Workspaces == nil {
		return WorkspaceMemberRemoveOutput{}, ErrWorkspaceStoreNotConfigured
	}
	if in.WorkspaceID == "" {
		return WorkspaceMemberRemoveOutput{}, errors.New("workspace_id is required")
	}
	if in.UserID == "" {
		return WorkspaceMemberRemoveOutput{}, errors.New("user_id is required")
	}
	if err := c.requireWorkspaceAdmin(ctx, caller, in.WorkspaceID); err != nil {
		return WorkspaceMemberRemoveOutput{}, err
	}
	currentRole, err := c.deps.Workspaces.GetRole(ctx, in.WorkspaceID, in.UserID)
	if err != nil {
		return WorkspaceMemberRemoveOutput{}, errors.New("member not found")
	}
	if currentRole == workspace.RoleAdmin {
		count, err := c.workspaceAdminCount(ctx, in.WorkspaceID)
		if err != nil {
			return WorkspaceMemberRemoveOutput{}, err
		}
		if count <= 1 {
			return WorkspaceMemberRemoveOutput{}, errors.New("cannot remove the last admin")
		}
	}
	if err := c.deps.Workspaces.RemoveMember(ctx, in.WorkspaceID, in.UserID); err != nil {
		return WorkspaceMemberRemoveOutput{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	c.appendMembershipAudit(ctx, in.WorkspaceID, "member_remove", caller.UserID, now, map[string]any{
		"target_user_id": in.UserID,
		"previous_role":  currentRole,
	})
	return WorkspaceMemberRemoveOutput{WorkspaceID: in.WorkspaceID, UserID: in.UserID, Removed: true}, nil
}

// requireWorkspaceAdmin asserts the caller is an admin of the given
// workspace. Returns a user-friendly error on mismatch — the wire
// layer emits the message verbatim.
func (c *Client) requireWorkspaceAdmin(ctx context.Context, caller Caller, workspaceID string) error {
	if caller.UserID == "" {
		return errors.New("no caller identity")
	}
	role, err := c.deps.Workspaces.GetRole(ctx, workspaceID, caller.UserID)
	if err != nil {
		return errors.New("workspace not found")
	}
	if role != workspace.RoleAdmin {
		return errors.New("admin role required")
	}
	return nil
}

// workspaceAdminCount returns the number of admin members on a
// workspace. Used for the last-admin guard on downgrades and
// removals.
func (c *Client) workspaceAdminCount(ctx context.Context, workspaceID string) (int, error) {
	members, err := c.deps.Workspaces.ListMembers(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range members {
		if m.Role == workspace.RoleAdmin {
			n++
		}
	}
	return n, nil
}

// appendMembershipAudit writes a ledger row recording a membership
// mutation. Scoped to the system default project + the target
// workspace. Safe to no-op when defaults aren't wired (tests).
func (c *Client) appendMembershipAudit(ctx context.Context, workspaceID, kind, actor string, now time.Time, payload map[string]any) {
	if c.deps.Ledger == nil || c.deps.DefaultProjectID == "" {
		return
	}
	payload["workspace_id"] = workspaceID
	payload["kind"] = kind
	body, _ := json.Marshal(payload)
	_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   c.deps.DefaultProjectID,
		Type:        ledger.TypeDecision,
		Tags:        []string{"kind:workspace." + kind},
		Content:     string(body),
		CreatedBy:   actor,
	}, now)
}

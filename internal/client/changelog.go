// Package client — changelog verbs (sty_f3f7bf9b slice 9).
//
// Slice 9 of sty_f3f7bf9b lifted the changelog mutators (add + update
// + delete) out of mcpserver/changelog_handlers.go into this file,
// alongside the previously-extracted reads (get / list, which had
// landed inline in operator_reads.go during the read-tier gap-fill
// of sty_004f3d3a). The mcpserver adapters now parse the request,
// build a Changelog*Input, call the typed surface, and marshal the
// wire envelope — byte-identical to the pre-extraction wire shape.
//
// Validation, project + workspace resolution, and the per-verb
// access guards all travel inside the typed methods. The wire layer
// stays a thin parse/marshal shell so the same business logic
// services both transports (MCP today; HTTP /api/v1 once the
// operator_writes wire-side switches to the typed surface in a
// follow-up slice).
package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/changelog"
)

// ErrChangelogStoreNotConfigured is returned when a changelog verb is
// invoked against a Client whose Deps.Changelog is nil.
var ErrChangelogStoreNotConfigured = errors.New("changelog: store not configured")

// ChangelogGetInput names a changelog row lookup.
type ChangelogGetInput struct {
	ID          string
	Memberships []string
}

// ChangelogGet returns a changelog row when the caller has access to its
// project.
func (c *Client) ChangelogGet(ctx context.Context, caller Caller, in ChangelogGetInput) (changelog.Changelog, error) {
	if c.deps.Changelog == nil {
		return changelog.Changelog{}, errors.New("changelog store not configured")
	}
	if in.ID == "" {
		return changelog.Changelog{}, errors.New("id required")
	}
	row, err := c.deps.Changelog.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return changelog.Changelog{}, errors.New("changelog not found")
	}
	if _, err := c.ResolveProjectID(ctx, row.ProjectID, "", caller, in.Memberships); err != nil {
		return changelog.Changelog{}, errors.New("changelog not found")
	}
	return row, nil
}

// ChangelogListInput captures the filter set for changelog_list.
type ChangelogListInput struct {
	ProjectID   string
	Service     string
	Limit       int
	Memberships []string
}

// ChangelogList returns changelog rows newest-first.
func (c *Client) ChangelogList(ctx context.Context, caller Caller, in ChangelogListInput) ([]changelog.Changelog, error) {
	if c.deps.Changelog == nil {
		return nil, errors.New("changelog store not configured")
	}
	projectID, err := c.ResolveProjectID(ctx, in.ProjectID, "", caller, in.Memberships)
	if err != nil {
		return nil, err
	}
	return c.deps.Changelog.List(ctx, changelog.ListOptions{
		ProjectID: projectID,
		Service:   in.Service,
		Limit:     in.Limit,
	}, in.Memberships)
}

// ChangelogAddInput captures the changelog_add request shape. Now is
// the optional create timestamp; the zero value falls back to
// time.Now().UTC(). EffectiveDate likewise falls back to Now when
// zero (matching the prior wire behaviour).
type ChangelogAddInput struct {
	ProjectID     string
	Service       string
	VersionFrom   string
	VersionTo     string
	Content       string
	EffectiveDate time.Time
	Memberships   []string
	Now           time.Time
}

// ChangelogAdd persists a new changelog row scoped to the resolved
// project. Returns the persisted row verbatim.
func (c *Client) ChangelogAdd(ctx context.Context, caller Caller, in ChangelogAddInput) (changelog.Changelog, error) {
	if c.deps.Changelog == nil {
		return changelog.Changelog{}, ErrChangelogStoreNotConfigured
	}
	projectID, err := c.ResolveProjectID(ctx, in.ProjectID, "", caller, in.Memberships)
	if err != nil {
		return changelog.Changelog{}, err
	}
	if in.Service == "" {
		return changelog.Changelog{}, errors.New("service is required")
	}
	if in.Content == "" {
		return changelog.Changelog{}, errors.New("content is required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	eff := in.EffectiveDate
	if eff.IsZero() {
		eff = now
	}
	wsID := c.ResolveProjectWorkspaceID(ctx, projectID)
	row := changelog.Changelog{
		WorkspaceID:   wsID,
		ProjectID:     projectID,
		Service:       in.Service,
		VersionFrom:   in.VersionFrom,
		VersionTo:     in.VersionTo,
		Content:       in.Content,
		EffectiveDate: eff,
		CreatedBy:     caller.UserID,
	}
	return c.deps.Changelog.Create(ctx, row, now)
}

// ChangelogUpdateInput captures the changelog_update request shape.
// Pointer-typed fields distinguish "omitted" from "clear" — matching
// the wire layer's prior args["field"]-present check. Now overrides
// the UpdatedAt timestamp; zero falls back to time.Now().UTC().
type ChangelogUpdateInput struct {
	ID            string
	VersionFrom   *string
	VersionTo     *string
	Content       *string
	EffectiveDate *time.Time
	Memberships   []string
	Now           time.Time
}

// ChangelogUpdate mutates an existing changelog row after asserting
// caller access to its project. Returns the updated row verbatim.
func (c *Client) ChangelogUpdate(ctx context.Context, caller Caller, in ChangelogUpdateInput) (changelog.Changelog, error) {
	if c.deps.Changelog == nil {
		return changelog.Changelog{}, ErrChangelogStoreNotConfigured
	}
	if in.ID == "" {
		return changelog.Changelog{}, errors.New("id required")
	}
	current, err := c.deps.Changelog.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return changelog.Changelog{}, errors.New("changelog not found")
	}
	if _, err := c.ResolveProjectID(ctx, current.ProjectID, "", caller, in.Memberships); err != nil {
		return changelog.Changelog{}, errors.New("changelog not found")
	}
	fields := changelog.UpdateFields{
		VersionFrom:   in.VersionFrom,
		VersionTo:     in.VersionTo,
		Content:       in.Content,
		EffectiveDate: in.EffectiveDate,
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Changelog.Update(ctx, in.ID, fields, now, in.Memberships)
}

// ChangelogDeleteInput captures the changelog_delete request shape.
type ChangelogDeleteInput struct {
	ID          string
	Memberships []string
}

// ChangelogDeleteOutput mirrors the JSON shape the wire handler
// previously emitted ({id, deleted:true}).
type ChangelogDeleteOutput struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// ChangelogDelete removes a changelog row after asserting caller
// access to its project.
func (c *Client) ChangelogDelete(ctx context.Context, caller Caller, in ChangelogDeleteInput) (ChangelogDeleteOutput, error) {
	if c.deps.Changelog == nil {
		return ChangelogDeleteOutput{}, ErrChangelogStoreNotConfigured
	}
	if in.ID == "" {
		return ChangelogDeleteOutput{}, errors.New("id required")
	}
	current, err := c.deps.Changelog.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return ChangelogDeleteOutput{}, errors.New("changelog not found")
	}
	if _, err := c.ResolveProjectID(ctx, current.ProjectID, "", caller, in.Memberships); err != nil {
		return ChangelogDeleteOutput{}, errors.New("changelog not found")
	}
	if err := c.deps.Changelog.Delete(ctx, in.ID, in.Memberships); err != nil {
		return ChangelogDeleteOutput{}, err
	}
	return ChangelogDeleteOutput{ID: in.ID, Deleted: true}, nil
}

// ParseChangelogEffectiveDate is a pure helper exported for wire-layer
// adapters that need to parse the historical effective_date input
// string. It preserves the prior behaviour: empty / missing → zero
// time, otherwise RFC3339-parsed.
func ParseChangelogEffectiveDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("effective_date: parse RFC3339: %w", err)
	}
	return t, nil
}

// Package client — project view composer (sty_4db0e025 slice A11).
//
// The wire layer (mcpserver + httpserver) used to compose a
// `projectView` struct that embedded project.Project alongside two
// computed fields (`mcp_url`, `mcp_config`). Slice A11 lifts both the
// struct and the resolver onto the typed client surface so transport
// files do not import internal/project directly. The wire layer calls
// BuildProjectViewJSON to get the wire-shape bytes back, then writes
// them through the transport-specific response envelope.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/bobmcallan/satellites/internal/project"
)

// ProjectView pairs the durable project row with the two computed
// fields the JSON wire shape carries (mcp_url + mcp_config). Mirrors
// the prior mcpserver.projectView; the JSON tag set is identical so
// existing clients still parse the payload.
type ProjectView struct {
	project.Project
	MCPURL    string         `json:"mcp_url,omitempty"`
	MCPConfig map[string]any `json:"mcp_config,omitempty"`
}

// BuildProjectView resolves the mcp_url + mcp_config fields for p and
// returns the embeddable view. Resolution chain mirrors V3 parity:
//
//  1. p.MCPURL (explicit override).
//  2. baseURL (request host / cfg.PublicURL / cfg.OAuthRedirectBaseURL —
//     the wire layer threads its preferred chain in).
//
// Returns a bare ProjectView when no URL resolves (the portal panel
// renders the not-configured empty-state).
func (c *Client) BuildProjectView(p project.Project, baseURL string) ProjectView {
	pv := ProjectView{Project: p}
	url := resolveProjectMCPURL(p.MCPURL, p.ID, baseURL)
	if url == "" {
		return pv
	}
	pv.MCPURL = url
	pv.MCPConfig = map[string]any{
		"mcpServers": map[string]any{
			"satellites": map[string]any{
				"type": "http",
				"url":  url,
			},
		},
	}
	return pv
}

// resolveProjectMCPURL is a string-only equivalent of
// project.ResolveMCPURL. Lives here so the wire layer can build views
// without importing internal/project directly.
func resolveProjectMCPURL(persisted, id, base string) string {
	if persisted != "" {
		return persisted
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || id == "" {
		return ""
	}
	return base + "/mcp?project_id=" + id
}

// ProjectGetViewInput threads the ProjectGet args plus the resolved
// base URL the view composer needs.
type ProjectGetViewInput struct {
	ID          string
	Memberships []string
	BaseURL     string
}

// ProjectGetViewOutput pairs the embeddable project view with the
// orientation bundle the prior wire handler emitted in one JSON map.
type ProjectGetViewOutput struct {
	Project    ProjectView      `json:"project"`
	IntentBody string           `json:"intent_body,omitempty"`
	Principles []PrincipleEntry `json:"principles,omitempty"`
}

// ProjectGetView is the typed equivalent of the prior
// mcpserver.handleProjectGet body. Returns the project view (mcp_url +
// mcp_config) alongside the orientation bundle (intent + principles)
// in one shape the wire layer can marshal verbatim.
func (c *Client) ProjectGetView(ctx context.Context, caller Caller, in ProjectGetViewInput) (ProjectGetViewOutput, error) {
	if c.deps.Projects == nil {
		return ProjectGetViewOutput{}, errors.New("project store not configured")
	}
	if in.ID == "" {
		return ProjectGetViewOutput{}, errors.New("id required")
	}
	p, err := c.deps.Projects.GetByID(ctx, in.ID, in.Memberships)
	if err != nil || p.OwnerUserID != caller.UserID {
		return ProjectGetViewOutput{}, errors.New("project not found")
	}
	bundle := c.BuildOrientation(ctx, p)
	return ProjectGetViewOutput{
		Project:    c.BuildProjectView(p, in.BaseURL),
		IntentBody: bundle.IntentBody,
		Principles: bundle.Principles,
	}, nil
}

// ProjectListJSON returns the caller's projects already marshalled as
// JSON so the wire layer doesn't have to spell out project.Project.
// Mirrors the prior mcpserver.handleProjectList body's marshal step.
func (c *Client) ProjectListJSON(ctx context.Context, caller Caller, memberships []string) ([]byte, int, error) {
	list, err := c.ProjectList(ctx, caller, memberships)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(list)
	if err != nil {
		return nil, 0, err
	}
	return body, len(list), nil
}

// ProjectAddView mints a project and returns the embeddable view
// with mcp_url + mcp_config resolved against baseURL.
func (c *Client) ProjectAddView(ctx context.Context, caller Caller, in ProjectAddInput, baseURL string) (ProjectView, project.Project, error) {
	p, err := c.ProjectAdd(ctx, caller, in)
	if err != nil {
		return ProjectView{}, project.Project{}, err
	}
	return c.BuildProjectView(p, baseURL), p, nil
}

// ProjectUpdateView mutates a project and returns the embeddable view.
func (c *Client) ProjectUpdateView(ctx context.Context, caller Caller, in ProjectUpdateInput, baseURL string) (ProjectView, project.Project, error) {
	p, err := c.ProjectUpdate(ctx, caller, in)
	if err != nil {
		return ProjectView{}, project.Project{}, err
	}
	return c.BuildProjectView(p, baseURL), p, nil
}

// ProjectDeleteView archives a project and returns it as plain JSON
// bytes (no view shape — the wire layer does not embed mcp_url on
// delete responses).
func (c *Client) ProjectDeleteView(ctx context.Context, caller Caller, in ProjectDeleteInput) ([]byte, project.Project, error) {
	p, err := c.ProjectDelete(ctx, caller, in)
	if err != nil {
		return nil, project.Project{}, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, project.Project{}, err
	}
	return body, p, nil
}

// ProjectSetViewOutput pairs ProjectSetOutput's status branches with
// the resolved view shape on the resolved branch.
type ProjectSetViewOutput struct {
	Status           string
	RepoURLCanonical string
	ResolvedView     ProjectView
	ResolvedProject  project.Project
	IntentBody       string
	Principles       []PrincipleEntry
}

// ProjectSetView wraps ProjectSet and pre-builds the project view so
// the wire layer can marshal the JSON envelope without re-handling
// project types.
func (c *Client) ProjectSetView(ctx context.Context, caller Caller, in ProjectSetInput, baseURL string) (ProjectSetViewOutput, error) {
	out, err := c.ProjectSet(ctx, caller, in)
	if err != nil {
		return ProjectSetViewOutput{}, err
	}
	view := ProjectSetViewOutput{
		Status:           string(out.Status),
		RepoURLCanonical: out.RepoURLCanonical,
		ResolvedProject:  out.ResolvedProject,
		IntentBody:       out.Orientation.IntentBody,
		Principles:       out.Orientation.Principles,
	}
	if out.Status == ProjectSetStatusResolved {
		view.ResolvedView = c.BuildProjectView(out.ResolvedProject, baseURL)
	}
	return view, nil
}

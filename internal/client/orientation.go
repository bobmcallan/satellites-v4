// Project orientation bundle (sty_31d51494 layer 2 — relocated to the
// typed surface in sty_df1cb227 Slice C). The bundle is what an agent
// reads on its first turn after a user prompt fires — "where am I,
// what is this project, what guardrails apply" — in one roundtrip.
// project_set returns it after binding the session to a project;
// project_get returns it for refresh on subsequent turns without
// re-resolving the repo URL.
//
// Sources:
//   - IntentBody: a scope=project artifact named "project_intent"
//     under config/seed/<project_id>/artifacts/. Read with nil
//     memberships per the system-scope read pattern.
//   - Principles: every active type=principle document, both
//     scope=system (read with nil memberships) and scope=workspace +
//     scope=project scoped to the bound project_id. Bodies inline so
//     the agent has the prose without fanning out to per-principle
//     reads.

package client

import (
	"context"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/project"
)

// ProjectIntentArtifactName is the canonical name the loader writes
// the project-intent artifact under in the document store. Mirror of
// the file name on disk.
const ProjectIntentArtifactName = "project_intent"

// PrincipleEntry is one principle's projection in the orientation
// bundle. Bodies are inlined — the bundle is small enough that
// per-principle fanout is wasted roundtrips.
type PrincipleEntry struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Body  string `json:"body"`
}

// OrientationFields is the subset of the verb response that carries
// the orientation bundle. project_set merges this into its existing
// response shape; project_get returns it alongside the project view.
// Exported (was orientationFields) so the typed surface is consumable
// across the wire-layer + the CLI.
type OrientationFields struct {
	IntentBody string           `json:"intent_body,omitempty"`
	Principles []PrincipleEntry `json:"principles"`
}

// BuildOrientation reads the project intent artifact + every active
// principle (system + workspace + project scope) and returns them in
// the bundle shape. Errors fall through to empty fields so a missing
// intent or principle store doesn't break the bootstrap call.
//
// Sty_7f5585e9 introduced the workspace-tier pass — a workspace
// authors a principle once and every project in the workspace
// inherits it. Reads are additive across tiers; collisions on Name
// produce two entries and the agent reconciles.
func (c *Client) BuildOrientation(ctx context.Context, p project.Project) OrientationFields {
	out := OrientationFields{Principles: []PrincipleEntry{}}
	if c.deps.Documents == nil {
		return out
	}

	if intent, err := c.deps.Documents.GetByName(ctx, p.ID, ProjectIntentArtifactName, nil); err == nil &&
		intent.Type == document.TypeArtifact &&
		intent.Status == document.StatusActive {
		out.IntentBody = intent.Body
	}

	if rows, err := c.deps.Documents.List(ctx, document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
	}, nil); err == nil {
		for _, d := range rows {
			if d.Status != document.StatusActive {
				continue
			}
			out.Principles = append(out.Principles, PrincipleEntry{
				Name: d.Name, Scope: "system", Body: d.Body,
			})
		}
	}

	if p.WorkspaceID != "" {
		if rows, err := c.deps.Documents.List(ctx, document.ListOptions{
			Type:  document.TypePrinciple,
			Scope: document.ScopeWorkspace,
		}, nil); err == nil {
			for _, d := range rows {
				if d.Status != document.StatusActive || d.WorkspaceID != p.WorkspaceID {
					continue
				}
				out.Principles = append(out.Principles, PrincipleEntry{
					Name: d.Name, Scope: "workspace", Body: d.Body,
				})
			}
		}
	}

	if rows, err := c.deps.Documents.List(ctx, document.ListOptions{
		Type:      document.TypePrinciple,
		Scope:     document.ScopeProject,
		ProjectID: p.ID,
	}, nil); err == nil {
		for _, d := range rows {
			if d.Status != document.StatusActive {
				continue
			}
			out.Principles = append(out.Principles, PrincipleEntry{
				Name: d.Name, Scope: "project", Body: d.Body,
			})
		}
	}

	return out
}

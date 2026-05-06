// Project orientation bundle (sty_31d51494 layer 2). The bundle is
// what an agent reads on its first turn after a user prompt fires —
// "where am I, what is this project, what guardrails apply" — in one
// roundtrip. project_set returns it after binding the session to a
// project; project_context returns it for refresh on subsequent turns
// without re-resolving the repo URL.
//
// Sources:
//   - intent_body: a scope=project artifact named "project_intent"
//     under config/seed/<project_id>/artifacts/. Read with nil
//     memberships per the system-scope read pattern (the artifact is
//     project-scoped but every agent in the project should see the
//     same prose; workspace gating doesn't apply at the read).
//   - principles: every active type=principle document, both
//     scope=system (read with nil memberships) and scope=project
//     scoped to the bound project_id (also nil memberships — same
//     reason). Bodies inline so the agent has the prose without
//     fanning out to per-principle reads.
package mcpserver

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

// orientationFields is the subset of the verb response that carries
// the orientation bundle. project_set merges this into its existing
// response shape; project_context returns it alongside project_id.
type orientationFields struct {
	IntentBody string           `json:"intent_body,omitempty"`
	Principles []PrincipleEntry `json:"principles"`
}

// buildOrientation reads the project intent artifact + every active
// principle (system + project scope) and returns them in the bundle
// shape. Errors fall through to empty fields so a missing intent or
// principle store doesn't break the bootstrap call.
func (s *Server) buildOrientation(ctx context.Context, p project.Project) orientationFields {
	out := orientationFields{Principles: []PrincipleEntry{}}
	if s.docs == nil {
		return out
	}

	if intent, err := s.docs.GetByName(ctx, p.ID, ProjectIntentArtifactName, nil); err == nil &&
		intent.Type == document.TypeArtifact &&
		intent.Status == document.StatusActive {
		out.IntentBody = intent.Body
	}

	if rows, err := s.docs.List(ctx, document.ListOptions{
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

	if rows, err := s.docs.List(ctx, document.ListOptions{
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

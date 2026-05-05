// Package agentprocess holds the agent-process artifact resolver. The
// artifact is a `document{type=artifact}` tagged `kind:agent-process`
// carrying the markdown the satellites MCP server surfaces as its
// handshake instructions block.
//
// Resolution chain:
//
//  1. project-scope `name=agent_process` artifact (per-project override)
//  2. system-scope `name=default_agent_process` artifact (seeded by configseed)
//  3. empty
//
// The body is authoritative prose, not code. The resolver shuttles
// whatever markdown the seed/override carries; constraints on its
// content live in the markdown itself, not here.
package agentprocess

import (
	"context"

	"github.com/bobmcallan/satellites/internal/document"
)

// SystemDefaultName is the name under which the system-scope default
// artifact is seeded.
const SystemDefaultName = "default_agent_process"

// ProjectOverrideName is the name a project's per-project override
// artifact must use to be picked up by the resolver.
const ProjectOverrideName = "agent_process"

// KindTag is the canonical tag every agent-process artifact carries.
// The `document_list(type=artifact, tags=[kind:agent-process])` query
// uses this tag to enumerate every override across a workspace.
const KindTag = "kind:agent-process"

// ResolverDocs is the read-only subset of document.Store the resolver
// needs. Defined here (rather than imported wholesale) so tests can
// inject a stub without standing up a full document store.
type ResolverDocs interface {
	GetByName(ctx context.Context, projectID, name string, memberships []string) (document.Document, error)
}

// Resolve returns the resolved agent-process body for projectID. Walks
// the project-scope override → system-scope default → empty chain.
// memberships scopes the lookup the same way every other document read
// does (nil = no scoping; empty = deny-all; non-empty = workspace_id IN
// memberships). Errors from the document store fall through to the
// next tier — the resolver never propagates a transient lookup failure.
func Resolve(ctx context.Context, docs ResolverDocs, projectID string, memberships []string) string {
	if docs == nil {
		return ""
	}
	if projectID != "" {
		if d, err := docs.GetByName(ctx, projectID, ProjectOverrideName, memberships); err == nil && isAgentProcess(d) {
			return d.Body
		}
	}
	if d, err := docs.GetByName(ctx, "", SystemDefaultName, memberships); err == nil && isAgentProcess(d) {
		return d.Body
	}
	return ""
}

// isAgentProcess validates that a fetched document is the right shape:
// type=artifact, status=active, carrying the kind:agent-process tag.
// A doc with the right name but the wrong shape is treated as missing —
// the resolver falls through to the next tier rather than serving
// confusing content.
func isAgentProcess(d document.Document) bool {
	if d.Type != document.TypeArtifact || d.Status != document.StatusActive {
		return false
	}
	for _, t := range d.Tags {
		if t == KindTag {
			return true
		}
	}
	return false
}

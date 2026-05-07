// Scope-aware read helpers (sty_6ee30308). scope=system rows are the
// substrate's public configuration tier — git-tracked under
// config/seed/system/, no tenant data. They must be visible to every
// authenticated caller so dispatched agents can read their own agent
// doc, contracts, and principles via MCP (per
// pr_substrate_provides_context). scope=workspace and scope=project
// rows carry tenant data and stay membership-scoped.
//
// The pattern mirrors internal/configseed/sweep.go:57-60 — pass nil
// memberships for the system tier, the caller's memberships otherwise.
package mcpserver

import (
	"context"
	"sort"

	"github.com/bobmcallan/satellites/internal/document"
)

// listScoped is the typed `_list` MCP wrappers' entry into the
// hierarchical resolver. Story_08196787 promoted the manual tier-routing
// that lived inline here into Store.ResolveList so list-shape reads now
// walk the same project → workspace → system ladder ResolveByName uses
// for name-shape reads. Scope filtering, membership scoping, and the
// system-tier-workspace-blind invariant (sty_6ee30308) are enforced
// inside the resolver; this helper is a thin pass-through.
func listScoped(ctx context.Context, store document.Store, opts document.ListOptions, workspaceID string, memberships []string) ([]document.Document, error) {
	return store.ResolveList(ctx, opts, workspaceID, memberships)
}

// searchScoped is listScoped's analogue for the structured-filter
// Search path. The query-side substring match runs inside the store,
// so the helper just routes per-scope and unions in the empty-scope
// case.
func searchScoped(ctx context.Context, store document.Store, opts document.SearchOptions, memberships []string) ([]document.Document, error) {
	switch opts.Scope {
	case document.ScopeSystem:
		return store.Search(ctx, opts, nil)
	case "":
		// fall through
	default:
		return store.Search(ctx, opts, memberships)
	}

	sysOpts := opts
	sysOpts.Scope = document.ScopeSystem
	sysRows, err := store.Search(ctx, sysOpts, nil)
	if err != nil {
		return nil, err
	}

	memRows, err := store.Search(ctx, opts, memberships)
	if err != nil {
		return nil, err
	}

	return mergeUnionByID(sysRows, memRows, 0), nil
}

// searchSemanticScoped is the SearchSemantic analogue. Two pre-filter
// passes (one workspace-blind for system, one membership-scoped for
// the rest) feed the union; per-parent best-score ordering is
// preserved by re-sorting the merged set on BestChunkScore.
func searchSemanticScoped(ctx context.Context, store document.Store, query string, opts document.SearchOptions, memberships []string) ([]document.Document, error) {
	switch opts.Scope {
	case document.ScopeSystem:
		return store.SearchSemantic(ctx, query, opts, nil)
	case "":
		// fall through
	default:
		return store.SearchSemantic(ctx, query, opts, memberships)
	}

	sysOpts := opts
	sysOpts.Scope = document.ScopeSystem
	sysRows, err := store.SearchSemantic(ctx, query, sysOpts, nil)
	if err != nil {
		return nil, err
	}

	memRows, err := store.SearchSemantic(ctx, query, opts, memberships)
	if err != nil {
		return nil, err
	}

	merged := mergeUnionByID(sysRows, memRows, 0)
	sort.SliceStable(merged, func(i, j int) bool {
		var si, sj float32
		if merged[i].BestChunkScore != nil {
			si = *merged[i].BestChunkScore
		}
		if merged[j].BestChunkScore != nil {
			sj = *merged[j].BestChunkScore
		}
		return si > sj
	})
	return merged, nil
}

// getByIDScoped reads a row workspace-blind and re-applies the
// membership predicate only when the row's scope is not system. This
// keeps tenant-scoped rows isolated while letting any authenticated
// caller resolve a system-tier doc by id.
func getByIDScoped(ctx context.Context, store document.Store, id string, memberships []string) (document.Document, error) {
	doc, err := store.GetByID(ctx, id, nil)
	if err != nil {
		return document.Document{}, err
	}
	if doc.Scope == document.ScopeSystem {
		return doc, nil
	}
	if !workspaceInMemberships(doc.WorkspaceID, memberships) {
		return document.Document{}, document.ErrNotFound
	}
	return doc, nil
}

// mergeUnionByID concatenates two row slices into a single slice with
// no duplicate ids. The first slice's ordering is preserved; rows from
// the second slice that share an id are dropped. When limit > 0 the
// merged slice is truncated to limit rows.
func mergeUnionByID(first, second []document.Document, limit int) []document.Document {
	out := make([]document.Document, 0, len(first)+len(second))
	seen := make(map[string]struct{}, len(first)+len(second))
	for _, r := range first {
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		out = append(out, r)
	}
	for _, r := range second {
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// workspaceInMemberships reports whether wsID is in memberships. Nil
// memberships means "no scoping" (mirrors document.inDocMemberships)
// and grants visibility; an empty (non-nil) slice denies.
func workspaceInMemberships(wsID string, memberships []string) bool {
	if memberships == nil {
		return true
	}
	for _, m := range memberships {
		if m == wsID {
			return true
		}
	}
	return false
}

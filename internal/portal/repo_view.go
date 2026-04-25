// Repo view composite for slice 11.5 (story_d4685302). Builds the
// `/repo` page state per docs/ui-design.md §2.6 — repo header + symbol
// search input + recent commits panel + branch diff tool.
package portal

import (
	"context"
	"time"

	"github.com/bobmcallan/satellites/internal/repo"
)

// repoComposite is the view-model for the /repo landing page.
type repoComposite struct {
	Repo    repoCard     `json:"repo"`
	Commits []commitCard `json:"commits"`
	Empty   bool         `json:"empty"`
	IsAdmin bool         `json:"is_admin"`
}

// repoCard is the header row.
type repoCard struct {
	ID            string `json:"id"`
	GitRemote     string `json:"git_remote"`
	DefaultBranch string `json:"default_branch"`
	HeadSHA       string `json:"head_sha,omitempty"`
	LastIndexedAt string `json:"last_indexed_at,omitempty"`
	IndexVersion  int    `json:"index_version"`
	SymbolCount   int    `json:"symbol_count"`
	FileCount     int    `json:"file_count"`
	Status        string `json:"status"`
}

// buildRepoComposite assembles the view-model for projectID's repo.
// When no repo is registered, returns an empty composite with Empty=true
// so the SSR template renders the empty-state copy. isAdmin gates the
// reindex affordance — non-admins see the panel but no button.
func buildRepoComposite(ctx context.Context, store repo.Store, projectID string, memberships []string, isAdmin bool) repoComposite {
	if store == nil || projectID == "" {
		return repoComposite{Empty: true, IsAdmin: isAdmin}
	}
	rows, err := store.List(ctx, projectID, memberships)
	if err != nil || len(rows) == 0 {
		return repoComposite{Empty: true, IsAdmin: isAdmin}
	}
	r := rows[0]
	card := repoCard{
		ID:            r.ID,
		GitRemote:     r.GitRemote,
		DefaultBranch: r.DefaultBranch,
		HeadSHA:       r.HeadSHA,
		IndexVersion:  r.IndexVersion,
		SymbolCount:   r.SymbolCount,
		FileCount:     r.FileCount,
		Status:        r.Status,
	}
	if !r.LastIndexedAt.IsZero() {
		card.LastIndexedAt = r.LastIndexedAt.UTC().Format(time.RFC3339)
	}
	out := repoComposite{
		Repo:    card,
		Commits: commitCardsFor(ctx, store, r.ID, memberships),
		IsAdmin: isAdmin,
	}
	return out
}

// commitCardsFor reads ListCommits and projects rows into the
// commitCard view-model already used by the story-view repo-provenance
// panel. Returns an empty slice on error or when no commits are
// persisted.
func commitCardsFor(ctx context.Context, store repo.Store, repoID string, memberships []string) []commitCard {
	rows, err := store.ListCommits(ctx, repoID, "", 50, memberships)
	if err != nil || len(rows) == 0 {
		return []commitCard{}
	}
	out := make([]commitCard, 0, len(rows))
	for _, c := range rows {
		out = append(out, commitCard{
			SHA:       c.SHA,
			Subject:   c.Subject,
			Author:    c.Author,
			URL:       c.URL,
			CreatedAt: c.CommittedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

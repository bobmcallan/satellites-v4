package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/project"
)

// MigrateRemotesCanonical is the sty_14dfd05b boot-time migration. Three
// passes, all idempotent:
//
//  1. Re-canonicalise every existing `repos` row whose stored
//     `git_remote` differs from the canonical form. Required because
//     repo_add only canonicalises on write going forward — pre-existing
//     rows still carry whatever shape the caller originally passed.
//
//  2. For every `projects` row that still carries the legacy
//     `git_remote` column with no corresponding `repos` row, create
//     the `repos` row using the canonical form. This is the
//     compatibility bridge so projects whose remote was only ever
//     stored on the legacy column remain reachable via project_set
//     after the column drops.
//
//  3. UNSET `git_remote` on every `projects` row. SurrealDB schemaless
//     tables tolerate orphan fields but the value is irrelevant once
//     the Go struct + lookup path no longer references it; clearing it
//     keeps the table shape honest.
//
// Returns counts (recanonicalised repos, backfilled projects) for the
// boot log. Errors are warned individually; the function does not abort
// — a malformed remote on one row should not stop boot.
func MigrateRemotesCanonical(ctx context.Context, db *surrealdb.DB, repos Store, logger arbor.ILogger, now time.Time) (recanonicalised, backfilled int, err error) {
	if db == nil || repos == nil {
		return 0, 0, errors.New("repo: MigrateRemotesCanonical requires db + store")
	}

	// Pass 1 — recanonicalise existing repos rows in place.
	type repoRow struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		GitRemote   string `json:"git_remote"`
	}
	repoSQL := "SELECT meta::id(id) AS id, workspace_id, git_remote FROM repos"
	repoResults, qErr := surrealdb.Query[[]repoRow](ctx, db, repoSQL, nil)
	if qErr != nil {
		return 0, 0, fmt.Errorf("repo: select repos: %w", qErr)
	}
	if repoResults != nil && len(*repoResults) > 0 {
		for _, r := range (*repoResults)[0].Result {
			canonical, cErr := project.CanonicaliseGitRemote(r.GitRemote)
			if cErr != nil || canonical == "" {
				logger.Warn().
					Str("repo_id", r.ID).
					Str("git_remote", r.GitRemote).
					Msg("repo migrate: skipping unparseable git_remote")
				continue
			}
			if canonical == r.GitRemote {
				continue
			}
			updateSQL := "UPDATE $rid SET git_remote = $rem, updated_at = $now"
			vars := map[string]any{
				"rid": surrealmodels.NewRecordID("repos", r.ID),
				"rem": canonical,
				"now": now,
			}
			if _, uErr := surrealdb.Query[any](ctx, db, updateSQL, vars); uErr != nil {
				logger.Warn().Str("repo_id", r.ID).Str("error", uErr.Error()).Msg("repo migrate: recanonicalise update failed")
				continue
			}
			recanonicalised++
		}
	}

	// Pass 2 — backfill repos rows from any project still carrying the
	// legacy git_remote column. Only acts when the project has no repo
	// row at all (the one-per-project invariant means we never collide
	// with an existing row).
	type legacyProjectRow struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		GitRemote   string `json:"git_remote"`
	}
	projSQL := "SELECT meta::id(id) AS id, workspace_id, git_remote FROM projects WHERE git_remote IS NOT NONE AND git_remote != ''"
	projResults, qErr := surrealdb.Query[[]legacyProjectRow](ctx, db, projSQL, nil)
	if qErr != nil {
		return recanonicalised, 0, fmt.Errorf("repo: select legacy project remotes: %w", qErr)
	}
	if projResults != nil && len(*projResults) > 0 {
		for _, p := range (*projResults)[0].Result {
			canonical, cErr := project.CanonicaliseGitRemote(p.GitRemote)
			if cErr != nil || canonical == "" {
				logger.Warn().
					Str("project_id", p.ID).
					Str("git_remote", p.GitRemote).
					Msg("repo migrate: project legacy remote unparseable; skipping backfill")
				continue
			}
			existing, listErr := repos.List(ctx, p.ID, nil)
			if listErr != nil {
				logger.Warn().Str("project_id", p.ID).Str("error", listErr.Error()).Msg("repo migrate: list existing repos failed")
				continue
			}
			if len(existing) > 0 {
				continue // project already has a repo row; legacy column data is orphan
			}
			if _, cErr := repos.Create(ctx, Repo{
				WorkspaceID:   p.WorkspaceID,
				ProjectID:     p.ID,
				GitRemote:     canonical,
				DefaultBranch: "main",
				Status:        StatusActive,
			}, now); cErr != nil {
				logger.Warn().Str("project_id", p.ID).Str("error", cErr.Error()).Msg("repo migrate: backfill repo create failed")
				continue
			}
			backfilled++
		}
	}

	// Pass 3 — UNSET the legacy column on every projects row. Idempotent
	// — re-running on already-cleared rows is a no-op at the field
	// level. SurrealDB schemaless tolerates orphan fields, but clearing
	// keeps the table representation honest with the Go struct.
	if _, uErr := surrealdb.Query[any](ctx, db, "UPDATE projects UNSET git_remote", nil); uErr != nil {
		logger.Warn().Str("error", uErr.Error()).Msg("repo migrate: UNSET projects.git_remote failed")
	}

	return recanonicalised, backfilled, nil
}

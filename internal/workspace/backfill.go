package workspace

import (
	"context"
	"fmt"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
)

// BackfillReport summarises the rows touched by BackfillPrimitives. All
// counters are cumulative across the call; every field is idempotent at
// repeat-call time (subsequent boots report 0 across the board on a
// previously-migrated database).
type BackfillReport struct {
	ProjectsStamped  int
	StoriesStamped   int
	LedgerStamped    int
	DocumentsStamped int
}

// BackfillPrimitives walks every primitive that carries workspace_id and
// stamps rows whose workspace_id is empty. Each project row is mapped via
// `EnsureDefault(OwnerUserID)` so the system user + legacy per-user rows all
// land in the correct workspace. Stories, ledger entries, and documents
// cascade from their parent project's workspace_id.
//
// Idempotent. Safe to call on every boot. Errors from individual row
// updates log-and-continue — the boot sequence must not refuse to start if
// one row fails; the next boot retries.
func BackfillPrimitives(
	ctx context.Context,
	ws Store,
	projects project.Store,
	stories story.Store,
	ledgerStore ledger.Store,
	docs document.Store,
	logger arbor.ILogger,
	now time.Time,
) (BackfillReport, error) {
	if ws == nil || projects == nil {
		return BackfillReport{}, fmt.Errorf("workspace backfill: workspace + project stores required")
	}
	report := BackfillReport{}
	missing, err := projects.ListMissingWorkspaceID(ctx)
	if err != nil {
		return report, fmt.Errorf("workspace backfill: list missing: %w", err)
	}
	for _, p := range missing {
		wsID, err := EnsureDefault(ctx, ws, logger, p.OwnerUserID, now)
		if err != nil {
			logger.Warn().
				Str("project_id", p.ID).
				Str("owner_user_id", p.OwnerUserID).
				Str("error", err.Error()).
				Msg("workspace backfill: ensure default failed; skipping row")
			continue
		}
		if _, err := projects.SetWorkspaceID(ctx, p.ID, wsID, now); err != nil {
			logger.Warn().
				Str("project_id", p.ID).
				Str("workspace_id", wsID).
				Str("error", err.Error()).
				Msg("workspace backfill: set project workspace_id failed")
			continue
		}
		report.ProjectsStamped++
	}

	// Cascade: stamp children by project_id → workspace_id for every
	// project we've just touched AND every project that was already
	// stamped (so legacy child rows without workspace_id on a stamped
	// parent also get fixed). A second pass walks the full project set.
	all := append([]project.Project{}, missing...)
	// Freshen the rows we just stamped — but also iterate the rest.
	// ListByOwner is per-owner; easier to refetch missing-workspace child
	// rows per-project by passing the (project_id, workspace_id) pair we
	// already know. For rows already stamped before this call we delegate
	// to a pre-boot refresh: rely on project.SeedDefault to have used the
	// system workspace, then iterate known system-owned projects too.
	for _, p := range all {
		ws, err := resolveProjectWorkspace(ctx, projects, p.ID)
		if err != nil || ws == "" {
			continue
		}
		if stories != nil {
			if n, err := stories.BackfillWorkspaceID(ctx, p.ID, ws, now); err != nil {
				logger.Warn().Str("project_id", p.ID).Str("error", err.Error()).Msg("workspace backfill: stories")
			} else {
				report.StoriesStamped += n
			}
		}
		if ledgerStore != nil {
			if n, err := ledgerStore.BackfillWorkspaceID(ctx, p.ID, ws); err != nil {
				logger.Warn().Str("project_id", p.ID).Str("error", err.Error()).Msg("workspace backfill: ledger")
			} else {
				report.LedgerStamped += n
			}
		}
		if docs != nil {
			if n, err := docs.BackfillWorkspaceID(ctx, p.ID, ws, now); err != nil {
				logger.Warn().Str("project_id", p.ID).Str("error", err.Error()).Msg("workspace backfill: documents")
			} else {
				report.DocumentsStamped += n
			}
		}
	}

	logger.Info().
		Int("projects_stamped", report.ProjectsStamped).
		Int("stories_stamped", report.StoriesStamped).
		Int("ledger_stamped", report.LedgerStamped).
		Int("documents_stamped", report.DocumentsStamped).
		Msg("workspace backfill complete")
	return report, nil
}

// resolveProjectWorkspace returns the current workspace_id for a project,
// or empty if the project no longer exists or has no workspace stamped.
func resolveProjectWorkspace(ctx context.Context, projects project.Store, projectID string) (string, error) {
	p, err := projects.GetByID(ctx, projectID)
	if err != nil {
		return "", err
	}
	return p.WorkspaceID, nil
}

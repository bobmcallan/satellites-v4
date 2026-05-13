package project

import (
	"context"
	"time"

	"github.com/ternarybob/arbor"
)

// DefaultOwnerUserID is the owner stamp on the system-seeded default
// project. It is a synthetic literal, not a real user id; stories
// intentionally distinct from it own their own projects.
const DefaultOwnerUserID = "system"

// DefaultProjectName is the display name of the seeded default project.
const DefaultProjectName = "Satellites v4"

// SeedDefault returns the id of the default project, creating it if it
// doesn't already exist. Idempotent: running twice returns the same id.
// The default project is owned by DefaultOwnerUserID ("system") and
// scoped to workspaceID when supplied. Backstops document_ingest_file /
// document_get callers that don't supply their own project_id.
func SeedDefault(ctx context.Context, store Store, logger arbor.ILogger, workspaceID string) (string, error) {
	existing, err := store.ListByOwner(ctx, DefaultOwnerUserID, nil)
	if err != nil {
		return "", err
	}
	for _, p := range existing {
		if p.Name == DefaultProjectName {
			logger.Info().Str("project_id", p.ID).Msg("default project already seeded")
			return p.ID, nil
		}
	}
	p, err := store.Create(ctx, DefaultOwnerUserID, workspaceID, DefaultProjectName, time.Now().UTC())
	if err != nil {
		return "", err
	}
	logger.Info().Str("project_id", p.ID).Str("workspace_id", workspaceID).Msg("default project seeded")
	return p.ID, nil
}

// PerUserDefaultName is the display name of the legacy per-user default
// project. Retained as a literal for the ArchiveLegacyDefaults migration
// — new users no longer get a Default project on login, and existing
// rows with this name are archived as part of the project-schema
// rollout (sty_c975ebeb). A canonical project is now keyed on its
// git_remote and must be created explicitly via project_add.
const PerUserDefaultName = "Default"

// ArchiveLegacyDefaults flips every active per-user Default project
// (Name == PerUserDefaultName, OwnerUserID != DefaultOwnerUserID,
// Status == StatusActive) to StatusArchived. Idempotent: a second
// invocation finds no active rows. Returns the count of rows touched.
//
// Run once at boot during the schema rollout. Existing data on the
// archived rows persists; only listings filter them out via
// status-active scoping.
func ArchiveLegacyDefaults(ctx context.Context, store Store, logger arbor.ILogger, now time.Time) (int, error) {
	var n int
	// nil memberships → see every row regardless of workspace.
	owners := map[string]struct{}{}
	// We don't have a ListAll, so walk owners we know about indirectly via
	// ListMissingWorkspaceID won't help. Instead, scan via a per-owner
	// pass triggered from the SurrealDB layer when present.
	type ownerLister interface {
		ListAll(ctx context.Context) ([]Project, error)
	}
	if al, ok := store.(ownerLister); ok {
		all, err := al.ListAll(ctx)
		if err != nil {
			return 0, err
		}
		for _, p := range all {
			if p.Name == PerUserDefaultName && p.OwnerUserID != DefaultOwnerUserID && p.Status == StatusActive {
				owners[p.ID] = struct{}{}
			}
		}
	}
	for id := range owners {
		if _, err := store.SetStatus(ctx, id, StatusArchived, now); err != nil {
			logger.Warn().Str("project_id", id).Str("error", err.Error()).Msg("archive legacy default failed")
			continue
		}
		n++
	}
	if n > 0 {
		logger.Info().Int("archived", n).Msg("legacy per-user Default projects archived")
	}
	return n, nil
}

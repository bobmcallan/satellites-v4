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

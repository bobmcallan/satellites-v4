package document

import (
	"context"
	"time"

	"github.com/ternarybob/arbor"
)

// DefaultSeedFiles is the boot-seed filelist for the walking skeleton —
// the three v4 docs that currently exist in /app/docs.
var DefaultSeedFiles = []string{
	"architecture.md",
	"ui-design.md",
	"development.md",
}

// SeedIfEmpty ingests DefaultSeedFiles when store.Count returns zero.
// Returns the number of ingestions performed (0 if skipped).
func SeedIfEmpty(ctx context.Context, store Store, logger arbor.ILogger, docsDir string) (int, error) {
	return Seed(ctx, store, logger, docsDir, DefaultSeedFiles)
}

// Seed runs IngestFile over files. Skips work when store.Count > 0 — seeds
// are idempotent at the collection level; individual file changes should be
// driven through explicit ingest calls.
func Seed(ctx context.Context, store Store, logger arbor.ILogger, docsDir string, files []string) (int, error) {
	n, err := store.Count(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		logger.Info().Int("existing", n).Msg("document seed skipped (store not empty)")
		return 0, nil
	}
	now := time.Now().UTC()
	ingested := 0
	for _, f := range files {
		if _, err := IngestFile(ctx, store, logger, docsDir, f, now); err != nil {
			logger.Warn().Str("filename", f).Str("error", err.Error()).Msg("document seed skip")
			continue
		}
		ingested++
	}
	logger.Info().Int("count", ingested).Msg("document seed done")
	return ingested, nil
}

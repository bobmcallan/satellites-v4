// System-tier orphan sweep (sty_8f6b90c8). The seed loader's normal
// upsert path creates and updates rows but does not delete them when a
// seed file disappears. project_set's orientation bundle reads
// scope=system principles across every workspace (nil memberships) so
// orphaned rows continue to bleed into every project's principles
// list. This pass closes that gap for type=principle by archiving
// every active scope=system principle row whose Name does not match a
// .md file currently present under <seedDir>/system/principles/.
//
// Limited to type=principle for now — that's the kind sty_8f6b90c8
// migrated. If a future story moves agents/contracts/workflows out of
// system tier, extend the loop or generalise the helper.
package configseed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/document"
)

// SweepOrphanedSystemPrinciples archives every active scope=system
// type=principle document whose Name does not appear in the on-disk
// seed set under <seedDir>/system/principles/*.md.
//
// Reads the principle files directly (Parse + frontmatter `name`) so
// the comparison is against what will actually be seeded on the next
// boot, not a hardcoded list. Workspace-blind — passes nil memberships
// to docs.List and docs.Delete so rows in workspaces the boot user is
// not a member of are still reachable.
//
// Idempotent: a second invocation on a clean DB lists only active rows
// matching active seed files, finds no orphans, archives nothing.
//
// Returns the number of rows archived. Per-row Delete failures are
// logged via the supplied logger and do not abort the sweep — partial
// progress is preferred to a hard stop.
func SweepOrphanedSystemPrinciples(ctx context.Context, docs document.Store, seedDir string, logger arbor.ILogger, now time.Time) (archived int, err error) {
	if docs == nil {
		return 0, fmt.Errorf("configseed: doc store is nil")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}

	expected, err := readSystemPrincipleNames(seedDir)
	if err != nil {
		return 0, err
	}

	rows, err := docs.List(ctx, document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
	}, nil)
	if err != nil {
		return 0, fmt.Errorf("sweep: list system principles: %w", err)
	}

	for _, row := range rows {
		if row.Status != document.StatusActive {
			continue
		}
		if expected[row.Name] {
			continue
		}
		if derr := docs.Delete(ctx, row.ID, document.DeleteArchive, nil); derr != nil {
			logger.Warn().
				Str("doc_id", row.ID).
				Str("name", row.Name).
				Str("workspace_id", row.WorkspaceID).
				Str("error", derr.Error()).
				Msg("sweep system principle archive failed")
			continue
		}
		archived++
		logger.Info().
			Str("doc_id", row.ID).
			Str("name", row.Name).
			Str("workspace_id", row.WorkspaceID).
			Msg("archived orphan system principle")
	}
	return archived, nil
}

// readSystemPrincipleNames returns the set of `name` frontmatter
// values for every .md file under <seedDir>/system/principles/. A
// missing directory returns an empty set with no error (the post-
// sty_8f6b90c8 expected state — every principle moved to project
// tier).
func readSystemPrincipleNames(seedDir string) (map[string]bool, error) {
	out := make(map[string]bool)
	dir := filepath.Join(seedDir, SystemSubdir, "principles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("sweep: read system principles dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		fm, _, perr := Parse(content)
		if perr != nil {
			continue
		}
		name := fm.String("name")
		if name != "" {
			out[name] = true
		}
	}
	return out, nil
}

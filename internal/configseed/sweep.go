// System-tier orphan sweep (sty_8f6b90c8 → sty_92271886). The seed
// loader's normal upsert path creates and updates rows but does not
// delete them when a seed file disappears. project_set's orientation
// bundle reads scope=system rows across every workspace (nil
// memberships) so orphaned rows continue to bleed into every project's
// view. This pass closes that gap by archiving every active scope=system
// row whose Name does not match a .md file currently present under
// <seedDir>/system/<kind>/.
//
// Sty_92271886 generalised the original kind=principle helper into
// SweepOrphanedSystemDocs, which iterates every kind the system tier
// loads. SweepOrphanedSystemPrinciples is preserved as a thin alias
// so legacy callers (boot path's earlier wiring) keep working until
// migrated; new wiring should call SweepOrphanedSystemDocs.
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

// systemSweepKinds is the ordered list of kinds the generalised sweep
// inspects. Mirrors configseed.Run's main-loop kinds (agent, contract,
// workflow, story_template, replicate_vocabulary, artifact) plus
// principle (which Run loads in its dedicated phase). help is excluded
// — help docs live under config/help/ rather than config/seed/system/,
// and they're managed via a separate seed-write path.
//
// Each kind maps to {document.Type, kindSubdir} via documentTypeForKind
// + kindSubdir.
var systemSweepKinds = []Kind{
	KindAgent,
	KindContract,
	KindWorkflow,
	KindStoryTemplate,
	KindReplicateVocabulary,
	KindArtifact,
	KindPrinciple,
}

// SweepOrphanedSystemDocs archives every active scope=system document
// whose Name does not appear in the on-disk seed set under
// <seedDir>/system/<kind_subdir>/*.md, for every kind in
// systemSweepKinds. Workspace-blind — passes nil memberships to
// docs.List + docs.Delete so rows in workspaces the boot user is not a
// member of are still reachable.
//
// Idempotent: a second invocation on a clean DB lists only active rows
// matching active seed files and archives nothing.
//
// Returns the per-kind archived count summed across kinds. Per-row
// Delete failures are logged via the supplied logger and do not abort
// the sweep — partial progress is preferred to a hard stop.
//
// Sty_92271886.
func SweepOrphanedSystemDocs(ctx context.Context, docs document.Store, seedDir string, logger arbor.ILogger, now time.Time) (archived int, err error) {
	if docs == nil {
		return 0, fmt.Errorf("configseed: doc store is nil")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}

	for _, kind := range systemSweepKinds {
		docType, ok := documentTypeForKind(kind)
		if !ok {
			continue
		}
		subdir := kindSubdir(kind)
		if subdir == "" {
			continue
		}
		expected, rerr := readSystemSeedNames(seedDir, subdir)
		if rerr != nil {
			return archived, rerr
		}

		rows, lerr := docs.List(ctx, document.ListOptions{
			Type:  docType,
			Scope: document.ScopeSystem,
		}, nil)
		if lerr != nil {
			return archived, fmt.Errorf("sweep: list system %s: %w", docType, lerr)
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
					Str("type", row.Type).
					Str("name", row.Name).
					Str("workspace_id", row.WorkspaceID).
					Str("error", derr.Error()).
					Msg("sweep system doc archive failed")
				continue
			}
			archived++
			logger.Info().
				Str("doc_id", row.ID).
				Str("type", row.Type).
				Str("name", row.Name).
				Str("workspace_id", row.WorkspaceID).
				Msg("archived orphan system doc")
		}
	}
	return archived, nil
}

// SweepOrphanedProjectDocs archives every active scope=project document
// under (workspaceID, projectID) whose Name does not appear in the
// on-disk seed set under <seedDir>/<workspaceID>/<projectID>/<kind_subdir>/*.md,
// for every kind in projectKinds. Mirrors SweepOrphanedSystemDocs at
// the project tier — the system tier sweep walks system/<kind>/, this
// one walks <ws>/<proj>/<kind>/.
//
// Runs workspace-blind (nil memberships) so a boot user without
// membership in the target workspace can still reconcile orphan rows;
// the project tier is non-tenant from the seed loader's POV (the loader
// always runs as `system`).
//
// Idempotent: a second invocation on a clean DB lists only active rows
// matching active seed files and archives nothing.
//
// Returns the per-kind archived count summed across kinds. Per-row
// Delete failures are logged via the supplied logger and do not abort
// the sweep.
//
// Sty_94c54229 — added so a project-tier rename of frontmatter `name:`
// reconciles the live DB the same way a system-tier rename does.
func SweepOrphanedProjectDocs(ctx context.Context, docs document.Store, seedDir, workspaceID, projectID string, logger arbor.ILogger, now time.Time) (archived int, err error) {
	if docs == nil {
		return 0, fmt.Errorf("configseed: doc store is nil")
	}
	if workspaceID == "" {
		return 0, fmt.Errorf("configseed: workspace_id required")
	}
	if projectID == "" {
		return 0, fmt.Errorf("configseed: project_id required")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}

	for _, kind := range projectKinds {
		docType, ok := documentTypeForKind(kind)
		if !ok {
			continue
		}
		subdir := kindSubdir(kind)
		if subdir == "" {
			continue
		}
		expected, rerr := readProjectSeedNames(seedDir, workspaceID, projectID, subdir)
		if rerr != nil {
			return archived, rerr
		}

		rows, lerr := docs.List(ctx, document.ListOptions{
			Type:      docType,
			Scope:     document.ScopeProject,
			ProjectID: projectID,
		}, nil)
		if lerr != nil {
			return archived, fmt.Errorf("sweep: list project %s: %w", docType, lerr)
		}

		for _, row := range rows {
			if row.Status != document.StatusActive {
				continue
			}
			if row.WorkspaceID != workspaceID {
				continue
			}
			if expected[row.Name] {
				continue
			}
			if derr := docs.Delete(ctx, row.ID, document.DeleteArchive, nil); derr != nil {
				logger.Warn().
					Str("doc_id", row.ID).
					Str("type", row.Type).
					Str("name", row.Name).
					Str("workspace_id", row.WorkspaceID).
					Str("project_id", projectID).
					Str("error", derr.Error()).
					Msg("sweep project doc archive failed")
				continue
			}
			archived++
			logger.Info().
				Str("doc_id", row.ID).
				Str("type", row.Type).
				Str("name", row.Name).
				Str("workspace_id", row.WorkspaceID).
				Str("project_id", projectID).
				Msg("archived orphan project doc")
		}
	}
	return archived, nil
}

// SweepOrphanedSystemPrinciples is the original principle-only entry
// point, preserved as a thin alias around the generalised sweep so
// legacy callers (the in-tree boot wiring before sty_92271886) keep
// working until migrated. New code paths should call
// SweepOrphanedSystemDocs directly.
//
// Restricts the inspection to type=principle by listing only that kind;
// uses the generalised orphan-detection helper.
func SweepOrphanedSystemPrinciples(ctx context.Context, docs document.Store, seedDir string, logger arbor.ILogger, now time.Time) (archived int, err error) {
	if docs == nil {
		return 0, fmt.Errorf("configseed: doc store is nil")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}

	expected, err := readSystemSeedNames(seedDir, kindSubdir(KindPrinciple))
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

// documentTypeForKind maps a configseed Kind to its document.Type
// constant. Returns the type + true when the mapping is defined; any
// kind without a Type counterpart (none today) returns false.
func documentTypeForKind(kind Kind) (string, bool) {
	switch kind {
	case KindAgent:
		return document.TypeAgent, true
	case KindContract:
		return document.TypeContract, true
	case KindWorkflow:
		return document.TypeWorkflow, true
	case KindPrinciple:
		return document.TypePrinciple, true
	case KindStoryTemplate:
		return document.TypeStoryTemplate, true
	case KindReplicateVocabulary:
		return document.TypeReplicateVocabulary, true
	case KindArtifact:
		return document.TypeArtifact, true
	case KindSkill:
		return document.TypeSkill, true
	case KindReviewer:
		return document.TypeReviewer, true
	case KindHelp:
		return document.TypeHelp, true
	}
	return "", false
}

// readProjectSeedNames returns the set of canonical names for every
// .md file under <seedDir>/<workspaceID>/<projectID>/<subdir>/. Mirrors
// readSystemSeedNames but rooted at the project-tier path.
func readProjectSeedNames(seedDir, workspaceID, projectID, subdir string) (map[string]bool, error) {
	out := make(map[string]bool)
	dir := filepath.Join(seedDir, workspaceID, projectID, subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("sweep: read project %s dir: %w", subdir, err)
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
		if name == "" {
			name = fm.String("category")
		}
		if name == "" {
			name = fm.String("slug")
		}
		if name != "" {
			out[name] = true
		}
	}
	return out, nil
}

// readSystemSeedNames returns the set of `name` frontmatter values for
// every .md file under <seedDir>/system/<subdir>/. A missing directory
// returns an empty set with no error (the kind has no system-tier seed
// today).
//
// story_template files use `category` as their canonical id (see
// storyTemplateToInput); the parser falls back to `category` when
// `name` is absent so the orphan check matches the stored Name.
func readSystemSeedNames(seedDir, subdir string) (map[string]bool, error) {
	out := make(map[string]bool)
	dir := filepath.Join(seedDir, SystemSubdir, subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("sweep: read system %s dir: %w", subdir, err)
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
		if name == "" {
			// story_template / replicate_vocabulary may key on a
			// different frontmatter field. story_template falls back to
			// `category` (see storyTemplateToInput).
			name = fm.String("category")
		}
		if name == "" {
			// help docs use slug (config/help/, not under system/), but
			// guard the fallthrough anyway.
			name = fm.String("slug")
		}
		if name != "" {
			out[name] = true
		}
	}
	return out, nil
}

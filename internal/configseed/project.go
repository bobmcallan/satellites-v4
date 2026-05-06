// Project-scoped seed loader (sty_8868eaf4). Parallel to Run, but the
// produced documents are scope=project, project_id=<resolved>. Layout:
//
//	<seedDir>/<project_id>/<kind>/*.md
//
// Distinct from system kind subdirs (which sit at <seedDir>/<kind>/)
// because project_id has the unambiguous proj_ prefix. Discovery walks
// the seed dir and enumerates proj_* entries; the loader runs once per
// project at boot (as a goroutine, non-blocking) and on demand via the
// project_seed_run MCP verb.
//
// Strict isolation: this loader produces only scope=project rows. The
// document store's Validate path rejects scope=project documents that
// lack a project_id, and rejects scope=system documents that carry a
// project_id, so a misuse here surfaces as a write-time error rather
// than silent cross-tier contamination. configseed.Run produces only
// scope=system rows; the two paths never overlap.
package configseed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// ProjectDirPrefix is the canonical prefix every project-id directory
// carries under <seedDir>. Discovery uses it to distinguish project
// dirs from system kind subdirs.
const ProjectDirPrefix = "proj_"

// projectKinds is the ordered list of Kinds the project loader walks
// per project_id directory. Mirrors the system loader's list so the
// directory convention is symmetric: any kind that exists at system
// scope can also be seeded at project scope by dropping a file under
// <seedDir>/<project_id>/<kind>/. An empty subdir (or one missing on
// disk) loads zero rows for that kind — no error, no warning.
//
// Kinds that don't make sense at project scope (story_template,
// replicate_vocabulary as currently shipped) are still walked here,
// but the on-disk seed is expected to be empty. If a future story
// excludes a kind from the project tier, drop it from this list and
// document the choice on the change.
var projectKinds = []Kind{
	KindAgent,
	KindContract,
	KindWorkflow,
	KindStoryTemplate,
	KindReplicateVocabulary,
	KindArtifact,
	KindPrinciple,
}

// DiscoverProjectDirs returns the project_id values for every
// <seedDir>/proj_*/ entry found on disk, sorted. Returns nil + nil
// when the seed dir is missing (cold-boot test fixture). Other read
// errors come back as the second return; callers typically log and
// proceed.
func DiscoverProjectDirs(seedDir string) ([]string, error) {
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("configseed: read seed dir for project discovery: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ProjectDirPrefix) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// RunProject loads <seedDir>/<projectID>/<kind>/*.md and upserts each
// as a scope=project, project_id=projectID document. workspaceID is
// stamped on every produced row so workspace-scoping reads behave the
// same as for system rows. Idempotent on body hash via docs.Upsert.
//
// projectID resolution is the caller's responsibility — the loader
// trusts the supplied id. When invoked from the boot path, main.go
// looks up each discovered project_id against the project store and
// skips (with a structured warning) any directory whose id has no
// project row.
func RunProject(ctx context.Context, docs document.Store, seedDir, projectID, workspaceID, actor string, now time.Time) (Summary, error) {
	if docs == nil {
		return Summary{}, fmt.Errorf("configseed: doc store is nil")
	}
	if projectID == "" {
		return Summary{}, fmt.Errorf("configseed: project_id required")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}
	projectRoot := filepath.Join(seedDir, projectID)
	if _, err := os.Stat(projectRoot); err != nil {
		if os.IsNotExist(err) {
			return Summary{}, nil
		}
		return Summary{}, fmt.Errorf("configseed: stat project seed dir: %w", err)
	}

	pid := projectID
	summary := Summary{}
	for _, kind := range projectKinds {
		inputs, errs := LoadDir(projectRoot, kind, workspaceID, actor)
		summary.Errors = append(summary.Errors, prefixedErrors(errs, projectID)...)
		for _, in := range inputs {
			// Override scope + project_id. The per-kind parsers stamp
			// scope=system unconditionally; we re-target the input to
			// the project tier here so the same parsers + frontmatter
			// validation cover both surfaces.
			in.Scope = document.ScopeProject
			in.ProjectID = &pid
			summary.Loaded++
			res, err := docs.Upsert(ctx, in, now)
			if err != nil {
				summary.Errors = append(summary.Errors, ErrorEntry{
					Path:   projectID + "/" + string(kind) + "/" + in.Name,
					Reason: err.Error(),
				})
				continue
			}
			switch {
			case res.Created:
				summary.Created++
			case res.Changed:
				summary.Updated++
			default:
				summary.Skipped++
			}
		}
	}
	return summary, nil
}

// prefixedErrors prepends the project_id to each ErrorEntry's Path so
// boot logs identify which project's seed produced the failure.
func prefixedErrors(errs []ErrorEntry, projectID string) []ErrorEntry {
	if len(errs) == 0 {
		return nil
	}
	out := make([]ErrorEntry, len(errs))
	for i, e := range errs {
		out[i] = ErrorEntry{
			Path:   projectID + "/" + e.Path,
			Reason: e.Reason,
		}
	}
	return out
}
